// Command fetch16c downloads art packs from 16colo.rs and extracts them into year and pack
// directories.
package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

// artExtensions lists the file types a BBS or terminal art viewer can show:
// ANSI, ASCII, and the common textmode art formats found in 16colo.rs packs.
var artExtensions = []string{
	"ans",  // ANSI art
	"ansi", // ANSI art (long extension)
	"asc",  // ASCII art
	"txt",  // plain text
	"nfo",  // release information text
	"diz",  // FILE_ID.DIZ descriptions
	"mem",  // member lists
	"lit",  // literature
	"ice",  // iCE color ANSI
	"pcb",  // PCBoard color codes
	"avt",  // Avatar
	"bin",  // raw binary screen dumps
	"xb",   // XBin
	"adf",  // Artworx
	"idf",  // iCE Draw
	"tnd",  // TundraDraw
	"rip",  // RIPscrip vector graphics
}

// fileFilter decides whether an extracted file should be kept. A nil filter keeps every file.
type fileFilter func(name string) bool

// extensionFilter keeps files whose extension appears in the list. Extensions are matched
// without regard to case and with or without a leading dot.
func extensionFilter(extensions []string) fileFilter {
	normalized := normalizeExtensions(extensions)
	if len(normalized) == 0 {
		return nil
	}
	return func(name string) bool {
		extension := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
		return slices.Contains(normalized, extension)
	}
}

func normalizeExtensions(extensions []string) []string {
	var normalized []string
	for _, extension := range extensions {
		for part := range strings.SplitSeq(extension, ",") {
			part = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(part)), ".")
			if part != "" && !slices.Contains(normalized, part) {
				normalized = append(normalized, part)
			}
		}
	}
	slices.Sort(normalized)
	return normalized
}

// externalExtractor describes a command-line tool that can unpack an archive format
// the Go standard library cannot read.
type externalExtractor struct {
	command string
	args    func(archivePath string, outputDir string) []string
}

func (extractor externalExtractor) String() string {
	return extractor.command
}

// extractResult describes what an extraction produced.
type extractResult struct {
	// Damaged lists archive members whose stored data is corrupt and were skipped.
	Damaged []string
}

var lhaExtractor = externalExtractor{
	command: "lha",
	args: func(archivePath string, outputDir string) []string {
		return []string{"xw=" + outputDir, archivePath}
	},
}

var arjExtractor = externalExtractor{
	command: "arj",
	args: func(archivePath string, outputDir string) []string {
		return []string{"x", "-y", archivePath, outputDir + string(os.PathSeparator)}
	},
}

func sevenZipExtractor(command string) externalExtractor {
	return externalExtractor{
		command: command,
		args: func(archivePath string, outputDir string) []string {
			return []string{"x", "-y", "-o" + outputDir, archivePath}
		},
	}
}

var unarExtractor = externalExtractor{
	command: "unar",
	args: func(archivePath string, outputDir string) []string {
		return []string{"-quiet", "-force-overwrite", "-no-directory", "-output-directory", outputDir, archivePath}
	},
}

// externalExtractors maps an archive extension to the tools that can unpack it, in order of preference.
var externalExtractors = map[string][]externalExtractor{
	".lha": {lhaExtractor, sevenZipExtractor("7z"), sevenZipExtractor("7zz"), sevenZipExtractor("7za"), unarExtractor},
	".lzh": {lhaExtractor, sevenZipExtractor("7z"), sevenZipExtractor("7zz"), sevenZipExtractor("7za"), unarExtractor},
	".arj": {arjExtractor, sevenZipExtractor("7z"), sevenZipExtractor("7zz"), sevenZipExtractor("7za"), unarExtractor},
}

// supportedArchiveExtensions lists every archive format fetch16c knows how to unpack,
// either natively or through an external tool.
var supportedArchiveExtensions = []string{".zip", ".lha", ".lzh", ".arj"}

func isSupportedArchiveExtension(extension string) bool {
	return slices.Contains(supportedArchiveExtensions, strings.ToLower(extension))
}

// extractorCommands names the tools that can unpack the given extension, for error messages.
func extractorCommands(extension string) []string {
	var commands []string
	for _, extractor := range externalExtractors[strings.ToLower(extension)] {
		if !slices.Contains(commands, extractor.command) {
			commands = append(commands, extractor.command)
		}
	}
	return commands
}

// resolvedExtractor is an external extractor whose command was found on this system.
type resolvedExtractor struct {
	externalExtractor
	path string
}

// findExtractor locates the first available tool for the extension. It returns nil, nil for
// ZIP archives, which are extracted natively, and an error when no tool is installed.
func findExtractor(lookPath func(string) (string, error), extension string) (*resolvedExtractor, error) {
	extension = strings.ToLower(extension)
	if extension == ".zip" {
		return nil, nil
	}
	candidates, ok := externalExtractors[extension]
	if !ok {
		return nil, fmt.Errorf("unsupported archive format %s", extension)
	}
	for _, candidate := range candidates {
		path, err := lookPath(candidate.command)
		if err == nil {
			return &resolvedExtractor{externalExtractor: candidate, path: path}, nil
		}
	}
	return nil, fmt.Errorf("no extractor found for %s archives; install one of: %s", extension, strings.Join(extractorCommands(extension), ", "))
}

