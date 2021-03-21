package httpd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/render"

	"github.com/drakkan/sftpgo/common"
	"github.com/drakkan/sftpgo/dataprovider"
	"github.com/drakkan/sftpgo/logger"
	"github.com/drakkan/sftpgo/vfs"
)

func validateBackupFile(outputFile string) (string, error) {
	if outputFile == "" {
		return "", errors.New("invalid or missing output-file")
	}
	if filepath.IsAbs(outputFile) {
		return "", fmt.Errorf("invalid output-file %#v: it must be a relative path", outputFile)
	}
	if strings.Contains(outputFile, "..") {
		return "", fmt.Errorf("invalid output-file %#v", outputFile)
	}
	outputFile = filepath.Join(backupsPath, outputFile)
	return outputFile, nil
}

func dumpData(w http.ResponseWriter, r *http.Request) {
	var outputFile, outputData, indent string
	if _, ok := r.URL.Query()["output-file"]; ok {
		outputFile = strings.TrimSpace(r.URL.Query().Get("output-file"))
	}
	if _, ok := r.URL.Query()["output-data"]; ok {
		outputData = strings.TrimSpace(r.URL.Query().Get("output-data"))
	}
	if _, ok := r.URL.Query()["indent"]; ok {
		indent = strings.TrimSpace(r.URL.Query().Get("indent"))
	}

	if outputData != "1" {
		var err error
		outputFile, err = validateBackupFile(outputFile)
		if err != nil {
			sendAPIResponse(w, r, err, "", http.StatusBadRequest)
			return
		}

		err = os.MkdirAll(filepath.Dir(outputFile), 0700)
		if err != nil {
			logger.Warn(logSender, "", "dumping data error: %v, output file: %#v", err, outputFile)
			sendAPIResponse(w, r, err, "", getRespStatus(err))
			return
		}
		logger.Debug(logSender, "", "dumping data to: %#v", outputFile)
	}

	backup, err := dataprovider.DumpData()
	if err != nil {
		logger.Warn(logSender, "", "dumping data error: %v, output file: %#v", err, outputFile)
		sendAPIResponse(w, r, err, "", getRespStatus(err))
		return
	}

	if outputData == "1" {
		w.Header().Set("Content-Disposition", "attachment; filename=\"sftpgo-backup.json\"")
		render.JSON(w, r, backup)
		return
	}

	var dump []byte
	if indent == "1" {
		dump, err = json.MarshalIndent(backup, "", "  ")
	} else {
		dump, err = json.Marshal(backup)
	}
	if err == nil {
		err = os.WriteFile(outputFile, dump, 0600)
	}
	if err != nil {
		logger.Warn(logSender, "", "dumping data error: %v, output file: %#v", err, outputFile)
		sendAPIResponse(w, r, err, "", getRespStatus(err))
		return
	}
	logger.Debug(logSender, "", "dumping data completed, output file: %#v, error: %v", outputFile, err)
	sendAPIResponse(w, r, err, "Data saved", http.StatusOK)
}

func loadDataFromRequest(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxRestoreSize)
	_, scanQuota, mode, err := getLoaddataOptions(r)
	if err != nil {
		sendAPIResponse(w, r, err, "", http.StatusBadRequest)
		return
	}

	content, err := io.ReadAll(r.Body)
	if err != nil || len(content) == 0 {
		if len(content) == 0 {
			err = dataprovider.NewValidationError("request body is required")
		}
		sendAPIResponse(w, r, err, "", getRespStatus(err))
		return
	}
	if err := restoreBackup(content, "", scanQuota, mode); err != nil {
		sendAPIResponse(w, r, err, "", getRespStatus(err))
	}
	sendAPIResponse(w, r, err, "Data restored", http.StatusOK)
}

func loadData(w http.ResponseWriter, r *http.Request) {
	inputFile, scanQuota, mode, err := getLoaddataOptions(r)
	if err != nil {
		sendAPIResponse(w, r, err, "", http.StatusBadRequest)
		return
	}
	if !filepath.IsAbs(inputFile) {
		sendAPIResponse(w, r, fmt.Errorf("invalid input_file %#v: it must be an absolute path", inputFile), "", http.StatusBadRequest)
		return
	}
	fi, err := os.Stat(inputFile)
	if err != nil {
		sendAPIResponse(w, r, err, "", getRespStatus(err))
		return
	}
	if fi.Size() > MaxRestoreSize {
		sendAPIResponse(w, r, err, fmt.Sprintf("Unable to restore input file: %#v size too big: %v/%v bytes",
			inputFile, fi.Size(), MaxRestoreSize), http.StatusBadRequest)
		return
	}

	content, err := os.ReadFile(inputFile)
	if err != nil {
		sendAPIResponse(w, r, err, "", getRespStatus(err))
		return
	}
	if err := restoreBackup(content, inputFile, scanQuota, mode); err != nil {
		sendAPIResponse(w, r, err, "", getRespStatus(err))
	}
	sendAPIResponse(w, r, err, "Data restored", http.StatusOK)
}

