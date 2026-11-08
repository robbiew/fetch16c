package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, apiBaseURL string) *Client {
	t.Helper()
	client, err := NewClient(apiBaseURL, ClientOptions{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// newRetryingTestClient returns a client that retries without sleeping and records the delays.
func newRetryingTestClient(t *testing.T, apiBaseURL string, retries int) (*Client, *[]time.Duration) {
	t.Helper()
	client, err := NewClient(apiBaseURL, ClientOptions{Timeout: time.Second, Retries: retries})
	if err != nil {
		t.Fatal(err)
	}
	delays := &[]time.Duration{}
	client.sleep = func(ctx context.Context, delay time.Duration) error {
		*delays = append(*delays, delay)
		return ctx.Err()
	}
	return client, delays
}

func TestResultUnmarshalToleratesLooseTypes(t *testing.T) {
	raw := `[
		{"year":1990,"name":1990,"download":"https://16colo.rs/archive/1990/1990.zip","gallery":"https://16colo.rs/pack/1990","archive":"1990.zip"},
		{"year":"1996","name":"acid-53a","download":"https://16colo.rs/archive/1996/","gallery":null,"archive":"","groups":["acid",123,null]},
		{"year":2001,"name":"solo","groups":null}
	]`
	var results []Result
	if err := json.Unmarshal([]byte(raw), &results); err != nil {
		t.Fatal(err)
	}
	if results[0].Name != "1990" || results[0].Year != 1990 || results[0].Groups != nil {
		t.Fatalf("numeric name decoded as %+v", results[0])
	}
	if results[1].Year != 1996 || results[1].Archive != "" || strings.Join(results[1].Groups, ",") != "acid,123" {
		t.Fatalf("mixed groups decoded as %+v", results[1])
	}
	if results[2].Groups != nil || results[2].Download != "" {
		t.Fatalf("null fields decoded as %+v", results[2])
	}
	if err := json.Unmarshal([]byte(`{"year":"nineteen"}`), &results[0]); err == nil {
		t.Fatal("Result accepted a non-numeric year")
	}
}

func TestValidateArchiveURL(t *testing.T) {
	client := newTestClient(t, defaultAPIBaseURL)

	tests := []struct {
		name    string
		url     string
		want    string
		wantErr error
		fails   bool
	}{
		{name: "official URL", url: "https://16colo.rs/archive/2026/pack.zip", want: "https://api.16colo.rs/archive/2026/pack.zip"},
		{name: "LZH archive", url: "https://api.16colo.rs/archive/1995/pack.lzh", want: "https://api.16colo.rs/archive/1995/pack.lzh"},
		{name: "ARJ archive", url: "https://16colo.rs/archive/1992/pack.arj", want: "https://api.16colo.rs/archive/1992/pack.arj"},
		{name: "hash in name", url: "https://16colo.rs/archive/1996/joint#06.zip", want: "https://api.16colo.rs/archive/1996/joint%2306.zip"},
		{name: "bang in name", url: "https://16colo.rs/archive/1995/1oo!-ger.lha", want: "https://api.16colo.rs/archive/1995/1oo!-ger.lha"},
		{name: "untrusted host", url: "https://example.com/archive/2026/pack.zip", fails: true},
		{name: "official host with port", url: "https://16colo.rs:8443/archive/2026/pack.zip", fails: true},
		{name: "insecure scheme", url: "http://16colo.rs/archive/2026/pack.zip", fails: true},
		{name: "directory URL", url: "https://16colo.rs/archive/1990/", wantErr: errNoArchive},
		{name: "empty URL", url: "", wantErr: errNoArchive},
		{name: "unsupported extension", url: "https://16colo.rs/archive/2026/pack.rar", wantErr: errUnsupportedArchive},
		{name: "credentials", url: "https://user@example.com/archive/2026/pack.zip", fails: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archiveURL, err := client.ValidateArchiveURL(test.url)
			if test.wantErr != nil {
				if err == nil || !errorIs(err, test.wantErr) {
					t.Fatalf("ValidateArchiveURL(%q) error = %v, want %v", test.url, err, test.wantErr)
				}
				return
			}
			if test.fails {
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

func TestFetchYearSendsPageSizeTypeAndUserAgent(t *testing.T) {
	var query atomic.Value
	var userAgent atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query.Store(request.URL.Query())
		userAgent.Store(request.Header.Get("User-Agent"))
		writeAPIResponse(t, writer, []Result{{Name: "pack"}})
	}))
	defer server.Close()

	client, err := NewClient(server.URL, ClientOptions{Timeout: time.Second, UserAgent: "fetch16c/test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.FetchYear(context.Background(), 2026, TypeMags); err != nil {
		t.Fatal(err)
	}
	values := query.Load().(url.Values)
	if values["pagesize"][0] != "500" || values["type"][0] != "mags" || values["page"][0] != "1" {
		t.Fatalf("query = %v", values)
	}
	if userAgent.Load().(string) != "fetch16c/test" {
		t.Fatalf("User-Agent = %q", userAgent.Load())
	}
}

func TestFetchYearDetectsRepeatedPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		// The live API answers out-of-range pages with page 1 again.
		response := Response{Page: Page{Page: 1, Pages: 2, Total: 4}, Results: []Result{{Name: "a"}, {Name: "b"}}}
		if err := json.NewEncoder(writer).Encode(response); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/")
	_, err := client.FetchYear(context.Background(), 2026, TypePacks)
	if err == nil || !strings.Contains(err.Error(), "instead of page 2") {
		t.Fatalf("FetchYear error = %v", err)
	}
}

func TestFetchYearRetriesTransientErrors(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		switch requests.Add(1) {
		case 1:
			writer.Header().Set("Retry-After", "7")
			http.Error(writer, "busy", http.StatusTooManyRequests)
		case 2:
			http.Error(writer, "down", http.StatusBadGateway)
		default:
			writeAPIResponse(t, writer, []Result{{Name: "pack"}})
		}
	}))
	defer server.Close()

	client, delays := newRetryingTestClient(t, server.URL+"/", 3)
	response, err := client.FetchYear(context.Background(), 2026, TypePacks)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || requests.Load() != 3 {
		t.Fatalf("results = %d, requests = %d", len(response.Results), requests.Load())
	}
	if len(*delays) != 2 || (*delays)[0] != 7*time.Second || (*delays)[1] <= 0 {
		t.Fatalf("retry delays = %v", *delays)
	}
}

func TestFetchYearDoesNotRetryPermanentErrors(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		http.NotFound(writer, request)
	}))
	defer server.Close()

	client, _ := newRetryingTestClient(t, server.URL+"/", 3)
	if _, err := client.FetchYear(context.Background(), 2026, TypePacks); err == nil {
		t.Fatal("FetchYear accepted a 404 response")
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1", requests.Load())
	}
}

