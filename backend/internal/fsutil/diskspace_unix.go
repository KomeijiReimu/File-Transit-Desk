//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package fsutil

import "syscall"

func availableDiskSpace(path string) (available, total uint64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}
	blockSize := uint64(stat.Bsize)
	return saturatingMultiply(uint64(stat.Bavail), blockSize), saturatingMultiply(uint64(stat.Blocks), blockSize), nil
}
