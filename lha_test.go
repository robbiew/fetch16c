package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractLHAArchiveUsesWorkingDirectoryOption(t *testing.T) {
	directory := t.TempDir()
	capturePath := filepath.Join(directory, "arguments")
	executable := filepath.Join(directory, "lha")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CAPTURE_PATH\"\n"
	if err := os.WriteFile(executable, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CAPTURE_PATH", capturePath)

	archivePath := filepath.Join(directory, "pack.lha")
	outputDir := filepath.Join(directory, "output")
	if err := extractLHAArchive(context.Background(), executable, archivePath, outputDir); err != nil {
		t.Fatal(err)
	}
	arguments, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{"xw=" + outputDir, archivePath, ""}, "\n")
	if string(arguments) != want {
		t.Fatalf("lha arguments = %q, want %q", arguments, want)
	}
}
