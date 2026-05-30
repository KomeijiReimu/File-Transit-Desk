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

func TestListHidesUploadTempFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".upload-abc.tmp"), []byte("partial"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "done.txt"), []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	entries, err := List(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "done.txt" {
		t.Fatalf("expected only committed file, got %+v", entries)
	}
}

func TestListDoesNotCreateMissingBase(t *testing.T) {
	// 浏览是只读操作，配置目录不存在时应返回错误，不能顺手在磁盘上创建目录。
	base := filepath.Join(t.TempDir(), "missing")
	if _, err := List(base, ""); err == nil {
		t.Fatalf("expected missing base to fail")
	}
	if _, err := os.Stat(base); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected missing base to remain absent, stat error=%v", err)
	}
}

func TestSafeNameTrimsWindowsEquivalentSuffixes(t *testing.T) {
	// 扩展名策略使用 SafeName 后的结果，尾随空格和点必须先被规范化，避免 bad.exe 这类名称绕过黑名单。
	cases := map[string]string{
		"bad.exe ":   "bad.exe",
		"bad.ps1.\t": "bad.ps1",
		"\t. ":       "file",
	}
	for input, want := range cases {
		if got := SafeName(input); got != want {
			t.Fatalf("SafeName(%q) = %q, want %q", input, got, want)
		}
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
