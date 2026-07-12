//go:build windows

package config

// Windows does not provide a portable directory fsync through os.File.Sync.
// Candidate and backup files are synced before rename, so parent-directory
// durability is best-effort rather than making normal online saves fail.
func syncDir(string) error {
	return nil
}
