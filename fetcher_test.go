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
			writeAPIResponse(t, writer, []Result{{Year: 2026, Name: "pack", Download: server.URL + "/archive/2026/pack.zip"}})
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
	if _, err := os.Stat(filepath.Join(packDir, manifestFilename)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "2026", "pack.zip")); !os.IsNotExist(err) {
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
	if _, err := os.Stat(filepath.Join(config.Path, "2026", "pack.zip")); err != nil {
		t.Fatal(err)
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

func TestFetcherRequiresLHACommandBeforeDownloading(t *testing.T) {
	var downloads atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/year/2026":
			writeAPIResponse(t, writer, []Result{{Year: 2026, Name: "legacy", Download: server.URL + "/archive/2026/legacy.lha"}})
		case "/archive/2026/legacy.lha":
			downloads.Add(1)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/")
	fetcher := NewFetcher(client, testConfig(t.TempDir()), io.Discard)
	fetcher.lookPath = func(string) (string, error) { return "", errors.New("not found") }
	summary, err := fetcher.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "lha command is required") {
		t.Fatalf("Run summary = %+v, error = %v", summary, err)
	}
	if downloads.Load() != 0 {
		t.Fatalf("download count = %d, want 0", downloads.Load())
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
			for current > maximum.Load() && !maximum.CompareAndSwap(maximum.Load(), current) {
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
	for count := 0; count < 2; count++ {
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

func zipBytes(t *testing.T, name string, content string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	entry, err := archive.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
