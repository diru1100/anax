package agreement

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/boltdb/bolt"
	"github.com/golang/glog"
	"github.com/open-horizon/anax/abstractprotocol"
	"github.com/open-horizon/anax/config"
	"github.com/open-horizon/anax/device"
	"github.com/open-horizon/anax/events"
	"github.com/open-horizon/anax/exchange"
	"github.com/open-horizon/anax/persistence"
	"github.com/open-horizon/anax/policy"
	"github.com/open-horizon/anax/producer"
	"github.com/open-horizon/anax/worker"
	"net/http"
	"reflect"
	"runtime"
	"strconv"
	"time"
)

// must be safely-constructed!!
type AgreementWorker struct {
	worker.Worker            // embedded field
	db                       *bolt.DB
	httpClient               *http.Client
	userId                   string
	deviceId                 string
	deviceToken              string
	protocols                map[string]bool
	pm                       *policy.PolicyManager
	containerSyncUpEvent     bool
	containerSyncUpSucessful bool
	producerPH               map[string]producer.ProducerProtocolHandler
}

func NewAgreementWorker(cfg *config.HorizonConfig, db *bolt.DB, pm *policy.PolicyManager) *AgreementWorker {
	messages := make(chan events.Message)
	commands := make(chan worker.Command, 100)

	id, _ := device.Id()

	token := ""
	if dev, _ := persistence.FindExchangeDevice(db); dev != nil {
		token = dev.Token
	}

	worker := &AgreementWorker{
		Worker: worker.Worker{
			Manager: worker.Manager{
				Config:   cfg,
				Messages: messages,
			},

			Commands: commands,
		},

		db:                  db,
		httpClient:          &http.Client{Timeout: time.Duration(config.HTTPDEFAULTTIMEOUT*time.Millisecond)},
		protocols:           make(map[string]bool),
		pm:                  pm,
		deviceId:            id,
		deviceToken:         token,
		producerPH:          make(map[string]producer.ProducerProtocolHandler),
	}

	glog.Info("Starting Agreement worker")
	worker.start()
	return worker
}

func (w *AgreementWorker) Messages() chan events.Message {
	return w.Worker.Manager.Messages
}

func (w *AgreementWorker) NewEvent(incoming events.Message) {

	switch incoming.(type) {
	case *events.EdgeRegisteredExchangeMessage:
		msg, _ := incoming.(*events.EdgeRegisteredExchangeMessage)
		w.Commands <- NewDeviceRegisteredCommand(msg.Token())

	case *events.PolicyCreatedMessage:
		msg, _ := incoming.(*events.PolicyCreatedMessage)

		switch msg.Event().Id {
		case events.NEW_POLICY:
			w.Commands <- NewAdvertisePolicyCommand(msg.PolicyFile())
		default:
			glog.Errorf("AgreementWorker received Unsupported event: %v", incoming.Event().Id)
		}

	case *events.BlockchainClientInitializedMessage:
		msg, _ := incoming.(*events.BlockchainClientInitializedMessage)
		switch msg.Event().Id {
		case events.BC_CLIENT_INITIALIZED:
			cmd := producer.NewBCInitializedCommand(msg)
			w.Commands <- cmd
		}

	case *events.BlockchainClientStoppingMessage:
		msg, _ := incoming.(*events.BlockchainClientStoppingMessage)
		switch msg.Event().Id {
		case events.BC_CLIENT_STOPPING:
			cmd := producer.NewBCStoppingCommand(msg)
			w.Commands <- cmd
		}

	case *events.AccountFundedMessage:
		msg, _ := incoming.(*events.AccountFundedMessage)
		switch msg.Event().Id {
		case events.ACCOUNT_FUNDED:
			cmd := producer.NewBCWritableCommand(msg)
			w.Commands <- cmd
		}

	case *events.ExchangeDeviceMessage:
		msg, _ := incoming.(*events.ExchangeDeviceMessage)
		switch msg.Event().Id {
		case events.RECEIVED_EXCHANGE_DEV_MSG:
			w.Commands <- producer.NewExchangeMessageCommand(*msg)
		}

	case *events.DeviceContainersSyncedMessage:
		msg, _ := incoming.(*events.DeviceContainersSyncedMessage)
		switch msg.Event().Id {
		case events.DEVICE_CONTAINERS_SYNCED:
			w.containerSyncUpSucessful = msg.IsCompleted()
			w.containerSyncUpEvent = true
		}

	default: //nothing
	}

	return
}

