package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveBlocksTraversal(t *testing.T) {
	dir := t.TempDir()

	// 同时覆盖 Unix 和 Windows 风格路径，保证跨平台都拒绝目录穿越。
	bad := []string{"../x", "a/../x", "..", `/etc/passwd`, `a\..\x`}
	for _, rel := range bad {
		if _, _, err := Resolve(dir, rel); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("Resolve(%q) error = %v, want ErrUnsafePath", rel, err)
		}
	}
}

func TestResolveAllowsInside(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "a"), 0755); err != nil {
		t.Fatal(err)
	}
	full, rel, err := Resolve(dir, "a")
	if err != nil {
		t.Fatal(err)
	}
	if rel != "a" || full != filepath.Join(dir, "a") {
		t.Fatalf("got full=%q rel=%q", full, rel)
	}
}

func TestResolveBlocksSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on Windows")
	}
	base := t.TempDir()
	outside := t.TempDir()
	// 目标本身是符号链接时，Resolve 必须看真实路径是否仍在 base 内。
	link := filepath.Join(base, "outside")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if _, _, err := Resolve(base, "outside"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("Resolve through symlink error = %v, want ErrUnsafePath", err)
	}
}

func TestResolveForCreateBlocksSymlinkParentEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on Windows")
	}
	base := t.TempDir()
	outside := t.TempDir()
	// 新文件尚不存在时也要检查最近存在父目录，防止写入符号链接指向的外部目录。
	link := filepath.Join(base, "outside")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if _, _, err := ResolveForCreate(base, "outside/new-dir"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("ResolveForCreate through symlink error = %v, want ErrUnsafePath", err)
	}
}
