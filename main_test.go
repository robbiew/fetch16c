package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func errorIs(err error, target error) bool {
	return errors.Is(err, target)
}

func TestArchiveDownloadURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "official archive",
			url:  "https://16colo.rs/archive/2026/chromeintro.zip",
			want: "https://api.16colo.rs/archive/2026/chromeintro.zip",
		},
		{
			name: "hash in archive name",
			url:  "https://16colo.rs/archive/1996/joint#06.zip",
			want: "https://api.16colo.rs/archive/1996/joint%2306.zip",
		},
		{
			name: "unrelated path",
			url:  "https://16colo.rs/gallery/2026/chromeintro",
			want: "https://16colo.rs/gallery/2026/chromeintro",
		},
		{
			name: "unrelated host",
			url:  "https://example.com/archive/2026/chromeintro.zip",
			want: "https://example.com/archive/2026/chromeintro.zip",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := archiveDownloadURL(test.url); got != test.want {
				t.Fatalf("archiveDownloadURL(%q) = %q, want %q", test.url, got, test.want)
			}
		})
	}
}

func TestExecuteReturnsNonzeroOnFetchFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := execute(context.Background(), []string{"--year", "2026", "--quiet", "--retries", "0", "--path", t.TempDir()}, &stdout, &stderr, server.URL+"/")
	if exitCode != 1 {
		t.Fatalf("execute exit code = %d, want 1", exitCode)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("year 2026")) {
		t.Fatalf("stderr does not identify the failed year: %s", stderr.String())
	}
}

func TestExecuteReturnsTwoForInvalidFlags(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if exitCode := execute(context.Background(), []string{"--concurrency", "0"}, &stdout, &stderr, defaultAPIBaseURL); exitCode != 2 {
		t.Fatalf("execute exit code = %d, want 2", exitCode)
	}
}

func TestExecutePrintsVersion(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if exitCode := execute(context.Background(), []string{"--version"}, &stdout, &stderr, defaultAPIBaseURL); exitCode != 0 {
		t.Fatalf("execute exit code = %d, want 0", exitCode)
	}
	if !strings.HasPrefix(stdout.String(), "fetch16c ") {
		t.Fatalf("version output = %q", stdout.String())
	}
}

func TestExecuteSucceedsWithUnsupportedPacksAndWarns(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeAPIResponse(t, writer, []Result{{Year: 2026, Name: "old", Download: server.URL + "/archive/2026/old.arj"}})
	}))
	defer server.Close()

	t.Setenv("PATH", t.TempDir())
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := execute(context.Background(), []string{"--year", "2026", "--path", t.TempDir()}, &stdout, &stderr, server.URL+"/")
	if exitCode != 0 {
		t.Fatalf("execute exit code = %d, want 0 (stderr: %s)", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "unsupported=1") || !strings.Contains(stderr.String(), "install lha, 7z, or unar") {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}

func TestExecuteReturnsInterruptedExitCode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := execute(ctx, []string{"--year", "2026", "--quiet", "--path", t.TempDir()}, &stdout, &stderr, "http://127.0.0.1:9/")
	if exitCode != exitInterrupted {
		t.Fatalf("execute exit code = %d, want %d", exitCode, exitInterrupted)
	}
}

func TestLiveArtPackDownloadAndExtraction(t *testing.T) {
	if os.Getenv("FETCH16C_LIVE_TEST") == "" {
		t.Skip("set FETCH16C_LIVE_TEST=1 to run the live smoke test")
	}

	year := time.Now().Year()
	client, err := NewClient(defaultAPIBaseURL, ClientOptions{Timeout: 2 * time.Minute, Retries: 2})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.FetchYear(context.Background(), year, TypePacks)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) == 0 {
		t.Fatalf("API returned no art packs for %d", year)
	}

	result := response.Results[0]
	config := testConfig(t.TempDir())
	config.Year = year
	config.Packs = []string{result.Name}
	fetcher := NewFetcher(client, config, io.Discard)
	summary, err := fetcher.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Fetched != 1 || summary.Failed != 0 {
		t.Fatalf("live fetch summary = %+v", summary)
	}

	outputDir := filepath.Join(config.Path, fmt.Sprintf("%d", year), result.Name)
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("archive for %s extracted no files", result.Name)
	}
	summary, err = fetcher.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.Skipped != 1 || summary.Fetched != 0 {
		t.Fatalf("live resume summary = %+v", summary)
	}
}

func TestLiveHistoricalPagination(t *testing.T) {
	if os.Getenv("FETCH16C_LIVE_TEST") == "" {
		t.Skip("set FETCH16C_LIVE_TEST=1 to run the live smoke test")
	}

	client, err := NewClient(defaultAPIBaseURL, ClientOptions{Timeout: 2 * time.Minute, Retries: 2})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.FetchYear(context.Background(), 1996, TypePacks)
	if err != nil {
		t.Fatal(err)
	}
	if response.Page.Pages <= 1 || len(response.Results) != response.Page.Total {
		t.Fatalf("historical pagination returned %d of %d packs across %d pages", len(response.Results), response.Page.Total, response.Page.Pages)
	}
}

func TestLiveNumericPackNamesDecode(t *testing.T) {
	if os.Getenv("FETCH16C_LIVE_TEST") == "" {
		t.Skip("set FETCH16C_LIVE_TEST=1 to run the live smoke test")
	}

	client, err := NewClient(defaultAPIBaseURL, ClientOptions{Timeout: 2 * time.Minute, Retries: 2})
	if err != nil {
		t.Fatal(err)
	}
	// The 1990 listing contains a pack literally named 1990, served as a JSON number.
	response, err := client.FetchYear(context.Background(), 1990, TypePacks)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, result := range response.Results {
		if result.Name == "1990" {
			found = true
		}
	}
	if !found {
		t.Fatalf("pack 1990 missing from %+v", response.Results)
	}
}
