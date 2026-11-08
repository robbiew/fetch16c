package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExtractZipArchiveWithAbsoluteOutputPath(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "pack.zip")
	createZipArchive(t, archivePath, "nested/art.ans")

	outputDir := t.TempDir()
	if _, err := extractZipArchive(archivePath, outputDir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outputDir, "nested", "art.ans")); err != nil {
		t.Fatal(err)
	}
}

func TestExtractZipArchiveRejectsTraversal(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "pack.zip")
	createZipArchive(t, archivePath, "../escaped.ans")

	if _, err := extractZipArchive(archivePath, t.TempDir(), nil); err == nil {
		t.Fatal("extractZipArchive accepted a path outside the output directory")
	}
}

func TestExtractZipArchiveNormalizesDOSNames(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "pack.zip")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	// Code page 437 bytes for "ÄRT" with a backslash separator, as DOS-era packers wrote them.
	header := &zip.FileHeader{Name: "SUB\\\x8eRT.ANS", Method: zip.Deflate, NonUTF8: true}
	entry, err := archive.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("art")); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	outputDir := t.TempDir()
	if _, err := extractZipArchive(archivePath, outputDir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outputDir, "SUB", "ÄRT.ANS")); err != nil {
		t.Fatal(err)
	}
}

func TestExtractZipArchivePreservesModificationTime(t *testing.T) {
	modified := time.Date(1996, time.June, 15, 12, 30, 0, 0, time.UTC)
	archivePath := filepath.Join(t.TempDir(), "pack.zip")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	header := &zip.FileHeader{Name: "art.ans", Method: zip.Deflate, Modified: modified}
	entry, err := archive.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("art")); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	outputDir := t.TempDir()
	if _, err := extractZipArchive(archivePath, outputDir, nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(outputDir, "art.ans"))
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().UTC().Truncate(2*time.Second) != modified.Truncate(2*time.Second) {
		t.Fatalf("mtime = %s, want %s", info.ModTime().UTC(), modified)
	}
}

func TestExtractZipArchiveSkipsSymlinks(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "pack.zip")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	header := &zip.FileHeader{Name: "link.ans", Method: zip.Store}
	header.SetMode(os.ModeSymlink | 0o777)
	entry, err := archive.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("/etc/passwd")); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	outputDir := t.TempDir()
	if _, err := extractZipArchive(archivePath, outputDir, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(outputDir, "link.ans")); !os.IsNotExist(err) {
		t.Fatalf("symlink entry was materialized: %v", err)
	}
}

func TestExtractArchiveAppliesFilterAndPrunesEmptyDirectories(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "pack.zip")
	createZipArchive(t, archivePath, "ART.ANS", "FILE_ID.DIZ", "VIEWER.EXE", "music/track.xm", "text/notes.txt")

	outputDir := t.TempDir()
	if _, err := extractArchive(context.Background(), archivePath, outputDir, nil, extensionFilter(artExtensions)); err != nil {
		t.Fatal(err)
	}
	if got := listFiles(t, outputDir); !reflect.DeepEqual(got, []string{"ART.ANS", "FILE_ID.DIZ", "text/notes.txt"}) {
		t.Fatalf("extracted files = %v", got)
	}
	if _, err := os.Stat(filepath.Join(outputDir, "music")); !os.IsNotExist(err) {
		t.Fatal("empty directory was not pruned")
	}
}