func TestFetchYearGivesUpAfterRetryBudget(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(writer, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client, _ := newRetryingTestClient(t, server.URL+"/", 2)
	_, err := client.FetchYear(context.Background(), 2026, TypePacks)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("FetchYear error = %v", err)
	}
	if requests.Load() != 3 {
		t.Fatalf("requests = %d, want 3", requests.Load())
	}
}

func TestListYearsSortsNewestFirst(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/year/" {
			http.NotFound(writer, request)
			return
		}
		_, _ = io.WriteString(writer, `{"1996":{"packs":844,"mags":215},"2026":{"packs":19,"mags":0},"2001":{"packs":187,"mags":4}}`)
	}))
	defer server.Close()

	years, err := newTestClient(t, server.URL+"/").ListYears(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(years) != 3 || years[0] != 2026 || years[1] != 2001 || years[2] != 1996 {
		t.Fatalf("ListYears = %v", years)
	}
}

func TestDownloadKeepsDestinationAndRemovesPartialFileOnInterruption(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
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

func TestDownloadRetriesTransientFailures(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		switch requests.Add(1) {
		case 1:
			http.Error(writer, "unavailable", http.StatusServiceUnavailable)
		case 2:
			writer.Header().Set("Content-Length", "100")
			_, _ = writer.Write([]byte("short"))
		default:
			_, _ = writer.Write([]byte("archive"))
		}
	}))
	defer server.Close()

	client, delays := newRetryingTestClient(t, server.URL+"/", 3)
	archiveURL, err := client.ValidateArchiveURL(server.URL + "/archive/pack.zip")
	if err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(t.TempDir(), "pack.zip")
	written, err := client.Download(context.Background(), archiveURL, outputPath, "pack")
	if err != nil {
		t.Fatal(err)
	}
	if written != int64(len("archive")) || requests.Load() != 3 || len(*delays) != 2 {
		t.Fatalf("written = %d, requests = %d, delays = %v", written, requests.Load(), *delays)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil || string(data) != "archive" {
		t.Fatalf("archive content = %q, error = %v", data, err)
	}
}

