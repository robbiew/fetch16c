package main

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func extractArchive(ctx context.Context, archivePath string, outputDir string, lhaPath string) error {
	extension := strings.ToLower(filepath.Ext(archivePath))
	switch extension {
	case ".zip":
		return extractZipArchive(archivePath, outputDir)
	case ".lha", ".lzh":
		if lhaPath == "" {
			return fmt.Errorf("lha command is required to extract %s archives", extension)
		}
		return extractLHAArchive(ctx, lhaPath, archivePath, outputDir)
	default:
		return fmt.Errorf("unsupported archive format: %s", extension)
	}
}

func extractZipArchive(archivePath string, outputDir string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("open ZIP archive: %w", err)
	}
	defer reader.Close()

	for _, file := range reader.File {
		if err := extractZIPFile(file, outputDir); err != nil {
			return fmt.Errorf("extract %q from ZIP archive: %w", file.Name, err)
		}
	}
	return nil
}

func extractLHAArchive(ctx context.Context, lhaPath string, archivePath string, outputDir string) error {
	command := exec.CommandContext(ctx, lhaPath, "xw="+outputDir, archivePath)
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("extract LHA archive: %s", message)
	}
	return nil
}

func extractZIPFile(file *zip.File, outputDir string) error {
	path := filepath.Join(outputDir, file.Name)
	relativePath, err := filepath.Rel(outputDir, path)
	if err != nil || relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("invalid file path")
	}

	if file.FileInfo().IsDir() {
		return os.MkdirAll(path, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	reader, err := file.Open()
	if err != nil {
		return err
	}
	defer reader.Close()

	writer, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(writer, reader)
	closeErr := writer.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
