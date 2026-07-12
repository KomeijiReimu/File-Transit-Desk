package fsutil

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestAvailableDiskSpaceUsesNearestExistingParent(t *testing.T) {
	root := t.TempDir()
	available, total, err := AvailableDiskSpace(filepath.Join(root, "missing", "child"))
	if err != nil {
		t.Fatalf("available disk space: %v", err)
	}
	if total == 0 || available > total {
		t.Fatalf("unexpected disk values available=%d total=%d", available, total)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, _, err := AvailableDiskSpace(file); err != nil {
		t.Fatalf("file path should use parent filesystem: %v", err)
	}
}

func TestSaturatingMultiply(t *testing.T) {
	if got := saturatingMultiply(math.MaxUint64, 2); got != math.MaxUint64 {
		t.Fatalf("expected saturated multiplication, got %d", got)
	}
	if got := saturatingMultiply(2, 3); got != 6 {
		t.Fatalf("unexpected multiplication result %d", got)
	}
}
