package main

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"filetrans-backend/internal/security"
)

func TestRunFormats(t *testing.T) {
	for _, format := range []string{"yaml", "phc"} {
		var out bytes.Buffer
		if err := run([]string{"--format", format}, strings.NewReader("secret\n"), &out); err != nil {
			t.Fatalf("run %s: %v", format, err)
		}
		phc := strings.TrimSpace(out.String())
		if format == "yaml" {
			lines := strings.Split(phc, "\n")
			if len(lines) != 4 || strings.TrimSpace(lines[0]) != "auth:" || strings.TrimSpace(lines[1]) != "admin:" {
				t.Fatalf("unexpected YAML output: %q", out.String())
			}
			phc = strings.TrimPrefix(strings.TrimSpace(lines[2]), "password_hash: ")
			phc = strings.Trim(phc, `"`)
			legacy := strings.Trim(strings.TrimPrefix(strings.TrimSpace(lines[3]), "password_sha256: "), `"`)
			if legacy != "2bb80d537b1da3e38bd30361aa855686bde0eacd7162fef6a25fe97bf527a25b" {
				t.Fatalf("YAML rollback SHA does not match the input password: %q", legacy)
			}
		} else if strings.Contains(out.String(), "password_sha256") {
			t.Fatalf("phc format must not include rollback SHA: %q", out.String())
		}
		if ok, err := security.Verify(phc, []byte("secret")); err != nil || !ok {
			t.Fatalf("verify %s output: ok=%v err=%v output=%q", format, ok, err, out.String())
		}
		if strings.Contains(out.String(), "secret") {
			t.Fatalf("output leaked plaintext")
		}
	}
	var legacy bytes.Buffer
	if err := run([]string{"--format", "legacy-sha256"}, strings.NewReader("secret\n"), &legacy); err != nil {
		t.Fatalf("legacy format: %v", err)
	}
	if strings.TrimSpace(legacy.String()) != "2bb80d537b1da3e38bd30361aa855686bde0eacd7162fef6a25fe97bf527a25b" {
		t.Fatalf("unexpected legacy output: %q", legacy.String())
	}
}

func TestRunRejectsArgumentsAndOversizedPassword(t *testing.T) {
	if err := run([]string{"plaintext"}, strings.NewReader("ignored"), io.Discard); err == nil {
		t.Fatalf("expected positional password rejected")
	}
	if err := run(nil, strings.NewReader(strings.Repeat("x", 1025)), io.Discard); err == nil {
		t.Fatalf("expected oversized password rejected")
	}
}
