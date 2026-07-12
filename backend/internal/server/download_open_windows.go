//go:build windows

package server

import "os"

func openDownloadFile(path string) (*os.File, error) {
	return os.Open(path)
}