func TestExtractArchiveRejectsUnsupportedFormats(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "pack.rar")
	if err := os.WriteFile(archivePath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := extractArchive(context.Background(), archivePath, t.TempDir(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported archive format") {
		t.Fatalf("extractArchive error = %v", err)
	}
}

func TestExtractArchiveRequiresExtractorForLHA(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "pack.lzh")
	if err := os.WriteFile(archivePath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := extractArchive(context.Background(), archivePath, t.TempDir(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "install one of: lha, 7z, 7zz, 7za, unar") {
		t.Fatalf("extractArchive error = %v", err)
	}
}

func TestFindExtractorPrefersLHAThenFallsBack(t *testing.T) {
	available := map[string]string{"unar": "/opt/bin/unar", "7zz": "/opt/bin/7zz"}
	lookPath := func(command string) (string, error) {
		if path, ok := available[command]; ok {
			return path, nil
		}
		return "", errors.New("not found")
	}
	extractor, err := findExtractor(lookPath, ".LHA")
	if err != nil {
		t.Fatal(err)
	}
	if extractor.command != "7zz" || extractor.path != "/opt/bin/7zz" {
		t.Fatalf("findExtractor chose %+v", extractor)
	}
	if extractor, err := findExtractor(lookPath, ".zip"); err != nil || extractor != nil {
		t.Fatalf("findExtractor(.zip) = %v, %v", extractor, err)
	}
	if _, err := findExtractor(lookPath, ".rar"); err == nil {
		t.Fatal("findExtractor accepted an unknown format")
	}
	if _, err := findExtractor(func(string) (string, error) { return "", errors.New("missing") }, ".arj"); err == nil || !strings.Contains(err.Error(), "arj, 7z") {
		t.Fatalf("findExtractor(.arj) error = %v", err)
	}
}

func TestExternalExtractorArguments(t *testing.T) {
	tests := []struct {
		extractor externalExtractor
		want      []string
	}{
		{lhaExtractor, []string{"xw=/out", "/in/pack.lha"}},
		{sevenZipExtractor("7z"), []string{"x", "-y", "-o/out", "/in/pack.lha"}},
		{unarExtractor, []string{"-quiet", "-force-overwrite", "-no-directory", "-output-directory", "/out", "/in/pack.lha"}},
		{arjExtractor, []string{"x", "-y", "/in/pack.lha", "/out" + string(os.PathSeparator)}},
	}
	for _, test := range tests {
		if got := test.extractor.args("/in/pack.lha", "/out"); !reflect.DeepEqual(got, test.want) {
			t.Errorf("%s args = %v, want %v", test.extractor.command, got, test.want)
		}
	}
}

func TestRunExternalExtractorPassesArgumentsAndReportsOutput(t *testing.T) {
	skipWithoutShell(t)
	directory := t.TempDir()
	capturePath := filepath.Join(directory, "arguments")
	executable := writeScript(t, directory, "lha", "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CAPTURE_PATH\"\n")
	t.Setenv("CAPTURE_PATH", capturePath)

	archivePath := filepath.Join(directory, "pack.lha")
	outputDir := filepath.Join(directory, "output")
	extractor := resolvedExtractor{externalExtractor: lhaExtractor, path: executable}
	if err := runExternalExtractor(context.Background(), extractor, archivePath, outputDir); err != nil {
		t.Fatal(err)
	}
	arguments, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatal(err)
	}
	if want := "xw=" + outputDir + "\n" + archivePath + "\n"; string(arguments) != want {
		t.Fatalf("lha arguments = %q, want %q", arguments, want)
	}

	failing := writeScript(t, directory, "lha-fail", "#!/bin/sh\necho 'unsupported LHA method' >&2\nexit 1\n")
	err = runExternalExtractor(context.Background(), resolvedExtractor{externalExtractor: lhaExtractor, path: failing}, archivePath, outputDir)
	if err == nil || !strings.Contains(err.Error(), "unsupported LHA method") || !strings.Contains(err.Error(), "with lha") {
		t.Fatalf("runExternalExtractor error = %v", err)
	}
}

func TestExtractArchiveSanitizesExternalOutput(t *testing.T) {
	skipWithoutShell(t)
	directory := t.TempDir()
	// A fake extractor that writes an art file, an executable, a nested file, and a symlink.
	script := "#!/bin/sh\nfor arg in \"$@\"; do case \"$arg\" in xw=*) dir=\"${arg#xw=}\";; esac; done\n" +
		"mkdir -p \"$dir/sub\" \"$dir/empty\"\n" +
		"echo art > \"$dir/ART.ANS\"\necho exe > \"$dir/VIEW.EXE\"\necho nfo > \"$dir/sub/INFO.NFO\"\nln -s /etc/passwd \"$dir/link.ans\"\n"
	executable := writeScript(t, directory, "lha", script)
	archivePath := filepath.Join(directory, "pack.lha")
	if err := os.WriteFile(archivePath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	outputDir := filepath.Join(directory, "output")
	extractor := &resolvedExtractor{externalExtractor: lhaExtractor, path: executable}
	if _, err := extractArchive(context.Background(), archivePath, outputDir, extractor, extensionFilter(artExtensions)); err != nil {
		t.Fatal(err)
	}
	if got := listFiles(t, outputDir); !reflect.DeepEqual(got, []string{"ART.ANS", "sub/INFO.NFO"}) {
		t.Fatalf("extracted files = %v", got)
	}
	if _, err := os.Lstat(filepath.Join(outputDir, "link.ans")); !os.IsNotExist(err) {
		t.Fatal("symlink from external extractor was kept")
	}
	if _, err := os.Stat(filepath.Join(outputDir, "empty")); !os.IsNotExist(err) {
		t.Fatal("empty directory was kept")
	}
}

func TestNormalizeExtensions(t *testing.T) {
	got := normalizeExtensions([]string{".ANS, txt", "nfo", "", "ans", " .Diz "})
	if want := []string{"ans", "diz", "nfo", "txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeExtensions = %v, want %v", got, want)
	}
	if extensionFilter(nil) != nil || extensionFilter([]string{" , "}) != nil {
		t.Fatal("empty extension lists produced a filter")
	}
}

func skipWithoutShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake extractors are shell scripts")
	}
}

