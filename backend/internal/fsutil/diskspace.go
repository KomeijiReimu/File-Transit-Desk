package fsutil

import (
	"os"
	"path/filepath"
)

func AvailableDiskSpace(path string) (available, total uint64, err error) {
	existing, err := nearestExistingDirectory(path)
	if err != nil {
		return 0, 0, err
	}
	return availableDiskSpace(existing)
}

func nearestExistingDirectory(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(abs)
	for {
		info, statErr := os.Stat(current)
		if statErr == nil {
			if info.IsDir() {
				return current, nil
			}
			return filepath.Dir(current), nil
		}
		if !os.IsNotExist(statErr) {
			return "", statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", statErr
		}
		current = parent
	}
}

func saturatingMultiply(left, right uint64) uint64 {
	if left == 0 || right == 0 {
		return 0
	}
	if left > ^uint64(0)/right {
		return ^uint64(0)
	}
	return left * right
}
