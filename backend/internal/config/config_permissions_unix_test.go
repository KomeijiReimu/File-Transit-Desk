//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareAtomicTempPermissionUnix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	prepared, _, err := PrepareAtomic(path, validTestConfig())
	if err != nil {
		t.Fatalf("prepare config: %v", err)
	}
	defer prepared.Abort()
	info, err := os.Stat(prepared.tempPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("expected 0600 temp, mode=%v err=%v", info.Mode().Perm(), err)
	}
}