func (w *AgreementWorker) start() {

	glog.Info(logString(fmt.Sprintf("started")))

	// Enter the command processing loop. Initialization is complete so wait for commands to
	// perform. Commands are created as the result of events that are triggered elsewhere
	// in the system.
	go func() {

		// Block for the container syncup message, to make sure the docker state matches our local DB.
		for {
			if w.containerSyncUpEvent == false {
				time.Sleep(time.Duration(5) * time.Second)
				glog.V(3).Infof("AgreementWorker waiting for container syncup to be done.")
			} else if w.containerSyncUpSucessful {
				break
			} else {
				panic(logString(fmt.Sprintf("Terminating, unable to sync up containers")))
			}
		}

		if w.deviceToken != "" {

			// Establish agreement protocol handlers
			for _, protocolName := range policy.AllAgreementProtocols() {
				pph := producer.CreateProducerPH(protocolName, w.Worker.Manager.Config, w.db, w.pm, w.deviceId, w.deviceToken)
				pph.Initialize()
				w.producerPH[protocolName] = pph
			}

			// Sync up between what's in our database versus what's in the exchange, and make sure that the policy manager's
			// agreement counts are correct. This function will cancel any agreements whose state might have changed
			// while the device was down. We will also check to make sure that policies havent changed. If they have, then
			// we will cancel agreements and allow them to re-negotiate.
			if err := w.syncOnInit(); err != nil {
				glog.Errorf(logString(fmt.Sprintf("Terminating, unable to sync up, error: %v", err)))
				panic(logString(fmt.Sprintf("Terminating, unable to complete agreement sync up, error: %v", err)))
			} else {
				w.Messages() <- events.NewDeviceAgreementsSyncedMessage(events.DEVICE_AGREEMENTS_SYNCED, true)
			}

			// If the device is registered, start heartbeating. If the device isn't registered yet, then we will
			// start heartbeating when the registration event comes in.
			targetURL := w.Manager.Config.Edge.ExchangeURL + "devices/" + w.deviceId + "/heartbeat"
			go exchange.Heartbeat(w.httpClient, targetURL, w.deviceId, w.deviceToken, w.Worker.Manager.Config.Edge.ExchangeHeartbeat)

		}

		// Publish what we have for the world to see
		if err := w.advertiseAllPolicies(w.Worker.Manager.Config.Edge.PolicyPath); err != nil {
			glog.Errorf(logString(fmt.Sprintf("unable to advertise policies with exchange, error: %v", err)))
		}
		
		// Handle agreement processor commands
		for {
			glog.V(2).Infof(logString(fmt.Sprintf("blocking for commands")))
			command := <-w.Commands
			glog.V(2).Infof(logString(fmt.Sprintf("received command: %v", command.ShortString())))
			glog.V(5).Infof(logString(fmt.Sprintf("received command: %v", command)))

			switch command.(type) {
			case *DeviceRegisteredCommand:
				cmd, _ := command.(*DeviceRegisteredCommand)
				w.handleDeviceRegistered(cmd)

			case *TerminateCommand:
				cmd, _ := command.(*TerminateCommand)
				glog.Errorf(logString(fmt.Sprintf("terminating, reason: %v", cmd.reason)))
				return

			case *AdvertisePolicyCommand:
				cmd, _ := command.(*AdvertisePolicyCommand)

				if newPolicy, err := policy.ReadPolicyFile(cmd.PolicyFile); err != nil {
					glog.Errorf(logString(fmt.Sprintf("unable to read policy file %v into memory, error: %v", cmd.PolicyFile, err)))
				} else if err := w.pm.AddPolicy(newPolicy); err != nil {
					glog.Errorf(logString(fmt.Sprintf("policy name is a duplicate, not added, error: %v", err)))
				} else {

					// Publish what we have for the world to see
					if err := w.advertiseAllPolicies(w.Worker.Manager.Config.Edge.PolicyPath); err != nil {
						glog.Errorf(logString(fmt.Sprintf("unable to advertise policies with exchange, error: %v", err)))
					}
				}

			case *producer.ExchangeMessageCommand:
				cmd, _ := command.(*producer.ExchangeMessageCommand)
				exchangeMsg := new(exchange.DeviceMessage)
				if err := json.Unmarshal(cmd.Msg.ExchangeMessage(), &exchangeMsg); err != nil {
					glog.Errorf(logString(fmt.Sprintf("unable to demarshal exchange device message %v, error %v", cmd.Msg.ExchangeMessage(), err)))
				} else if there, err := w.messageInExchange(exchangeMsg.MsgId); err != nil {
					glog.Errorf(logString(fmt.Sprintf("unable to get messages from the exchange, error %v", err)))
					continue
				} else if !there {
					glog.V(3).Infof(logString(fmt.Sprintf("ignoring message %v, already deleted from the exchange.", exchangeMsg.MsgId)))
					continue
				}

				protocolMsg := cmd.Msg.ProtocolMessage()

				glog.V(3).Infof(logString(fmt.Sprintf("received message %v from the exchange", exchangeMsg.MsgId)))

				// Process the message if it's a proposal.
				deleteMessage := true

				if msgProtocol, err := abstractprotocol.ExtractProtocol(protocolMsg); err != nil {
					glog.Errorf(logString(fmt.Sprintf("unable to extract agreement protocol name from message %v", protocolMsg)))
				} else if _, ok := w.producerPH[msgProtocol]; !ok {
					glog.Infof(logString(fmt.Sprintf("unable to direct exchange message %v to a protocol handler, deleting it.", protocolMsg)))
				} else if p, err := w.producerPH[msgProtocol].AgreementProtocolHandler("", "").ValidateProposal(protocolMsg); err != nil {
					glog.V(5).Infof(logString(fmt.Sprintf("Proposal handler ignoring non-proposal message: %s due to %v", cmd.Msg.ShortProtocolMessage(), err)))
					deleteMessage = false
				} else {
					deleteMessage = w.producerPH[msgProtocol].HandleProposalMessage(p, protocolMsg, exchangeMsg)
				}

				if deleteMessage {
					if err := w.deleteMessage(exchangeMsg); err != nil {
						glog.Errorf(logString(fmt.Sprintf("error deleting exchange message %v, error %v", exchangeMsg.MsgId, err)))
					}
				}

			case *producer.BCInitializedCommand:
				cmd, _ := command.(*producer.BCInitializedCommand)
				for _, pph := range w.producerPH {
					pph.SetBlockchainClientAvailable(cmd)
				}

			case *producer.BCStoppingCommand:
				cmd, _ := command.(*producer.BCStoppingCommand)
				for _, pph := range w.producerPH {
					pph.SetBlockchainClientNotAvailable(cmd)
				}

			case *producer.BCWritableCommand:
				cmd, _ := command.(*producer.BCWritableCommand)
				for _, pph := range w.producerPH {
					pph.SetBlockchainWritable(cmd)
				}

			default:
				glog.Errorf("Unknown command (%T): %v", command, command)
			}

			glog.V(5).Infof(logString(fmt.Sprintf("handled command")))
			runtime.Gosched()
		}

	}()

	glog.Info(logString(fmt.Sprintf("waiting for commands.")))

}

