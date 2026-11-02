package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestLayeredMigrationAndLosslessSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	c := validTestConfig()
	c.Database.Path = "./legacy-data/test.db"
	c.Storage.Dirs[0].Path = "./Legacy-Uploads"
	b, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	legacy, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveAtomic(path, legacy); !errors.Is(err, ErrLegacyConfig) {
		t.Fatalf("legacy write must require migration: %v", err)
	}
	if err := MigrateConfig(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Legacy() || loaded.Database.Path != filepath.Join(cwd, "legacy-data/test.db") || loaded.Storage.Dirs[0].Path != filepath.Join(cwd, "Legacy-Uploads") {
		t.Fatalf("migration changed runtime paths: %+v", loaded)
	}
	bootstrap, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(dir, "settings.yaml")
	settings, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(settings, []byte("password_hash")) {
		t.Fatal("credentials leaked into settings")
	}
	if err := MigrateConfig(path); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	// Unknown extensions belong to the new-format online-save scenario, not to
	// legacy migration, where their ownership/path semantics cannot be inferred.
	var bootstrapDoc, settingsDoc yaml.Node
	if err := yaml.Unmarshal(bootstrap, &bootstrapDoc); err != nil {
		t.Fatal(err)
	}
	adminNode := nodeField(&bootstrapDoc, "admin")
	adminNode.Content = append(adminNode.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "operator_note"}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "保留管理员扩展"})
	bootstrap, err = yaml.Marshal(&bootstrapDoc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bootstrap, 0600); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(settings, &settingsDoc); err != nil {
		t.Fatal(err)
	}
	var extension yaml.Node
	if err := extension.Encode(map[string]any{"enabled": false, "number": 0}); err != nil {
		t.Fatal(err)
	}
	settingsDoc.Content[0].Content = append(settingsDoc.Content[0].Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "extension"}, &extension)
	resourceNode := nodeField(nodeField(&settingsDoc, "storage"), "dirs").Content[0]
	resourceNode.Content = append(resourceNode.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "plugin_flag"}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "false"})
	settings, err = yaml.Marshal(&settingsDoc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, settings, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Accounts.RegistrationEnabled = false
	if err := SaveAtomic(path, loaded); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, bootstrap) {
		t.Fatalf("online save touched bootstrap: %v", err)
	}
	loaded, err = Load(path)
	if err != nil || loaded.Accounts.RegistrationEnabled {
		t.Fatalf("policy not saved: %v", err)
	}
	resource := loaded.Storage.Dirs[0]
	resource.Type = ResourceFile
	resource.AllowUpload = false
	loaded.SetResources([]Dir{resource})
	if err := SaveAtomic(path, loaded); err != nil {
		t.Fatal(err)
	}
	after, err = os.ReadFile(settingsPath)
	if err != nil || !strings.Contains(string(after), "enabled: false") || !strings.Contains(string(after), "number: 0") || !strings.Contains(string(after), "plugin_flag: false") {
		t.Fatalf("unknown nodes not retained: %s %v", after, err)
	}
	fileConfig, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	directory := resource
	directory.Type = ResourceDirectory
	for _, tc := range []struct {
		name      string
		resources []Dir
	}{
		{"delete last file", nil},
		{"move last file to directory", []Dir{directory}},
	} {
		next := fileConfig.Clone()
		next.SetResources(tc.resources)
		if err := SaveAtomic(path, next); err != nil {
			t.Fatalf("%s save: %v", tc.name, err)
		}
		reloaded, err := Load(path)
		if err != nil {
			t.Errorf("%s reload: %v", tc.name, err)
			continue
		}
		if len(reloaded.Storage.Shares) != 0 || len(reloaded.Storage.Dirs) != len(tc.resources) {
			t.Errorf("%s: shares=%v dirs=%v", tc.name, reloaded.Storage.Shares, reloaded.Storage.Dirs)
			continue
		}
		if len(tc.resources) == 1 && (reloaded.Storage.Dirs[0].ID != directory.ID || reloaded.Storage.Dirs[0].Type != ResourceDirectory) {
			t.Errorf("%s: expected only directory %q, got %+v", tc.name, directory.ID, reloaded.Storage.Dirs)
		}
	}
	backup, err := os.ReadFile(path + ".pre-migration.bak")
	if err != nil || !bytes.Equal(backup, b) {
		t.Fatalf("original backup: %v", err)
	}
}

func TestLayeredRequiredSettingsConflictAndSHAOnlyMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	c := validTestConfig()
	if err := SaveAtomic(path, c); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(dir, "settings.yaml")
	if err := os.WriteFile(settings, []byte("different: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, nested := range []bool{false, true} {
		var doc yaml.Node
		if err := yaml.Unmarshal(original, &doc); err != nil {
			t.Fatal(err)
		}
		target := doc.Content[0]
		field := "unassigned_extension"
		if nested {
			target = nodeField(nodeField(&doc, "storage"), "dirs").Content[0]
			field = "plugin_path"
		}
		target.Content = append(target.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: field}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "private-relative-value"})
		unknown, err := yaml.Marshal(&doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, unknown, 0600); err != nil {
			t.Fatal(err)
		}
		if loaded, err := Load(path); err != nil || !loaded.Legacy() {
			t.Fatalf("ordinary legacy read became strict: %v", err)
		}
		migrationErr := MigrateConfig(path)
		if migrationErr == nil || !strings.Contains(migrationErr.Error(), field) || !strings.Contains(migrationErr.Error(), "line ") || !strings.Contains(migrationErr.Error(), path) || strings.Contains(migrationErr.Error(), "private-relative-value") {
			t.Fatalf("expected field/line diagnostic without its value: %v", migrationErr)
		}
		if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, unknown) {
			t.Fatalf("unknown-field rejection changed original: %v", err)
		}
		if after, err := os.ReadFile(settings); err != nil || string(after) != "different: true\n" {
			t.Fatalf("unknown-field rejection changed existing settings: %v", err)
		}
		if _, err := os.Stat(path + ".pre-migration.bak"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unknown-field rejection created a migration backup: %v", err)
		}
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := MigrateConfig(path); err == nil {
		t.Fatal("conflicting settings overwritten")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatal("conflict changed bootstrap")
	}
	if err := os.Remove(settings); err != nil {
		t.Fatal(err)
	}
	if err := MigrateConfig(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(settings); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("missing sibling settings accepted")
	}
	for _, databasePath := range []string{"./New-Data/db.sqlite", ""} {
		content := fmt.Sprintf("accounts:\n  totp_enabled: false\ndatabase:\n  path: %q\naudit:\n  prune_every_writes: 0\nextension:\n  untouched: false\n", databasePath)
		if err := os.WriteFile(settings, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		newConfig, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		expectedPath := databasePath
		if expectedPath == "" {
			expectedPath = Default().Database.Path
		}
		if newConfig.Database.Path != filepath.Join(dir, expectedPath) || newConfig.Accounts.TOTPEnabled || newConfig.Audit.PruneEveryWrites != 0 {
			t.Fatalf("new path/default semantics for database.path=%q: %+v", databasePath, newConfig)
		}
	}
	shaPath := filepath.Join(t.TempDir(), "config.yaml")
	c.Auth.Admin.PasswordHash = ""
	c.Auth.Admin.PasswordSHA256 = "2bb80d537b1da3e38bd30361aa855686bde0eacd7162fef6a25fe97bf527a25b"
	if err := SaveAtomic(shaPath, c); err != nil {
		t.Fatal(err)
	}
	if err := MigrateConfig(shaPath); err == nil {
		t.Fatal("SHA-only migration invented an Argon2 password")
	}
}
