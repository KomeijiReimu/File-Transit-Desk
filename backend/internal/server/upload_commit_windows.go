//go:build windows

package server

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

func promoteUploadNoReplace(stagingPath, destinationPath string) (bool, error) {
	from, err := windows.UTF16PtrFromString(stagingPath)
	if err != nil {
		return false, err
	}
	to, err := windows.UTF16PtrFromString(destinationPath)
	if err != nil {
		return false, err
	}
	if err := windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH); err == nil {
		return true, nil
	} else if errors.Is(err, syscall.ERROR_FILE_EXISTS) || errors.Is(err, syscall.ERROR_ALREADY_EXISTS) {
		return false, nil
	} else {
		return false, err
	}
}