func (w *AgreementWorker) handleDeviceRegistered(cmd *DeviceRegisteredCommand) {

	w.deviceToken = cmd.Token

	if len(w.producerPH) == 0 {
		// Establish agreement protocol handlers
		for _, protocolName := range policy.AllAgreementProtocols() {
			pph := producer.CreateProducerPH(protocolName, w.Worker.Manager.Config, w.db, w.pm, w.deviceId, w.deviceToken)
			pph.Initialize()
			w.producerPH[protocolName] = pph
		}
	}

	// Start the go thread that heartbeats to the exchange
	targetURL := w.Manager.Config.Edge.ExchangeURL + "devices/" + w.deviceId + "/heartbeat"
	go exchange.Heartbeat(w.httpClient, targetURL, w.deviceId, w.deviceToken, w.Worker.Manager.Config.Edge.ExchangeHeartbeat)

}

// This function is only called when anax device side initializes. The agbot has it's own initialization checking.
// This function is responsible for reconciling the agreements in our local DB with the agreements recorded in the exchange
// and the blockchain, as well as looking for agreements that need to change based on changes to policy files. This function
// handles agreements that exist in the exchange for which we have no DB records, it handles DB records for which
// the state in the exchange is missing, and it handles agreements whose state has changed in the blockchain.
func (w *AgreementWorker) syncOnInit() error {

	glog.V(3).Infof(logString("beginning sync up."))

	// Reconcile the set of agreements recorded in the exchange for this device with the agreements in the local DB.
	// First get all the agreements for this device from the exchange.

	exchangeDeviceAgreements, err := w.getAllAgreements()
	if err != nil {
		return errors.New(logString(fmt.Sprintf("encountered error getting device agreement list from exchange, error %v", err)))
	} else {

		// Loop through each agreement in the exchange and search for that agreement in our DB. If it should not
		// be in the exchange, then we have to delete it from the exchange because its presence in the exchange
		// prevents an agbot from making an agreement with our device. It is posible to have a DB record for
		// an agreement that is not yet recorded in the exchange (this case is handled later in this function),
		// but the reverse should not occur normally. Agreements in the exchange must have a record on our local DB.
		for exchangeAg, _ := range exchangeDeviceAgreements {
			if agreements, err := persistence.FindEstablishedAgreementsAllProtocols(w.db, policy.AllAgreementProtocols(), []persistence.EAFilter{persistence.IdEAFilter(exchangeAg), persistence.UnarchivedEAFilter()}); err != nil {
				glog.Errorf(logString(fmt.Sprintf("error searching for agreement %v from exchange agreements", exchangeAg, err)))
			} else if len(agreements) == 0 {
				glog.V(3).Infof(logString(fmt.Sprintf("found agreement %v in the exchange that is not in our DB.", exchangeAg)))
				// Delete the agreement from the exchange.
				if err := deleteProducerAgreement(w.Config.Edge.ExchangeURL, w.deviceId, w.deviceToken, exchangeAg); err != nil {
					glog.Errorf(logString(fmt.Sprintf("error deleting agreement %v in exchange: %v", exchangeAg, err)))
				}
			}
		}

	}

	// Now perform the reverse set of checks, looping through our database and checking each record for accuracy with the exchange
	// and the blockchain.
	if agreements, err := persistence.FindEstablishedAgreementsAllProtocols(w.db, policy.AllAgreementProtocols(), []persistence.EAFilter{persistence.UnarchivedEAFilter()}); err == nil {

		neededBCInstances := make(map[string]map[string]bool)

		// If there are agreemens in the database then we will assume that the device is already registered
		for _, ag := range agreements {

			// Make a list of all blockchain instances in use by agreements in our DB so that we can make sure there is a
			// blockchain client running for each instance.
			bcType, bcName := w.producerPH[ag.AgreementProtocol].GetKnownBlockchain(&ag)

			if len(neededBCInstances[bcType]) == 0 {
				neededBCInstances[bcType] = make(map[string]bool)
			}
			neededBCInstances[bcType][bcName] = true

			// If there is an active agreement that is marked as terminated, then anax was restarted in the middle of
			// termination processing, and therefore we dont know how far it got. Initiate a cancel again to clean it up.
			if ag.AgreementTerminatedTime != 0 {
				reason := uint(ag.TerminatedReason)
				if _, err := persistence.AgreementStateForceTerminated(w.db, ag.CurrentAgreementId, ag.AgreementProtocol); err != nil {
					glog.Errorf(logString(fmt.Sprintf("unable to set force termination for agreement %v, error %v", ag.CurrentAgreementId, err)))
				}
				w.Messages() <- events.NewInitAgreementCancelationMessage(events.AGREEMENT_ENDED, reason, ag.AgreementProtocol, ag.CurrentAgreementId, ag.CurrentDeployment)

			// If the agreement's protocol requires that it is recorded externally in some way, verify that it is present there (e.g. a blockchain).
			// Make sure the external state agrees with our local DB state for this agreement. If not, then we might need to cancel the agreement.
			// Anax could have been down for a long time (or inoperable), and the external state may have changed.
			} else if ok, err := w.verifyAgreement(&ag, w.producerPH[ag.AgreementProtocol], bcType, bcName); err != nil {
				return errors.New(logString(fmt.Sprintf("unable to check for agreement %v in blockchain, error %v", ag.CurrentAgreementId, err)))
			} else if !ok {
				w.Messages() <- events.NewInitAgreementCancelationMessage(events.AGREEMENT_ENDED, w.producerPH[ag.AgreementProtocol].GetTerminationCode(producer.TERM_REASON_AGBOT_REQUESTED), ag.AgreementProtocol, ag.CurrentAgreementId, ag.CurrentDeployment)

			// If the agreement has been started then we just need to make sure that the policy manager's agreement counts
			// are correct. Even for already timedout agreements, the governance process will cleanup old and outdated agreements,
			// so we don't need to do anything here.

			} else if proposal, err := w.producerPH[ag.AgreementProtocol].AgreementProtocolHandler("", "").DemarshalProposal(ag.Proposal); err != nil {
				glog.Errorf(logString(fmt.Sprintf("unable to demarshal proposal for agreement %v, error %v", ag.CurrentAgreementId, err)))
			} else if pol, err := policy.DemarshalPolicy(proposal.ProducerPolicy()); err != nil {
				glog.Errorf(logString(fmt.Sprintf("unable to demarshal policy for agreement %v, error %v", ag.CurrentAgreementId, err)))
			} else if existingPol := w.pm.GetPolicy(pol.Header.Name); existingPol == nil {
				glog.Errorf(logString(fmt.Sprintf("agreement %v has a policy %v that doesn't exist anymore", ag.CurrentAgreementId, pol.Header.Name)))
				w.Messages() <- events.NewInitAgreementCancelationMessage(events.AGREEMENT_ENDED, w.producerPH[ag.AgreementProtocol].GetTerminationCode(producer.TERM_REASON_POLICY_CHANGED), ag.AgreementProtocol, ag.CurrentAgreementId, ag.CurrentDeployment)

			} else if err := w.pm.MatchesMine(pol); err != nil {
				glog.Warningf(logString(fmt.Sprintf("agreement %v has a policy %v that has changed.", ag.CurrentAgreementId, pol.Header.Name)))
				w.Messages() <- events.NewInitAgreementCancelationMessage(events.AGREEMENT_ENDED, w.producerPH[ag.AgreementProtocol].GetTerminationCode(producer.TERM_REASON_POLICY_CHANGED), ag.AgreementProtocol, ag.CurrentAgreementId, ag.CurrentDeployment)

			} else if err := w.pm.AttemptingAgreement(existingPol, ag.CurrentAgreementId); err != nil {
				glog.Errorf(logString(fmt.Sprintf("cannot update agreement count for %v, error: %v", ag.CurrentAgreementId, err)))
			} else if err := w.pm.FinalAgreement(existingPol, ag.CurrentAgreementId); err != nil {
				glog.Errorf(logString(fmt.Sprintf("cannot update agreement count for %v, error: %v", ag.CurrentAgreementId, err)))

				// There is a small window where an agreement might not have been recorded in the exchange. Let's just make sure.
			} else if ag.AgreementAcceptedTime != 0 && ag.AgreementTerminatedTime == 0 {

				if _, there := exchangeDeviceAgreements[ag.CurrentAgreementId]; !there {
					glog.V(3).Infof(logString(fmt.Sprintf("agreement %v missing from exchange, adding it back in.", ag.CurrentAgreementId)))
					state := ""
					if ag.AgreementFinalizedTime != 0 {
						state = "Finalized Agreement"
					} else if ag.AgreementAcceptedTime != 0 {
						state = "Agree to proposal"
					} else {
						state = "unknown"
					}
					w.recordAgreementState(ag.CurrentAgreementId, pol.APISpecs[0].SpecRef, state)
				}
				glog.V(3).Infof(logString(fmt.Sprintf("added agreement %v to policy agreement counter.", ag.CurrentAgreementId)))
			}

		}

		// Fire off start requests for each BC client that we need running. The blockchain worker and the container worker will tolerate
		// a start request for containers that are already running.
		for typeName, instMap := range neededBCInstances {
			for instName, _ := range instMap {
				w.Messages() <- events.NewNewBCContainerMessage(events.NEW_BC_CLIENT, typeName, instName, w.Config.Edge.ExchangeURL, w.deviceId, w.deviceToken)
			}
		}


	} else {
		return errors.New(logString(fmt.Sprintf("error searching database: %v", err)))
	}

	glog.V(3).Infof(logString("sync up completed normally."))
	return nil

}