// extractArchive unpacks archivePath into outputDir, keeping only files accepted by filter.
// Non-ZIP archives require an extractor resolved by findExtractor. ZIP members whose data is
// corrupt in the archive itself are skipped and reported in the result.
func extractArchive(ctx context.Context, archivePath string, outputDir string, extractor *resolvedExtractor, filter fileFilter) (extractResult, error) {
	var result extractResult
	extension := strings.ToLower(filepath.Ext(archivePath))
	if extension == ".zip" {
		damaged, err := extractZipArchive(archivePath, outputDir, filter)
		if err != nil {
			return result, err
		}
		result.Damaged = damaged
		return result, removeEmptyDirectories(outputDir)
	}
	if !isSupportedArchiveExtension(extension) {
		return result, fmt.Errorf("unsupported archive format: %s", extension)
	}
	if extractor == nil {
		return result, fmt.Errorf("no extractor available for %s archives; install one of: %s", extension, strings.Join(extractorCommands(extension), ", "))
	}
	if err := runExternalExtractor(ctx, *extractor, archivePath, outputDir); err != nil {
		return result, err
	}
	if err := sanitizeExtracted(outputDir, filter); err != nil {
		return result, err
	}
	return result, removeEmptyDirectories(outputDir)
}

func runExternalExtractor(ctx context.Context, extractor resolvedExtractor, archivePath string, outputDir string) error {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create extraction directory: %w", err)
	}
	command := exec.CommandContext(ctx, extractor.path, extractor.args(archivePath, outputDir)...)
	command.Dir = outputDir
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		if ctx.Err() != nil {
			return fmt.Errorf("extract archive with %s: %w", extractor.command, ctx.Err())
		}
		return fmt.Errorf("extract archive with %s: %s", extractor.command, message)
	}
	return nil
}

// sanitizeExtracted removes symbolic links produced by an external tool and, when a filter is
// set, every file the filter rejects.
func sanitizeExtracted(outputDir string, filter fileFilter) error {
	return filepath.WalkDir(outputDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == outputDir || entry.IsDir() {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 || !entry.Type().IsRegular() || (filter != nil && !filter(entry.Name())) {
			return os.Remove(path)
		}
		return nil
	})
}

// removeEmptyDirectories prunes empty subdirectories left behind by filtering, bottom-up.
// The root itself is never removed.
func removeEmptyDirectories(root string) error {
	var directories []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && path != root {
			directories = append(directories, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	slices.Reverse(directories)
	for _, directory := range directories {
		entries, err := os.ReadDir(directory)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			if err := os.Remove(directory); err != nil {
				return err
			}
		}
	}
	return nil
}

// extractZipArchive unpacks a ZIP archive and returns the names of members whose compressed
// data is corrupt. Such members are skipped so the rest of a decades-old pack is still usable.
func extractZipArchive(archivePath string, outputDir string, filter fileFilter) ([]string, error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, fmt.Errorf("open ZIP archive: %w", err)
	}
	defer reader.Close()

	var damaged []string
	for _, file := range reader.File {
		err := extractZIPFile(file, outputDir, filter)
		if err == nil {
			continue
		}
		if isDamagedEntry(err) {
			damaged = append(damaged, file.Name)
			continue
		}
		return damaged, fmt.Errorf("extract %q from ZIP archive: %w", file.Name, err)
	}
	return damaged, nil
}

// isDamagedEntry reports whether err means the archive's stored data for one member is corrupt,
// as opposed to a problem with the local file system or an unsupported feature.
func isDamagedEntry(err error) bool {
	var corruptFlate flate.CorruptInputError
	return errors.Is(err, zip.ErrChecksum) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, errCorruptLegacyZip) || errors.As(err, &corruptFlate)
}

// zipEntryName returns the archive member name as a slash-separated path. DOS-era packs often
// store backslash separators and code page 437 bytes, which this converts to UTF-8.
func zipEntryName(file *zip.File) string {
	name := strings.ReplaceAll(file.Name, `\`, "/")
	if !utf8.ValidString(name) {
		if decoded, err := charmap.CodePage437.NewDecoder().String(name); err == nil {
			name = decoded
		}
	}
	return name
}

// openZIPEntry returns the decompressed contents of a ZIP entry, handling the PKZIP 1.x
// Shrink and Implode methods that archive/zip does not support.
func openZIPEntry(file *zip.File) (io.ReadCloser, error) {
	switch file.Method {
	case zipMethodShrink, zipMethodImplode:
	default:
		return file.Open()
	}
	raw, err := file.OpenRaw()
	if err != nil {
		return nil, err
	}
	var data []byte
	if file.Method == zipMethodShrink {
		data, err = unshrink(bufio.NewReader(raw), file.UncompressedSize64)
	} else {
		data, err = explode(bufio.NewReader(raw), file.Flags, file.UncompressedSize64)
	}
	if err != nil {
		return nil, err
	}
	if crc32.ChecksumIEEE(data) != file.CRC32 {
		return nil, zip.ErrChecksum
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func extractZIPFile(file *zip.File, outputDir string, filter fileFilter) error {
	name := zipEntryName(file)
	path := filepath.Join(outputDir, filepath.FromSlash(name))
	if err := ensureContainedPath(outputDir, path); err != nil {
		return errors.New("invalid file path")
	}

	mode := file.Mode()
	if file.FileInfo().IsDir() || strings.HasSuffix(name, "/") {
		return os.MkdirAll(path, 0o755)
	}
	if !mode.IsRegular() {
		// Symbolic links and other special entries are never materialized.
		return nil
	}
	if filter != nil && !filter(filepath.Base(path)) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	reader, err := openZIPEntry(file)
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
		// Leave no partial file behind for a member whose data is corrupt.
		_ = os.Remove(path)
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if modified := file.Modified; !modified.IsZero() && modified.Unix() > 0 {
		if err := os.Chtimes(path, modified, modified); err != nil {
			return err
		}
	}
	return nil
}