func restoreBackup(content []byte, inputFile string, scanQuota, mode int) error {
	dump, err := dataprovider.ParseDumpData(content)
	if err != nil {
		return dataprovider.NewValidationError(fmt.Sprintf("Unable to parse backup content: %v", err))
	}

	if err = RestoreFolders(dump.Folders, inputFile, mode, scanQuota); err != nil {
		return err
	}

	if err = RestoreUsers(dump.Users, inputFile, mode, scanQuota); err != nil {
		return err
	}

	if err = RestoreAdmins(dump.Admins, inputFile, mode); err != nil {
		return err
	}

	logger.Debug(logSender, "", "backup restored, users: %v, folders: %v, admins: %vs",
		len(dump.Users), len(dump.Folders), len(dump.Admins))

	return nil
}

func getLoaddataOptions(r *http.Request) (string, int, int, error) {
	var inputFile string
	var err error
	scanQuota := 0
	restoreMode := 0
	if _, ok := r.URL.Query()["input-file"]; ok {
		inputFile = strings.TrimSpace(r.URL.Query().Get("input-file"))
	}
	if _, ok := r.URL.Query()["scan-quota"]; ok {
		scanQuota, err = strconv.Atoi(r.URL.Query().Get("scan-quota"))
		if err != nil {
			err = fmt.Errorf("invalid scan_quota: %v", err)
			return inputFile, scanQuota, restoreMode, err
		}
	}
	if _, ok := r.URL.Query()["mode"]; ok {
		restoreMode, err = strconv.Atoi(r.URL.Query().Get("mode"))
		if err != nil {
			err = fmt.Errorf("invalid mode: %v", err)
			return inputFile, scanQuota, restoreMode, err
		}
	}
	return inputFile, scanQuota, restoreMode, err
}

// RestoreFolders restores the specified folders
func RestoreFolders(folders []vfs.BaseVirtualFolder, inputFile string, mode, scanQuota int) error {
	for _, folder := range folders {
		folder := folder // pin
		f, err := dataprovider.GetFolderByName(folder.Name)
		if err == nil {
			if mode == 1 {
				logger.Debug(logSender, "", "loaddata mode 1, existing folder %#v not updated", folder.Name)
				continue
			}
			folder.ID = f.ID
			err = dataprovider.UpdateFolder(&folder, f.Users)
			logger.Debug(logSender, "", "restoring existing folder: %+v, dump file: %#v, error: %v", folder, inputFile, err)
		} else {
			folder.Users = nil
			err = dataprovider.AddFolder(&folder)
			logger.Debug(logSender, "", "adding new folder: %+v, dump file: %#v, error: %v", folder, inputFile, err)
		}
		if err != nil {
			return err
		}
		if scanQuota >= 1 {
			if common.QuotaScans.AddVFolderQuotaScan(folder.Name) {
				logger.Debug(logSender, "", "starting quota scan for restored folder: %#v", folder.Name)
				go doFolderQuotaScan(folder) //nolint:errcheck
			}
		}
	}
	return nil
}

// RestoreAdmins restores the specified admins
func RestoreAdmins(admins []dataprovider.Admin, inputFile string, mode int) error {
	for _, admin := range admins {
		admin := admin // pin
		a, err := dataprovider.AdminExists(admin.Username)
		if err == nil {
			if mode == 1 {
				logger.Debug(logSender, "", "loaddata mode 1, existing admin %#v not updated", a.Username)
				continue
			}
			admin.ID = a.ID
			err = dataprovider.UpdateAdmin(&admin)
			admin.Password = redactedSecret
			logger.Debug(logSender, "", "restoring existing admin: %+v, dump file: %#v, error: %v", admin, inputFile, err)
		} else {
			err = dataprovider.AddAdmin(&admin)
			admin.Password = redactedSecret
			logger.Debug(logSender, "", "adding new admin: %+v, dump file: %#v, error: %v", admin, inputFile, err)
		}
		if err != nil {
			return err
		}
	}

	return nil
}

// RestoreUsers restores the specified users
func RestoreUsers(users []dataprovider.User, inputFile string, mode, scanQuota int) error {
	for _, user := range users {
		user := user // pin
		u, err := dataprovider.UserExists(user.Username)
		if err == nil {
			if mode == 1 {
				logger.Debug(logSender, "", "loaddata mode 1, existing user %#v not updated", u.Username)
				continue
			}
			user.ID = u.ID
			err = dataprovider.UpdateUser(&user)
			user.Password = redactedSecret
			logger.Debug(logSender, "", "restoring existing user: %+v, dump file: %#v, error: %v", user, inputFile, err)
			if mode == 2 && err == nil {
				disconnectUser(user.Username)
			}
		} else {
			err = dataprovider.AddUser(&user)
			user.Password = redactedSecret
			logger.Debug(logSender, "", "adding new user: %+v, dump file: %#v, error: %v", user, inputFile, err)
		}
		if err != nil {
			return err
		}
		if scanQuota == 1 || (scanQuota == 2 && user.HasQuotaRestrictions()) {
			if common.QuotaScans.AddUserQuotaScan(user.Username) {
				logger.Debug(logSender, "", "starting quota scan for restored user: %#v", user.Username)
				go doQuotaScan(user) //nolint:errcheck
			}
		}
	}
	return nil
}
