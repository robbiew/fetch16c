package main

import (
	"context"
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

func TestValidateArchiveURL(t *testing.T) {
	client, err := NewClient(defaultAPIBaseURL, time.Second, io.Discard, false)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		url     string
		want    string
		wantErr bool
	}{
		{name: "official URL", url: "https://16colo.rs/archive/2026/pack.zip", want: "https://api.16colo.rs/archive/2026/pack.zip"},
		{name: "LZH archive", url: "https://api.16colo.rs/archive/1995/pack.lzh", want: "https://api.16colo.rs/archive/1995/pack.lzh"},
		{name: "untrusted host", url: "https://example.com/archive/2026/pack.zip", wantErr: true},
		{name: "insecure scheme", url: "http://16colo.rs/archive/2026/pack.zip", wantErr: true},
		{name: "directory URL", url: "https://16colo.rs/archive/1990/", wantErr: true},
		{name: "unsupported extension", url: "https://16colo.rs/archive/2026/pack.rar", wantErr: true},
		{name: "credentials", url: "https://user@example.com/archive/2026/pack.zip", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archiveURL, err := client.ValidateArchiveURL(test.url)
			if test.wantErr {
				if err == nil {
					t.Fatalf("ValidateArchiveURL(%q) unexpectedly succeeded", test.url)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if archiveURL.String() != test.want {
				t.Fatalf("ValidateArchiveURL(%q) = %q, want %q", test.url, archiveURL, test.want)
			}
		})
	}
}

func TestDownloadKeepsDestinationAndRemovesPartialFileOnInterruption(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Length", "100")
		_, _ = writer.Write([]byte("short"))
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/")
	archiveURL, err := client.ValidateArchiveURL(server.URL + "/archive/pack.zip")
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	outputPath := filepath.Join(directory, "pack.zip")
	if err := os.WriteFile(outputPath, []byte("previous archive"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := client.Download(context.Background(), archiveURL, outputPath, "pack"); err == nil {
		t.Fatal("Download accepted an interrupted response")
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "previous archive" {
		t.Fatalf("destination was changed to %q", data)
	}
	partialFiles, err := filepath.Glob(filepath.Join(directory, "*.part"))
	if err != nil {
		t.Fatal(err)
	}
	if len(partialFiles) != 0 {
		t.Fatalf("partial files were not removed: %v", partialFiles)
	}
}

func TestDownloadRejectsRedirectTarget(t *testing.T) {
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		targetRequests.Add(1)
		_, _ = writer.Write([]byte("redirected archive"))
	}))
	defer target.Close()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL+"/archive/pack.zip", http.StatusFound)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/")
	archiveURL, err := client.ValidateArchiveURL(server.URL + "/archive/pack.zip")
	if err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(t.TempDir(), "pack.zip")
	if _, err := client.Download(context.Background(), archiveURL, outputPath, "pack"); err == nil {
		t.Fatal("Download followed a redirect")
	}
	if targetRequests.Load() != 0 {
		t.Fatalf("redirect target received %d requests", targetRequests.Load())
	}
	if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
		t.Fatalf("Download created a file after a redirect: %v", err)
	}
}

func TestDownloadPreservesDestinationWhenCommitFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte("archive"))
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/")
	archiveURL, err := client.ValidateArchiveURL(server.URL + "/archive/pack.zip")
	if err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(t.TempDir(), "pack.zip")
	if err := os.Mkdir(outputPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Download(context.Background(), archiveURL, outputPath, "pack"); err == nil {
		t.Fatal("Download replaced a destination directory")
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatal("failed download did not preserve the destination directory")
	}
}

func TestClientRequestTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()

	client, err := NewClient(server.URL+"/", 20*time.Millisecond, io.Discard, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.FetchYear(context.Background(), 2026)
	if err == nil || !strings.Contains(err.Error(), "Client.Timeout") {
		t.Fatalf("FetchYear timeout error = %v", err)
	}
}

func TestFetchYearRejectsInvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, "not JSON")
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/")
	if _, err := client.FetchYear(context.Background(), 2026); err == nil {
		t.Fatal("FetchYear accepted invalid JSON")
	}
}

func TestFetchYearAcceptsMixedGroupTypes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, `{"page":{"total":1,"pages":1},"results":[{"name":"legacy","groups":["group",123]}]}`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/")
	response, err := client.FetchYear(context.Background(), 1995)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || string(response.Results[0].Groups) != `["group",123]` {
		t.Fatalf("mixed groups were not preserved: %#v", response.Results)
	}
}