// This function verifies that an agreement is present in the blockchain. An agreement might not be present for a variety of reasons,
// some of which are legitimate. The purpose of this routine is to figure out whether or not an agreement cancellation
// has occurred. It returns false if the agreement needs to be cancelled, or if there was an error.
func (w *AgreementWorker) verifyAgreement(ag *persistence.EstablishedAgreement, pph producer.ProducerProtocolHandler, bcType string, bcName string) (bool, error) {

	// Agreements that havent been accepted yet by the device will not be in any external store so it's ok if they aren't there,
	// so return true.
	if ag.AgreementAcceptedTime == 0 {
		return true, nil
	} else if !pph.IsBlockchainClientAvailable(bcType, bcName) || !pph.IsAgreementVerifiable(ag) {
		glog.Warningf(logString(fmt.Sprintf("for %v unable to verify agreement, agreement protocol handler is not ready", ag.CurrentAgreementId)))
		return true, nil
	}

	// Check to see if the agreement is in an external store.
	if pph.AgreementProtocolHandler(bcType, bcName) == nil {
		glog.Errorf(logString(fmt.Sprintf("for %v unable to verify agreement, agreement protocol handler is not ready", ag.CurrentAgreementId)))
	} else if recorded, err := pph.AgreementProtocolHandler(bcType, bcName).VerifyAgreement(ag.CurrentAgreementId, ag.CounterPartyAddress, ag.ProposalSig); err != nil {
		return false, errors.New(logString(fmt.Sprintf("encountered error verifying agreement %v on blockchain, error %v", ag.CurrentAgreementId, err)))
	} else if !recorded {
		// A finalized agreement should be in the external store.
		if ag.AgreementFinalizedTime != 0 && ag.AgreementTerminatedTime == 0 {
			glog.V(3).Infof(logString(fmt.Sprintf("agreement %v is not in the blockchain, cancelling.", ag.CurrentAgreementId)))
			return false, nil
		}
	}
	return true, nil

}

