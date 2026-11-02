//go:build !windows

package server

import "filetrans-backend/internal/config"

func systemPickerRoots() ([]config.FilePickerRoot, error) {
	return []config.FilePickerRoot{systemRoot("system-root", "/")}, nil
}
func systemPickerRoot(id string) (config.FilePickerRoot, bool) {
	return systemRoot("system-root", "/"), id == "system-root"
}
func systemRootForAddress(_ string) (config.FilePickerRoot, error) {
	return systemRoot("system-root", "/"), nil
}
