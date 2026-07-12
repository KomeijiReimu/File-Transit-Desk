//go:build windows

package fsutil

import "golang.org/x/sys/windows"

func availableDiskSpace(path string) (available, total uint64, err error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var freeTotal uint64
	if err := windows.GetDiskFreeSpaceEx(pointer, &available, &total, &freeTotal); err != nil {
		return 0, 0, err
	}
	return available, total, nil
}