func TestDownloadAbandonsStalledTransfer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Length", "1000")
		_, _ = writer.Write([]byte("partial"))
		writer.(http.Flusher).Flush()
		<-request.Context().Done()
	}))
	defer server.Close()

	client, err := NewClient(server.URL+"/", ClientOptions{Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	archiveURL, err := client.ValidateArchiveURL(server.URL + "/archive/pack.zip")
	if err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(t.TempDir(), "pack.zip")
	start := time.Now()
	_, err = client.Download(context.Background(), archiveURL, outputPath, "pack")
	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("Download error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("stalled download took %s to abandon", elapsed)
	}
	if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
		t.Fatal("stalled download left a destination file")
	}
}

func TestDownloadRejectsRedirectToUntrustedTarget(t *testing.T) {
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
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
		t.Fatal("Download followed a redirect to another server")
	}
	if targetRequests.Load() != 0 {
		t.Fatalf("redirect target received %d requests", targetRequests.Load())
	}
	if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
		t.Fatalf("Download created a file after a redirect: %v", err)
	}
}

func TestDownloadFollowsRedirectOnTrustedHost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/archive/pack.zip" {
			http.Redirect(writer, request, "/archive/moved.zip", http.StatusMovedPermanently)
			return
		}
		_, _ = writer.Write([]byte("archive"))
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/")
	archiveURL, err := client.ValidateArchiveURL(server.URL + "/archive/pack.zip")
	if err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(t.TempDir(), "pack.zip")
	if _, err := client.Download(context.Background(), archiveURL, outputPath, "pack"); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadPreservesDestinationWhenCommitFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
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

func TestDownloadAppliesLastModified(t *testing.T) {
	modified := time.Date(2001, time.March, 4, 5, 6, 7, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
		_, _ = writer.Write([]byte("archive"))
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/")
	archiveURL, err := client.ValidateArchiveURL(server.URL + "/archive/pack.zip")
	if err != nil {
		t.Fatal(err)
	}
	outputPath := filepath.Join(t.TempDir(), "pack.zip")
	if _, err := client.Download(context.Background(), archiveURL, outputPath, "pack"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(modified) {
		t.Fatalf("archive mtime = %s, want %s", info.ModTime(), modified)
	}
}

func TestClientRequestTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()

	client, err := NewClient(server.URL+"/", ClientOptions{Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.FetchYear(context.Background(), 2026, TypePacks)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("FetchYear timeout error = %v", err)
	}
}

func TestClientHonorsCallerCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client, err := NewClient(server.URL+"/", ClientOptions{Timeout: time.Second, Retries: 5})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, err := client.FetchYear(ctx, 2026, TypePacks); err == nil {
		t.Fatal("FetchYear succeeded with a canceled context")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("FetchYear kept retrying after cancellation")
	}
}

func TestFetchYearRejectsInvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, "not JSON")
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/")
	if _, err := client.FetchYear(context.Background(), 2026, TypePacks); err == nil {
		t.Fatal("FetchYear accepted invalid JSON")
	}
}

func TestFetchYearAcceptsMixedGroupTypes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `{"page":{"total":1,"pages":1},"results":[{"name":"legacy","groups":["group",123]}]}`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL+"/")
	response, err := client.FetchYear(context.Background(), 1995, TypePacks)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || strings.Join(response.Results[0].Groups, ",") != "group,123" {
		t.Fatalf("mixed groups were not preserved: %#v", response.Results)
	}
}
