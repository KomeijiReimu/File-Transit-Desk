//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package server

import (
	"errors"
	"os"
)

func promoteUploadNoReplace(stagingPath, destinationPath string) (bool, error) {
	if err := os.Link(stagingPath, destinationPath); err == nil {
		return true, nil
	} else if errors.Is(err, os.ErrExist) {
		return false, nil
	} else {
		return false, err
	}
}