func (w *AgreementWorker) getAllAgreements() (map[string]exchange.DeviceAgreement, error) {

	var exchangeDeviceAgreements map[string]exchange.DeviceAgreement
	var resp interface{}
	resp = new(exchange.AllDeviceAgreementsResponse)

	targetURL := w.Worker.Manager.Config.Edge.ExchangeURL + "devices/" + w.deviceId + "/agreements"
	for {
		if err, tpErr := exchange.InvokeExchange(w.httpClient, "GET", targetURL, w.deviceId, w.deviceToken, nil, &resp); err != nil {
			glog.Errorf(err.Error())
			return exchangeDeviceAgreements, err
		} else if tpErr != nil {
			glog.Warningf(tpErr.Error())
			time.Sleep(10 * time.Second)
			continue
		} else {
			exchangeDeviceAgreements = resp.(*exchange.AllDeviceAgreementsResponse).Agreements
			glog.V(5).Infof(logString(fmt.Sprintf("found agreements %v in the exchange.", exchangeDeviceAgreements)))
			return exchangeDeviceAgreements, nil
		}
	}

}

// ===============================================================================================
// Utility functions
//

func (w *AgreementWorker) advertiseAllPolicies(location string) error {

	var pType, pValue, pCompare string
	var deviceName string

	if dev, err := persistence.FindExchangeDevice(w.db); err != nil {
		return errors.New(fmt.Sprintf("AgreementWorker received error getting device name: %v", err))
	} else if dev == nil {
		return errors.New("AgreementWorker could not get device name because no device was registered yet.")
	} else {
		deviceName = dev.Name
	}

	w.pm.UpgradeAgreementProtocols()
	policies := w.pm.GetAllPolicies()

	if len(policies) > 0 {
		ms := make([]exchange.Microservice, 0, 10)
		for _, p := range policies {
			newMS := new(exchange.Microservice)
			newMS.Url = p.APISpecs[0].SpecRef
			newMS.NumAgreements = p.MaxAgreements

			p.DataVerify.Obscure()
			if pBytes, err := json.Marshal(p); err != nil {
				return errors.New(fmt.Sprintf("AgreementWorker received error marshalling policy: %v", err))
			} else {
				newMS.Policy = string(pBytes)
			}

			if props, err := policy.RetrieveAllProperties(&p); err != nil {
				return errors.New(fmt.Sprintf("AgreementWorker received error calculating properties: %v", err))
			} else {
				for _, prop := range *props {
					switch prop.Value.(type) {
					case string:
						pType = "string"
						pValue = prop.Value.(string)
						pCompare = "in"
					case int:
						pType = "int"
						pValue = strconv.Itoa(prop.Value.(int))
						pCompare = ">="
					case bool:
						pType = "boolean"
						pValue = strconv.FormatBool(prop.Value.(bool))
						pCompare = "="
					case []string:
						pType = "list"
						pValue = exchange.ConvertToString(prop.Value.([]string))
						pCompare = "in"
					default:
						return errors.New(fmt.Sprintf("AgreementWorker encountered unsupported property type: %v", reflect.TypeOf(prop.Value).String()))
					}
					// Now put the property together
					newProp := &exchange.MSProp{
						Name:     prop.Name,
						Value:    pValue,
						PropType: pType,
						Op:       pCompare,
					}
					newMS.Properties = append(newMS.Properties, *newProp)
				}
			}

			ms = append(ms, *newMS)

		}

		pdr := exchange.CreateDevicePut(w.deviceToken, deviceName)
		pdr.RegisteredMicroservices = ms
		var resp interface{}
		resp = new(exchange.PutDeviceResponse)
		targetURL := w.Manager.Config.Edge.ExchangeURL + "devices/" + w.deviceId

		glog.V(3).Infof("AgreementWorker Registering microservices: %v at %v", pdr.ShortString(), targetURL)

		for {
			if err, tpErr := exchange.InvokeExchange(w.httpClient, "PUT", targetURL, w.deviceId, w.deviceToken, pdr, &resp); err != nil {
				return err
			} else if tpErr != nil {
				glog.Warningf(tpErr.Error())
				time.Sleep(10 * time.Second)
				continue
			} else {
				glog.V(3).Infof(logString(fmt.Sprintf("advertised policies for device %v in exchange: %v", w.deviceId, resp)))
				return nil
			}
		}
	}

	return nil
}

