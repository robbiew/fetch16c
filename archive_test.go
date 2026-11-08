package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
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

// bitWriter packs bits least significant first, mirroring bitReader.
type bitWriter struct {
	data  []byte
	bits  uint32
	count uint
}

func (writer *bitWriter) write(value uint32, count uint) {
	writer.bits |= value << writer.count
	writer.count += count
	for writer.count >= 8 {
		writer.data = append(writer.data, byte(writer.bits))
		writer.bits >>= 8
		writer.count -= 8
	}
}

// writeCode writes a Shannon-Fano code, whose first bit in the stream is its most significant.
func (writer *bitWriter) writeCode(value uint32, length uint) {
	for shift := length; shift > 0; shift-- {
		writer.write(value>>(shift-1)&1, 1)
	}
}

func (writer *bitWriter) bytes() []byte {
	if writer.count > 0 {
		return append(writer.data, byte(writer.bits))
	}
	return writer.data
}

// shrinkCompress is a minimal PKZIP Shrink encoder: LZW with explicit code-size increases.
func shrinkCompress(data []byte) []byte {
	writer := &bitWriter{}
	table := make(map[string]uint32, 8192)
	for code := range 256 {
		table[string([]byte{byte(code)})] = uint32(code)
	}
	nextCode := uint32(257)
	codeSize := uint(9)
	current := ""
	emit := func(code uint32) {
		writer.write(code, codeSize)
	}
	for _, next := range data {
		candidate := current + string([]byte{next})
		if _, ok := table[candidate]; ok {
			current = candidate
			continue
		}
		emit(table[current])
		if nextCode < 8192 {
			if nextCode >= 1<<codeSize {
				emit(256)
				emit(1)
				codeSize++
			}
			table[candidate] = nextCode
			nextCode++
		}
		current = string([]byte{next})
	}
	if current != "" {
		emit(table[current])
	}
	return writer.bytes()
}

