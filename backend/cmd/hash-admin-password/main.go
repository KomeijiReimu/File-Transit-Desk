package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"filetrans-backend/internal/security"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	flags := flag.NewFlagSet("hash-admin-password", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	format := flags.String("format", "yaml", "output format: yaml, phc, or legacy-sha256")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("password must be provided on stdin")
	}
	input, err := io.ReadAll(io.LimitReader(stdin, 1026))
	if err != nil {
		return err
	}
	input = []byte(strings.TrimSuffix(strings.TrimSuffix(string(input), "\n"), "\r"))
	if len(input) == 0 {
		return fmt.Errorf("password must not be empty")
	}
	if len(input) > 1024 {
		return fmt.Errorf("password must not exceed 1024 bytes")
	}
	switch *format {
	case "yaml", "phc":
		phc, err := security.Hash(input)
		if err != nil {
			return err
		}
		if *format == "yaml" {
			sum := sha256.Sum256(input)
			_, err = fmt.Fprintf(stdout, "auth:\n  admin:\n    password_hash: %q\n    password_sha256: %q\n", phc, hex.EncodeToString(sum[:]))
		} else {
			_, err = fmt.Fprintln(stdout, phc)
		}
		return err
	case "legacy-sha256":
		sum := sha256.Sum256(input)
		_, err := fmt.Fprintln(stdout, hex.EncodeToString(sum[:]))
		return err
	default:
		return fmt.Errorf("unsupported format %q", *format)
	}
}