func (w *AgreementWorker) recordAgreementState(agreementId string, microservice string, state string) error {

	glog.V(5).Infof(logString(fmt.Sprintf("setting agreement %v state to %v", agreementId, state)))

	as := new(exchange.PutAgreementState)
	as.Microservice = microservice
	as.State = state
	var resp interface{}
	resp = new(exchange.PostDeviceResponse)
	targetURL := w.Manager.Config.Edge.ExchangeURL + "devices/" + w.deviceId + "/agreements/" + agreementId
	for {
		if err, tpErr := exchange.InvokeExchange(w.httpClient, "PUT", targetURL, w.deviceId, w.deviceToken, as, &resp); err != nil {
			glog.Errorf(err.Error())
			return err
		} else if tpErr != nil {
			glog.Warningf(tpErr.Error())
			time.Sleep(10 * time.Second)
			continue
		} else {
			glog.V(5).Infof(logString(fmt.Sprintf("set agreement %v to state %v", agreementId, state)))
			return nil
		}
	}

}

func deleteProducerAgreement(url string, deviceId string, token string, agreementId string) error {

	glog.V(5).Infof(logString(fmt.Sprintf("deleting agreement %v in exchange", agreementId)))

	var resp interface{}
	resp = new(exchange.PostDeviceResponse)
	targetURL := url + "devices/" + deviceId + "/agreements/" + agreementId
	for {
		if err, tpErr := exchange.InvokeExchange(&http.Client{Timeout: time.Duration(config.HTTPDEFAULTTIMEOUT*time.Millisecond)}, "DELETE", targetURL, deviceId, token, nil, &resp); err != nil {
			glog.Errorf(logString(fmt.Sprintf(err.Error())))
			return err
		} else if tpErr != nil {
			glog.Warningf(tpErr.Error())
			time.Sleep(10 * time.Second)
			continue
		} else {
			glog.V(5).Infof(logString(fmt.Sprintf("deleted agreement %v from exchange", agreementId)))
			return nil
		}
	}

}

