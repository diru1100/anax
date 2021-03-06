package sync_service

import (
	"fmt"
	"github.com/open-horizon/anax/cli/cliutils"
	"github.com/open-horizon/anax/i18n"
	"os"
	"path"
	"strings"
)

// ObjectDownLoad is to download data to a file named ${objectType}_${objectId}
func ObjectDownLoad(org string, userPw string, objType string, objId string, filePath string, overwrite bool) {
	// get message printer
	msgPrinter := i18n.GetMessagePrinter()

	if userPw == "" {
		cliutils.Fatal(cliutils.CLI_INPUT_ERROR, msgPrinter.Sprintf("must specify exchange credentials to access the model management service"))
	}

	// For this command, object type and id are required parameters, No null checking is needed.
	// Set the API key env var if that's what we're using.
	cliutils.SetWhetherUsingApiKey(userPw)

	// Call the MMS service over HTTP to download the object data.
	var data []byte
	urlPath := path.Join("api/v1/objects/", org, objType, objId, "/data")
	httpCode := cliutils.ExchangeGet("Model Management Service", cliutils.GetMMSUrl(), urlPath, cliutils.OrgAndCreds(org, userPw), []int{200, 404}, &data)
	if httpCode == 404 {
		cliutils.Fatal(cliutils.NOT_FOUND, msgPrinter.Sprintf("object '%s' of type '%s' not found in org %s", objId, objType, org))
	}

	var fileName string
	// if no fileName and filePath specified, data will be saved in current dir, with name {objectType}_{objectId}
	if filePath == "" {
		fileName = fmt.Sprintf("%s_%s", objType, objId)
	}

	if filePath != "" {
		// trim the ending "/" if there are more than 1 "/"
		for strings.HasSuffix(filePath, "//") {
			filePath = strings.TrimSuffix(filePath, "/")
		}

		fi, _ := os.Stat(filePath)
		if fi == nil {
			// filePath is not an existing dir, then consider it as fileName, need to remove "/" in the end
			if strings.HasSuffix(filePath, "/") {
				filePath = strings.TrimSuffix(filePath, "/")
			}
			fileName = filePath
		} else {
			if fi.IsDir() {
				if !strings.HasSuffix(filePath, "/") {
					filePath = filePath + "/"
				}
				fileName = fmt.Sprintf("%s%s_%s", filePath, objType, objId)
			} else {
				fileName = filePath
			}
		}
	}

	if !overwrite {
		if _, err := os.Stat(fileName); err == nil {
			cliutils.Fatal(cliutils.CLI_INPUT_ERROR, msgPrinter.Sprintf("File %s already exists. Please specify a different file path or file name. To overwrite the existing file, use the '--overwrite' flag.", fileName))
		}
	}

	file, err := os.Create(fileName)
	if err != nil {
		cliutils.Fatal(cliutils.INTERNAL_ERROR, msgPrinter.Sprintf("Failed to create file: %s", fileName))
	}

	defer file.Close()

	if _, err := file.Write(data); err != nil {
		cliutils.Fatal(cliutils.INTERNAL_ERROR, msgPrinter.Sprintf("Failed to save data for object '%s' of type '%s' to file %s", objId, objType, fileName))
	}

	msgPrinter.Printf("Data of object %v saved to file %v", objId, fileName)
	msgPrinter.Println()

}
