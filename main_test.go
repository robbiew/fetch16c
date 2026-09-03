package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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

func TestFetchAPIResponseRejectsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/")
	if _, err := client.FetchYear(context.Background(), 2026); err == nil {
		t.Fatal("FetchYear accepted an unsuccessful HTTP response")
	}
}

func TestFetchYearResponseGetsEveryPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		page := 1
		name := "first"
		if request.URL.Query().Get("page") == "2" {
			page = 2
			name = "second"
		}
		response := Response{
			Page:    Page{Page: page, Pages: 2, Total: 2},
			Results: []Result{{Name: name}},
		}
		if err := json.NewEncoder(writer).Encode(response); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/")
	response, err := client.FetchYear(context.Background(), 2026)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 2 || response.Results[0].Name != "first" || response.Results[1].Name != "second" {
		t.Fatalf("FetchYear returned %#v", response.Results)
	}
}

func TestDownloadFileRejectsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	client := newTestClient(t, server.URL+"/")
	archiveURL, err := client.ValidateArchiveURL(server.URL + "/archive/missing.zip")
	if err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(t.TempDir(), "missing.zip")
	if _, err := client.Download(context.Background(), archiveURL, outputPath, "missing"); err == nil {
		t.Fatal("Download accepted an unsuccessful HTTP response")
	}
	if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
		t.Fatalf("downloadFile created %s after an HTTP error", outputPath)
	}
}

func TestExtractZipArchiveWithAbsoluteOutputPath(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "pack.zip")
	createZipArchive(t, archivePath, "nested/art.ans")

	outputDir := t.TempDir()
	if err := extractZipArchive(archivePath, outputDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outputDir, "nested", "art.ans")); err != nil {
		t.Fatal(err)
	}
}

func TestExtractZipArchiveRejectsTraversal(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "pack.zip")
	createZipArchive(t, archivePath, "../escaped.ans")

	if err := extractZipArchive(archivePath, t.TempDir()); err == nil {
		t.Fatal("extractZipArchive accepted a path outside the output directory")
	}
}

func createZipArchive(t *testing.T, archivePath string, entryName string) {
	t.Helper()

	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	entry, err := archive.Create(entryName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("ANSI art")); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLiveArtPackDownloadAndExtraction(t *testing.T) {
	if os.Getenv("FETCH16C_LIVE_TEST") == "" {
		t.Skip("set FETCH16C_LIVE_TEST=1 to run the live smoke test")
	}

	year := time.Now().Year()
	client, err := NewClient(defaultAPIBaseURL, 2*time.Minute, io.Discard, false)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.FetchYear(context.Background(), year)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) == 0 {
		t.Fatalf("API returned no art packs for %d", year)
	}

	result := response.Results[0]
	config := testConfig(t.TempDir())
	config.Year = year
	config.Pack = result.Name
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

	client, err := NewClient(defaultAPIBaseURL, 2*time.Minute, io.Discard, false)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.FetchYear(context.Background(), 1996)
	if err != nil {
		t.Fatal(err)
	}
	if response.Page.Pages <= 1 || len(response.Results) != response.Page.Total {
		t.Fatalf("historical pagination returned %d of %d packs across %d pages", len(response.Results), response.Page.Total, response.Page.Pages)
	}
}

func newTestClient(t *testing.T, apiBaseURL string) *Client {
	t.Helper()
	client, err := NewClient(apiBaseURL, time.Second, io.Discard, false)
	if err != nil {
		t.Fatal(err)
	}
	return client
}