func (w *AgreementWorker) deleteMessage(msg *exchange.DeviceMessage) error {
	var resp interface{}
	resp = new(exchange.PostDeviceResponse)
	targetURL := w.Manager.Config.Edge.ExchangeURL + "devices/" + w.deviceId + "/msgs/" + strconv.Itoa(msg.MsgId)
	for {
		if err, tpErr := exchange.InvokeExchange(w.httpClient, "DELETE", targetURL, w.deviceId, w.deviceToken, nil, &resp); err != nil {
			glog.Errorf(err.Error())
			return err
		} else if tpErr != nil {
			glog.Warningf(tpErr.Error())
			time.Sleep(10 * time.Second)
			continue
		} else {
			glog.V(3).Infof(logString(fmt.Sprintf("deleted message %v", msg.MsgId)))
			return nil
		}
	}
}

func (w *AgreementWorker) messageInExchange(msgId int) (bool, error) {
	var resp interface{}
	resp = new(exchange.GetDeviceMessageResponse)
	targetURL := w.Manager.Config.Edge.ExchangeURL + "devices/" + w.deviceId + "/msgs"
	for {
		if err, tpErr := exchange.InvokeExchange(w.httpClient, "GET", targetURL, w.deviceId, w.deviceToken, nil, &resp); err != nil {
			glog.Errorf(err.Error())
			return false, err
		} else if tpErr != nil {
			glog.Warningf(tpErr.Error())
			time.Sleep(10 * time.Second)
			continue
		} else {
			msgs := resp.(*exchange.GetDeviceMessageResponse).Messages
			for _, msg := range msgs {
				if msg.MsgId == msgId {
					return true, nil
				}
			}
			return false, nil
		}
	}
}

var logString = func(v interface{}) string {
	return fmt.Sprintf("AgreementWorker %v", v)
}