func TestUnshrinkRoundTrip(t *testing.T) {
	var plain []byte
	for index := range 6000 {
		plain = append(plain, []byte(fmt.Sprintf("line %d of ANSI text \x1b[1;3%dm\n", index%97, index%8))...)
	}
	compressed := shrinkCompress(plain)
	if len(compressed) >= len(plain) {
		t.Fatalf("shrink did not compress: %d >= %d", len(compressed), len(plain))
	}
	got, err := unshrink(bytes.NewReader(compressed), uint64(len(plain)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("unshrink output differs from input")
	}
	if _, err := unshrink(bytes.NewReader(compressed[:len(compressed)/2]), uint64(len(plain))); err == nil {
		t.Fatal("unshrink accepted truncated data")
	}
}

// flatTree describes a Shannon-Fano tree where every one of count symbols has the same code
// length, which makes codes trivial: symbol s gets code count-1-s.
func flatTree(count int, length uint) []byte {
	tree := []byte{byte(count/16 - 1)}
	for range count / 16 {
		tree = append(tree, byte(15<<4|(length-1)))
	}
	return tree
}

// implodeCompress encodes data with literal runs and matches using flat trees. Matches are
// given as (position, distance, length) and must not overlap the preceding literal run.
func implodeCompress(data []byte, flags uint16, matches [][3]int) []byte {
	var out []byte
	literalTree := flags&implodeFlag3Trees != 0
	minimumMatch := 2
	if literalTree {
		out = append(out, flatTree(256, 8)...)
		minimumMatch = 3
	}
	out = append(out, flatTree(64, 6)...) // length tree
	out = append(out, flatTree(64, 6)...) // distance tree
	distanceBits := uint(6)
	if flags&implodeFlag8K != 0 {
		distanceBits = 7
	}
	writer := &bitWriter{}
	position := 0
	for _, match := range matches {
		for ; position < match[0]; position++ {
			writer.write(1, 1)
			if literalTree {
				writer.writeCode(uint32(255-int(data[position])), 8)
			} else {
				writer.write(uint32(data[position]), 8)
			}
		}
		distance, length := match[1]-1, match[2]-minimumMatch
		writer.write(0, 1)
		writer.write(uint32(distance)&(1<<distanceBits-1), distanceBits)
		writer.writeCode(uint32(63-(distance>>distanceBits)), 6)
		if length >= 63 {
			writer.writeCode(0, 6) // symbol 63
			writer.write(uint32(length-63), 8)
		} else {
			writer.writeCode(uint32(63-length), 6)
		}
		position += match[2]
	}
	for ; position < len(data); position++ {
		writer.write(1, 1)
		if literalTree {
			writer.writeCode(uint32(255-int(data[position])), 8)
		} else {
			writer.write(uint32(data[position]), 8)
		}
	}
	return append(out, writer.bytes()...)
}

func TestExplodeDecodesAllModes(t *testing.T) {
	prefix := []byte("\x1b[0;36mBBS ART PACK ")
	var plain []byte
	for range 40 {
		plain = append(plain, prefix...)
	}
	plain = append(plain, bytes.Repeat([]byte{'='}, 300)...)
	tail := []byte(" the end\n")
	plain = append(plain, tail...)
	// A literal prefix, three long matches that repeat it (one using the 8-bit length
	// extension), then a literal '=' followed by an overlapping run match.
	matches := [][3]int{
		{len(prefix), len(prefix), 300},
		{len(prefix) + 300, len(prefix), 300},
		{len(prefix) + 600, len(prefix), len(prefix)*40 - len(prefix) - 600},
		{len(prefix)*40 + 1, 1, 299},
	}

	for _, flags := range []uint16{0, implodeFlag8K, implodeFlag3Trees, implodeFlag8K | implodeFlag3Trees} {
		compressed := implodeCompress(plain, flags, matches)
		got, err := explode(bytes.NewReader(compressed), flags, uint64(len(plain)))
		if err != nil {
			t.Fatalf("flags %#x: %v", flags, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("flags %#x: output differs from input", flags)
		}
		if _, err := explode(bytes.NewReader(compressed[:len(compressed)-20]), flags, uint64(len(plain))); err == nil {
			t.Fatalf("flags %#x: explode accepted truncated data", flags)
		}
	}
}

func TestExtractZipArchiveLegacyMethods(t *testing.T) {
	unit := []byte("\x1b[1;31mANSI\x1b[0m\n")
	plain := bytes.Repeat(unit, 50)
	shrunk := shrinkCompress(plain)
	// Matches are limited to 63+255 plus the minimum length, so repeat the unit in chunks.
	var matches [][3]int
	for position := len(unit); position < len(plain); position += 300 {
		matches = append(matches, [3]int{position, len(unit), min(300, len(plain)-position)})
	}
	imploded := implodeCompress(plain, implodeFlag8K|implodeFlag3Trees, matches)

	archivePath := filepath.Join(t.TempDir(), "legacy.zip")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for name, member := range map[string]struct {
		method uint16
		flags  uint16
		data   []byte
	}{
		"SHRUNK.ANS":   {zipMethodShrink, 0, shrunk},
		"IMPLODED.ANS": {zipMethodImplode, implodeFlag8K | implodeFlag3Trees, imploded},
	} {
		header := &zip.FileHeader{
			Name:               name,
			Method:             member.method,
			Flags:              member.flags,
			CRC32:              crc32.ChecksumIEEE(plain),
			CompressedSize64:   uint64(len(member.data)),
			UncompressedSize64: uint64(len(plain)),
		}
		raw, err := archive.CreateRaw(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := raw.Write(member.data); err != nil {
			t.Fatal(err)
		}
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
	if len(damaged) != 0 {
		t.Fatalf("damaged = %v", damaged)
	}
	for _, name := range []string{"SHRUNK.ANS", "IMPLODED.ANS"} {
		data, err := os.ReadFile(filepath.Join(outputDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, plain) {
			t.Fatalf("%s content differs", name)
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