func writeScript(t *testing.T, directory string, name string, script string) string {
	t.Helper()
	executable := filepath.Join(directory, name)
	if err := os.WriteFile(executable, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return executable
}

func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(relative))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func createZipArchive(t *testing.T, archivePath string, entryNames ...string) {
	t.Helper()

	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for _, entryName := range entryNames {
		entry, err := archive.Create(entryName)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte("ANSI art")); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestZipCorpus extracts every ZIP archive under FETCH16C_ZIP_CORPUS, recursively, and fails
// on the first archive that cannot be fully extracted. It exercises the legacy Shrink and
// Implode decoders against real packs.
func TestZipCorpus(t *testing.T) {
	corpus := os.Getenv("FETCH16C_ZIP_CORPUS")
	if corpus == "" {
		t.Skip("set FETCH16C_ZIP_CORPUS to a directory of ZIP archives")
	}
	var archives []string
	err := filepath.WalkDir(corpus, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && strings.EqualFold(filepath.Ext(path), ".zip") {
			archives = append(archives, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	legacy := 0
	for _, archivePath := range archives {
		reader, err := zip.OpenReader(archivePath)
		if err != nil {
			t.Logf("skipping unreadable %s: %v", archivePath, err)
			continue
		}
		uses := false
		for _, file := range reader.File {
			if file.Method == zipMethodShrink || file.Method == zipMethodImplode {
				uses = true
			}
		}
		reader.Close()
		if !uses {
			continue
		}
		legacy++
		if damaged, err := extractZipArchive(archivePath, t.TempDir(), nil); err != nil || len(damaged) > 0 {
			t.Errorf("%s: damaged=%v error=%v", archivePath, damaged, err)
		}
	}
	t.Logf("%d of %d archives use legacy methods", legacy, len(archives))
}

func TestExtractZipArchiveLegacyMethods(t *testing.T) {
	outputDir := t.TempDir()
	damaged, err := extractZipArchive(filepath.Join("testdata", "legacy-methods.zip"), outputDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(damaged) != 0 {
		t.Fatalf("damaged = %v", damaged)
	}
	sizes := map[string]int{"implode-4k-2trees.ans": 549, "implode-8k-3trees.ans": 6182, "shrink.ans": 156}
	for name, size := range sizes {
		data, err := os.ReadFile(filepath.Join(outputDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if len(data) != size {
			t.Fatalf("%s is %d bytes, want %d", name, len(data), size)
		}
		if !bytes.Contains(data, []byte("\x1b[")) {
			t.Fatalf("%s does not look like ANSI art", name)
		}
	}
}

func TestExtractZipArchiveSkipsDamagedMembers(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "pack.zip")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	good, err := archive.Create("GOOD.ANS")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := good.Write([]byte("ANSI art")); err != nil {
		t.Fatal(err)
	}
	// A stored member whose recorded CRC does not match its data.
	header := &zip.FileHeader{Name: "BAD.GIF", Method: zip.Store, CRC32: 0xDEADBEEF, CompressedSize64: 4, UncompressedSize64: 4}
	bad, err := archive.CreateRaw(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Write([]byte("GIF8")); err != nil {
		t.Fatal(err)
	}
	// A deflate member whose compressed data is garbage.
	header = &zip.FileHeader{Name: "TRUNC.MOD", Method: zip.Deflate, CRC32: 1, CompressedSize64: 3, UncompressedSize64: 100}
	truncated, err := archive.CreateRaw(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := truncated.Write([]byte{0xff, 0xff, 0xff}); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	outputDir := t.TempDir()
	damaged, err := extractZipArchive(archivePath, outputDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(damaged, []string{"BAD.GIF", "TRUNC.MOD"}) {
		t.Fatalf("damaged = %v", damaged)
	}
	if got := listFiles(t, outputDir); !reflect.DeepEqual(got, []string{"GOOD.ANS"}) {
		t.Fatalf("extracted files = %v", got)
	}
}
