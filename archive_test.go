package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractArchiveRequiresLHACommandForLZH(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "pack.lzh")
	if err := os.WriteFile(archivePath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := extractArchive(context.Background(), archivePath, t.TempDir(), ""); err == nil || !strings.Contains(err.Error(), "lha command is required") {
		t.Fatalf("extractArchive error = %v", err)
	}
}

func TestExtractLHAArchiveIncludesCommandOutput(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "lha")
	script := "#!/bin/sh\necho 'unsupported LHA method' >&2\nexit 1\n"
	if err := os.WriteFile(executable, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(directory, "pack.lha")
	if err := os.WriteFile(archivePath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	err := extractLHAArchive(context.Background(), executable, archivePath, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "unsupported LHA method") {
		t.Fatalf("extractLHAArchive error = %v", err)
	}
}
