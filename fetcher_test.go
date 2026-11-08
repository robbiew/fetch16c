package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFetcherFetchesSkipsAndOverwritesPack(t *testing.T) {
	archive := zipBytes(t, "art.ans", "ANSI art")
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/year/2026":
			writeAPIResponse(t, writer, []Result{{Year: 2026, Name: "pack", Gallery: "https://16colo.rs/pack/pack", Groups: []string{"grp"}, Download: server.URL + "/archive/2026/pack.zip"}})
		case "/archive/2026/pack.zip":
			_, _ = writer.Write(archive)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	root := t.TempDir()
	config := testConfig(root)
	client := newTestClient(t, server.URL+"/")
	fetcher := NewFetcher(client, config, io.Discard)

	summary, err := fetcher.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Fetched != 1 || summary.Skipped != 0 || summary.Failed != 0 {
		t.Fatalf("first summary = %+v", summary)
	}
	packDir := filepath.Join(root, "2026", "pack")
	if _, err := os.Stat(filepath.Join(packDir, "art.ans")); err != nil {
		t.Fatal(err)
	}
	manifest, err := readManifest(packDir)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Version != manifestVersion || manifest.FileCount != 1 || manifest.ArchiveName != "pack.zip" || manifest.Gallery == "" || len(manifest.Groups) != 1 || manifest.Extensions != nil {
		t.Fatalf("manifest = %+v", manifest)
	}
	if _, err := os.Stat(filepath.Join(root, "2026", "pack-pack.zip")); !os.IsNotExist(err) {
		t.Fatal("archive was not removed after extraction")
	}

	summary, err = fetcher.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Skipped != 1 || summary.Fetched != 0 {
		t.Fatalf("second summary = %+v", summary)
	}

	config.Overwrite = true
	config.SkipExisting = false
	summary, err = NewFetcher(client, config, io.Discard).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Fetched != 1 {
		t.Fatalf("overwrite summary = %+v", summary)
	}
}

func TestFetcherRecordsDamagedMembers(t *testing.T) {
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	good, err := archive.Create("GOOD.ANS")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := good.Write([]byte("ANSI art")); err != nil {
		t.Fatal(err)
	}
	bad, err := archive.CreateRaw(&zip.FileHeader{Name: "BAD.GIF", Method: zip.Store, CRC32: 1, CompressedSize64: 3, UncompressedSize64: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Write([]byte("GIF")); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	server := newPackServer(t, buffer.Bytes(), "pack.zip")
	defer server.Close()

	var output bytes.Buffer
	config := testConfig(t.TempDir())
	config.Quiet = false
	summary, err := NewFetcher(newTestClient(t, server.URL+"/"), config, &output).Run(context.Background())
	if err != nil || summary.Fetched != 1 {
		t.Fatalf("summary = %+v, error = %v", summary, err)
	}
	manifest, err := readManifest(filepath.Join(config.Path, "2026", "pack"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(manifest.Damaged, []string{"BAD.GIF"}) || manifest.FileCount != 1 {
		t.Fatalf("manifest = %+v", manifest)
	}
	if !strings.Contains(output.String(), "skipped 1 damaged: BAD.GIF") {
		t.Fatalf("output = %s", output.String())
	}
}

func TestFetcherKeepsArchive(t *testing.T) {
	archive := zipBytes(t, "art.ans", "ANSI art")
	server := newPackServer(t, archive, "pack.zip")
	defer server.Close()

	config := testConfig(t.TempDir())
	config.KeepArchives = true
	client := newTestClient(t, server.URL+"/")
	summary, err := NewFetcher(client, config, io.Discard).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Fetched != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if _, err := os.Stat(filepath.Join(config.Path, "2026", "pack-pack.zip")); err != nil {
		t.Fatal(err)
	}
}

func TestFetcherExtractsOnlyArtFiles(t *testing.T) {
	archive := zipBytes(t, "ART.ANS", "ANSI art", "FILE_ID.DIZ", "desc", "VIEWER.EXE", "binary", "music/track.xm", "mod")
	server := newPackServer(t, archive, "pack.zip")
	defer server.Close()

	config := testConfig(t.TempDir())
	config.Extensions = normalizeExtensions(artExtensions)
	client := newTestClient(t, server.URL+"/")
	fetcher := NewFetcher(client, config, io.Discard)
	summary, err := fetcher.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Fetched != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	packDir := filepath.Join(config.Path, "2026", "pack")
	if got := listFiles(t, packDir); !reflect.DeepEqual(got, []string{manifestFilename, "ART.ANS", "FILE_ID.DIZ"}) {
		t.Fatalf("extracted files = %v", got)
	}
	manifest, err := readManifest(packDir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(manifest.Extensions, config.Extensions) || manifest.FileCount != 2 {
		t.Fatalf("manifest = %+v", manifest)
	}

	summary, err = fetcher.Run(context.Background())
	if err != nil || summary.Skipped != 1 {
		t.Fatalf("filtered rerun summary = %+v, error = %v", summary, err)
	}

	config.Extensions = nil
	summary, err = NewFetcher(client, config, io.Discard).Run(context.Background())
	if err == nil || summary.Failed != 1 || !strings.Contains(err.Error(), "different file filter") {
		t.Fatalf("unfiltered rerun summary = %+v, error = %v", summary, err)
	}
}

func TestFetcherReportsUnavailableAndUnsupportedPacksWithoutFailing(t *testing.T) {
	var downloads atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/year/2026":
			writeAPIResponse(t, writer, []Result{
				{Year: 2026, Name: "missing", Download: server.URL + "/archive/2026/"},
				{Year: 2026, Name: "blank", Download: ""},
				{Year: 2026, Name: "legacy", Download: server.URL + "/archive/2026/legacy.lha"},
				{Year: 2026, Name: "oldarj", Download: server.URL + "/archive/2026/oldarj.arj"},
				{Year: 2026, Name: "exotic", Download: server.URL + "/archive/2026/exotic.rar"},
			})
		default:
			downloads.Add(1)
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	var output bytes.Buffer
	config := testConfig(t.TempDir())
	config.Quiet = false
	fetcher := NewFetcher(newTestClient(t, server.URL+"/"), config, &output)
	fetcher.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	summary, err := fetcher.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Unavailable != 2 || summary.Unsupported != 3 || summary.Failed != 0 || summary.Fetched != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	if downloads.Load() != 0 {
		t.Fatalf("download count = %d, want 0", downloads.Load())
	}
	for _, want := range []string{"Unavailable 2026/missing", "Unsupported 2026/legacy (no extractor found for .lha archives", "Unsupported 2026/exotic (unsupported archive format"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, output.String())
		}
	}
}

func TestFetcherFallsBackToPackNamedArchive(t *testing.T) {
	archive := zipBytes(t, "art.ans", "ANSI art")
	var requested []string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/year/1996":
			writeAPIResponse(t, writer, []Result{
				{Year: 1996, Name: "joint06", Download: server.URL + "/archive/1996/joint#06.zip"},
				{Year: 1996, Name: "gone", Download: server.URL + "/archive/1996/gone!.zip"},
			})
		case "/archive/1996/joint06.zip":
			requested = append(requested, request.URL.Path)
			_, _ = writer.Write(archive)
		default:
			requested = append(requested, request.URL.Path)
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	config := testConfig(t.TempDir())
	config.Year = 1996
	client := newTestClient(t, server.URL+"/")
	summary, err := NewFetcher(client, config, io.Discard).Run(context.Background())
	if err != nil || summary.Fetched != 1 || summary.Unavailable != 1 || summary.Failed != 0 {
		t.Fatalf("summary = %+v, error = %v", summary, err)
	}
	want := []string{"/archive/1996/joint#06.zip", "/archive/1996/joint06.zip", "/archive/1996/gone!.zip", "/archive/1996/gone.zip"}
	if !reflect.DeepEqual(requested, want) {
		t.Fatalf("requested = %v, want %v", requested, want)
	}
	manifest, err := readManifest(filepath.Join(config.Path, "1996", "joint06"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(manifest.ArchiveURL, "/archive/1996/joint06.zip") || manifest.ArchiveName != "joint#06.zip" {
		t.Fatalf("manifest = %+v", manifest)
	}

	summary, err = NewFetcher(client, config, io.Discard).Run(context.Background())
	if err != nil || summary.Skipped != 1 || summary.Unavailable != 1 {
		t.Fatalf("rerun summary = %+v, error = %v", summary, err)
	}
}

func TestFetcherExtractsLHAWithFallbackExtractor(t *testing.T) {
	skipWithoutShell(t)
	directory := t.TempDir()
	script := "#!/bin/sh\nfor arg in \"$@\"; do case \"$arg\" in -o*) dir=\"${arg#-o}\";; esac; done\n" +
		"echo art > \"$dir/ART.ANS\"\necho exe > \"$dir/VIEW.EXE\"\n"
	executable := writeScript(t, directory, "7zz", script)

	server := newPackServer(t, []byte("fake lha"), "pack.lha")
	defer server.Close()

	config := testConfig(filepath.Join(directory, "art"))
	config.Extensions = normalizeExtensions(artExtensions)
	fetcher := NewFetcher(newTestClient(t, server.URL+"/"), config, io.Discard)
	fetcher.lookPath = func(command string) (string, error) {
		if command == "7zz" {
			return executable, nil
		}
		return "", errors.New("not found")
	}
	summary, err := fetcher.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Fetched != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if got := listFiles(t, filepath.Join(config.Path, "2026", "pack")); !reflect.DeepEqual(got, []string{manifestFilename, "ART.ANS"}) {
		t.Fatalf("extracted files = %v", got)
	}
}

func TestFetcherFiltersByPackPatternAndGroup(t *testing.T) {
	archive := zipBytes(t, "art.ans", "ANSI art")
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/year/2026":
			writeAPIResponse(t, writer, []Result{
				{Year: 2026, Name: "acid-01", Groups: []string{"acid"}, Download: server.URL + "/archive/2026/acid-01.zip"},
				{Year: 2026, Name: "acid-02", Groups: []string{"ACiD", "ice"}, Download: server.URL + "/archive/2026/acid-02.zip"},
				{Year: 2026, Name: "ice-01", Groups: []string{"ice"}, Download: server.URL + "/archive/2026/ice-01.zip"},
				{Year: 2026, Name: "solo", Download: server.URL + "/archive/2026/solo.zip"},
			})
		default:
			if strings.HasPrefix(request.URL.Path, "/archive/") {
				_, _ = writer.Write(archive)
				return
			}
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/")

	config := testConfig(t.TempDir())
	config.Packs = []string{"ACID-*", "Solo"}
	summary, err := NewFetcher(client, config, io.Discard).Run(context.Background())
	if err != nil || summary.Fetched != 3 {
		t.Fatalf("pattern summary = %+v, error = %v", summary, err)
	}
	if got := listDirs(t, filepath.Join(config.Path, "2026")); !reflect.DeepEqual(got, []string{"acid-01", "acid-02", "solo"}) {
		t.Fatalf("pattern dirs = %v", got)
	}

	config = testConfig(t.TempDir())
	config.Groups = []string{"ICE"}
	summary, err = NewFetcher(client, config, io.Discard).Run(context.Background())
	if err != nil || summary.Fetched != 2 {
		t.Fatalf("group summary = %+v, error = %v", summary, err)
	}
	if got := listDirs(t, filepath.Join(config.Path, "2026")); !reflect.DeepEqual(got, []string{"acid-02", "ice-01"}) {
		t.Fatalf("group dirs = %v", got)
	}

	config = testConfig(t.TempDir())
	config.Packs = []string{"solo", "nothing-*"}
	summary, err = NewFetcher(client, config, io.Discard).Run(context.Background())
	if err == nil || summary.Fetched != 1 || !strings.Contains(err.Error(), `pack "nothing-*" was not found`) {
		t.Fatalf("missing pattern summary = %+v, error = %v", summary, err)
	}
}

func TestFetcherSearchesEveryYearForPack(t *testing.T) {
	archive := zipBytes(t, "art.ans", "ANSI art")
	var queried []string
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/year/":
			_, _ = io.WriteString(writer, `{"1996":{"packs":1,"mags":0},"2026":{"packs":1,"mags":0}}`)
		case request.URL.Path == "/year/2026":
			queried = append(queried, "2026")
			writeAPIResponse(t, writer, []Result{{Year: 2026, Name: "modern", Download: server.URL + "/archive/2026/modern.zip"}})
		case request.URL.Path == "/year/1996":
			queried = append(queried, "1996")
			writeAPIResponse(t, writer, []Result{{Year: 1996, Name: "classic", Download: server.URL + "/archive/1996/classic.zip"}})
		case strings.HasPrefix(request.URL.Path, "/archive/"):
			_, _ = writer.Write(archive)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	config := testConfig(t.TempDir())
	config.Year = 0
	config.AllYears = true
	config.Packs = []string{"classic"}
	summary, err := NewFetcher(newTestClient(t, server.URL+"/"), config, io.Discard).Run(context.Background())
	if err != nil || summary.Fetched != 1 {
		t.Fatalf("summary = %+v, error = %v", summary, err)
	}
	if !reflect.DeepEqual(queried, []string{"2026", "1996"}) {
		t.Fatalf("years queried = %v", queried)
	}
	if _, err := os.Stat(filepath.Join(config.Path, "1996", "classic", "art.ans")); err != nil {
		t.Fatal(err)
	}
}

func TestFetcherFetchesMagazinesAndPacks(t *testing.T) {
	archive := zipBytes(t, "art.ans", "ANSI art")
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/year/2026" && request.URL.Query().Get("type") == TypeMags:
			writeAPIResponse(t, writer, []Result{{Year: 2026, Name: "mag", Download: server.URL + "/archive/2026/mag.zip"}})
		case request.URL.Path == "/year/2026":
			writeAPIResponse(t, writer, []Result{{Year: 2026, Name: "pack", Download: server.URL + "/archive/2026/pack.zip"}})
		case strings.HasPrefix(request.URL.Path, "/archive/"):
			_, _ = writer.Write(archive)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	config := testConfig(t.TempDir())
	config.Type = TypeAll
	summary, err := NewFetcher(newTestClient(t, server.URL+"/"), config, io.Discard).Run(context.Background())
	if err != nil || summary.Fetched != 2 {
		t.Fatalf("summary = %+v, error = %v", summary, err)
	}
	if got := listDirs(t, filepath.Join(config.Path, "2026")); !reflect.DeepEqual(got, []string{"mag", "pack"}) {
		t.Fatalf("dirs = %v", got)
	}
}

func TestFetcherDryRunDownloadsNothing(t *testing.T) {
	archive := zipBytes(t, "art.ans", "ANSI art")
	var downloads atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/year/2026":
			writeAPIResponse(t, writer, []Result{
				{Year: 2026, Name: "pack", Download: server.URL + "/archive/2026/pack.zip"},
				{Year: 2026, Name: "missing", Download: server.URL + "/archive/2026/"},
			})
		case "/archive/2026/pack.zip":
			downloads.Add(1)
			_, _ = writer.Write(archive)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	var output bytes.Buffer
	config := testConfig(t.TempDir())
	config.Quiet = false
	config.DryRun = true
	client := newTestClient(t, server.URL+"/")
	summary, err := NewFetcher(client, config, &output).Run(context.Background())
	if err != nil || summary.Fetched != 1 || summary.Unavailable != 1 {
		t.Fatalf("dry run summary = %+v, error = %v", summary, err)
	}
	if downloads.Load() != 0 {
		t.Fatalf("dry run downloaded %d archives", downloads.Load())
	}
	if !strings.Contains(output.String(), "Would fetch 2026/pack (pack.zip)") {
		t.Fatalf("dry run output = %s", output.String())
	}
	if entries, _ := os.ReadDir(config.Path); len(entries) != 0 {
		t.Fatalf("dry run created %d entries", len(entries))
	}

	config.DryRun = false
	if _, err := NewFetcher(client, config, io.Discard).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	config.DryRun = true
	output.Reset()
	summary, err = NewFetcher(client, config, &output).Run(context.Background())
	if err != nil || summary.Skipped != 1 || !strings.Contains(output.String(), "Would skip 2026/pack") {
		t.Fatalf("second dry run summary = %+v, error = %v, output = %s", summary, err, output.String())
	}
}

func TestFetcherCleansUpStagingLeftovers(t *testing.T) {
	archive := zipBytes(t, "art.ans", "ANSI art")
	server := newPackServer(t, archive, "pack.zip")
	defer server.Close()

	config := testConfig(t.TempDir())
	yearPath := filepath.Join(config.Path, "2026")
	if err := os.MkdirAll(filepath.Join(yearPath, stagingPrefix+"abc", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(yearPath, backupPrefix+"keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".pack.zip-123.part", "notes.part", "keep.txt"} {
		if err := os.WriteFile(filepath.Join(yearPath, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := NewFetcher(newTestClient(t, server.URL+"/"), config, io.Discard).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(yearPath)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if want := []string{backupPrefix + "keep", "keep.txt", "notes.part", "pack"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("year directory = %v, want %v", names, want)
	}
}

func TestFetcherUsesDistinctArchivePathsForSharedBasename(t *testing.T) {
	archive := zipBytes(t, "art.ans", "ANSI art")
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/year/2026":
			writeAPIResponse(t, writer, []Result{
				{Year: 2026, Name: "first", Download: server.URL + "/archive/2026/pack.zip"},
				{Year: 2026, Name: "second", Download: server.URL + "/archive/2026/pack.zip"},
			})
		case "/archive/2026/pack.zip":
			_, _ = writer.Write(archive)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	config := testConfig(t.TempDir())
	config.Concurrency = 2
	config.KeepArchives = true
	client := newTestClient(t, server.URL+"/")
	summary, err := NewFetcher(client, config, io.Discard).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Fetched != 2 {
		t.Fatalf("summary = %+v", summary)
	}
	for _, archiveName := range []string{"first-pack.zip", "second-pack.zip"} {
		if _, err := os.Stat(filepath.Join(config.Path, "2026", archiveName)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFetcherKeepsArchiveWhenPackCommitFails(t *testing.T) {
	archive := zipBytes(t, "art.ans", "ANSI art")
	root := t.TempDir()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/year/2026":
			writeAPIResponse(t, writer, []Result{{Year: 2026, Name: "pack", Download: server.URL + "/archive/2026/pack.zip"}})
		case "/archive/2026/pack.zip":
			if err := os.MkdirAll(filepath.Join(root, "2026", "pack"), 0o755); err != nil {
				t.Error(err)
			}
			_, _ = writer.Write(archive)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	config := testConfig(root)
	client := newTestClient(t, server.URL+"/")
	summary, err := NewFetcher(client, config, io.Discard).Run(context.Background())
	if err == nil || summary.Failed != 1 {
		t.Fatalf("Run summary = %+v, error = %v", summary, err)
	}
	if _, err := os.Stat(filepath.Join(root, "2026", "pack-pack.zip")); err != nil {
		t.Fatal("archive was removed before the pack commit succeeded")
	}
}

func TestFetcherPreservesExistingPackWhenExtractionFails(t *testing.T) {
	server := newPackServer(t, []byte("not a ZIP archive"), "pack.zip")
	defer server.Close()

	config := testConfig(t.TempDir())
	config.Overwrite = true
	config.SkipExisting = false
	packDir := filepath.Join(config.Path, "2026", "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(packDir, "existing.ans")
	if err := os.WriteFile(markerPath, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}

	client := newTestClient(t, server.URL+"/")
	summary, err := NewFetcher(client, config, io.Discard).Run(context.Background())
	if err == nil || summary.Failed != 1 {
		t.Fatalf("Run summary = %+v, error = %v", summary, err)
	}
	if data, readErr := os.ReadFile(markerPath); readErr != nil || string(data) != "existing" {
		t.Fatalf("existing pack changed: data=%q error=%v", data, readErr)
	}
}

func TestFetcherStopsOnCancellation(t *testing.T) {
	archive := zipBytes(t, "art.ans", "ANSI art")
	ctx, cancel := context.WithCancel(context.Background())
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/year/2026":
			results := make([]Result, 10)
			for index := range results {
				name := fmt.Sprintf("pack-%d", index)
				results[index] = Result{Year: 2026, Name: name, Download: server.URL + "/archive/2026/" + name + ".zip"}
			}
			writeAPIResponse(t, writer, results)
		default:
			cancel()
			_, _ = writer.Write(archive)
		}
	}))
	defer server.Close()

	config := testConfig(t.TempDir())
	summary, err := NewFetcher(newTestClient(t, server.URL+"/"), config, io.Discard).Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "fetch canceled") {
		t.Fatalf("Run summary = %+v, error = %v", summary, err)
	}
	if summary.Fetched+summary.Failed >= 10 {
		t.Fatalf("cancellation did not stop the run: %+v", summary)
	}
}

func TestFetcherBoundsConcurrency(t *testing.T) {
	archive := zipBytes(t, "art.ans", "ANSI art")
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var active atomic.Int32
	var maximum atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/year/2026" {
			results := make([]Result, 4)
			for index := range results {
				name := fmt.Sprintf("pack-%d", index)
				results[index] = Result{Year: 2026, Name: name, Download: server.URL + "/archive/2026/" + name + ".zip"}
			}
			writeAPIResponse(t, writer, results)
			return
		}
		if strings.HasPrefix(request.URL.Path, "/archive/") {
			current := active.Add(1)
			for {
				observed := maximum.Load()
				if current <= observed || maximum.CompareAndSwap(observed, current) {
					break
				}
			}
			started <- struct{}{}
			<-release
			active.Add(-1)
			_, _ = writer.Write(archive)
			return
		}
		http.NotFound(writer, request)
	}))
	defer server.Close()

	config := testConfig(t.TempDir())
	config.Concurrency = 2
	client := newTestClient(t, server.URL+"/")
	resultChannel := make(chan error, 1)
	go func() {
		_, err := NewFetcher(client, config, io.Discard).Run(context.Background())
		resultChannel <- err
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("two downloads did not start concurrently")
		}
	}
	close(release)
	if err := <-resultChannel; err != nil {
		t.Fatal(err)
	}
	if maximum.Load() != 2 {
		t.Fatalf("maximum concurrent downloads = %d, want 2", maximum.Load())
	}
}

func TestFormatBytes(t *testing.T) {
	tests := map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 5 * 1024 * 1024: "5.0 MiB", 3 << 30: "3.0 GiB"}
	for bytes, want := range tests {
		if got := formatBytes(bytes); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", bytes, got, want)
		}
	}
}

func testConfig(path string) Config {
	return Config{
		Years:        1,
		Year:         2026,
		Path:         path,
		SkipExisting: true,
		Concurrency:  1,
		Timeout:      time.Second,
		Quiet:        true,
	}
}

func newPackServer(t *testing.T, archive []byte, archiveName string) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/year/2026":
			writeAPIResponse(t, writer, []Result{{Year: 2026, Name: "pack", Download: server.URL + "/archive/2026/" + archiveName}})
		case "/archive/2026/" + archiveName:
			_, _ = writer.Write(archive)
		default:
			http.NotFound(writer, request)
		}
	}))
	return server
}

func writeAPIResponse(t *testing.T, writer http.ResponseWriter, results []Result) {
	t.Helper()
	response := Response{Page: Page{Page: 1, Pages: 1, Total: len(results)}, Results: results}
	if err := json.NewEncoder(writer).Encode(response); err != nil {
		t.Error(err)
	}
}

// zipBytes builds a ZIP archive from alternating name, content pairs.
func zipBytes(t *testing.T, pairs ...string) []byte {
	t.Helper()
	if len(pairs)%2 != 0 {
		t.Fatal("zipBytes needs name, content pairs")
	}
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	for index := 0; index < len(pairs); index += 2 {
		entry, err := archive.Create(pairs[index])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(pairs[index+1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func listDirs(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names
}
