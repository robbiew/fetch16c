package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cheggaaa/pb/v3"
)

type Result struct {
	Year     int             `json:"year"`
	Name     string          `json:"name"`
	Download string          `json:"download"`
	Gallery  string          `json:"gallery"`
	Archive  string          `json:"archive"`
	Groups   json.RawMessage `json:"groups"`
}

type Response struct {
	Page    Page     `json:"page"`
	Results []Result `json:"results"`
}

type Page struct {
	Total    int    `json:"total"`
	Sort     string `json:"sort"`
	Order    string `json:"order"`
	PageSize int    `json:"pagesize"`
	Page     int    `json:"page"`
	Pages    int    `json:"pages"`
	Offset   int    `json:"offset"`
	Options  struct {
		Filter interface{} `json:"filter"`
		Type   string      `json:"type"`
		Groups bool        `json:"groups"`
	} `json:"options"`
}

type Client struct {
	httpClient *http.Client
	apiBaseURL *url.URL
	output     io.Writer
	progress   bool
}

func NewClient(apiBaseURL string, timeout time.Duration, output io.Writer, progress bool) (*Client, error) {
	return NewClientWithHTTPClient(apiBaseURL, &http.Client{Timeout: timeout}, output, progress)
}

func NewClientWithHTTPClient(apiBaseURL string, httpClient *http.Client, output io.Writer, progress bool) (*Client, error) {
	parsedURL, err := url.Parse(apiBaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid API base URL: %w", err)
	}
	if parsedURL.Scheme == "" || parsedURL.Host == "" {
		return nil, fmt.Errorf("invalid API base URL: %s", apiBaseURL)
	}
	if !strings.HasSuffix(parsedURL.Path, "/") {
		parsedURL.Path += "/"
	}
	if httpClient == nil {
		return nil, fmt.Errorf("HTTP client cannot be nil")
	}
	return &Client{
		httpClient: httpClient,
		apiBaseURL: parsedURL,
		output:     output,
		progress:   progress,
	}, nil
}

func (client *Client) FetchYear(ctx context.Context, year int) (Response, error) {
	yearURL := client.apiBaseURL.ResolveReference(&url.URL{Path: fmt.Sprintf("year/%d", year)})
	response, err := client.fetchPage(ctx, yearURL)
	if err != nil {
		return response, err
	}

	pages := response.Page.Pages
	if pages < 1 {
		pages = 1
	}
	for page := 2; page <= pages; page++ {
		pageURL := *yearURL
		query := pageURL.Query()
		query.Set("page", fmt.Sprintf("%d", page))
		pageURL.RawQuery = query.Encode()
		pageResponse, err := client.fetchPage(ctx, &pageURL)
		if err != nil {
			return response, fmt.Errorf("fetch page %d for year %d: %w", page, year, err)
		}
		response.Results = append(response.Results, pageResponse.Results...)
	}

	if response.Page.Total > 0 && len(response.Results) != response.Page.Total {
		return response, fmt.Errorf("year %d API returned %d of %d packs", year, len(response.Results), response.Page.Total)
	}
	return response, nil
}

func (client *Client) fetchPage(ctx context.Context, pageURL *url.URL) (Response, error) {
	var response Response
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL.String(), nil)
	if err != nil {
		return response, fmt.Errorf("create API request: %w", err)
	}
	resp, err := client.httpClient.Do(request)
	if err != nil {
		return response, fmt.Errorf("fetch API response: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return response, fmt.Errorf("fetch API response: HTTP %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return response, fmt.Errorf("decode API response: %w", err)
	}
	return response, nil
}

func archiveDownloadURL(rawURL string) string {
	parsedURL, err := url.Parse(rawURL)
	if err != nil || parsedURL.Hostname() != "16colo.rs" || !strings.HasPrefix(parsedURL.Path, "/archive/") {
		return rawURL
	}
	parsedURL.Host = "api.16colo.rs"
	return parsedURL.String()
}

func (client *Client) ValidateArchiveURL(rawURL string) (*url.URL, error) {
	parsedURL, err := url.Parse(archiveDownloadURL(rawURL))
	if err != nil {
		return nil, fmt.Errorf("invalid archive URL: %w", err)
	}
	if parsedURL.User != nil || parsedURL.Host == "" {
		return nil, urlError("archive URL must have a host", rawURL)
	}

	isOfficialHost := parsedURL.Hostname() == "api.16colo.rs" || parsedURL.Hostname() == "16colo.rs"
	isConfiguredHost := parsedURL.Hostname() == client.apiBaseURL.Hostname()
	if !isOfficialHost && !isConfiguredHost {
		return nil, urlError("unexpected archive host", rawURL)
	}
	if parsedURL.Scheme != "https" && !(isConfiguredHost && parsedURL.Scheme == client.apiBaseURL.Scheme) {
		return nil, urlError("archive URL must use HTTPS", rawURL)
	}
	if !strings.HasPrefix(parsedURL.Path, "/archive/") {
		return nil, urlError("archive URL has an unexpected path", rawURL)
	}
	if strings.HasSuffix(parsedURL.Path, "/") {
		return nil, urlError("archive URL does not name a file", rawURL)
	}

	filename := filepath.Base(parsedURL.Path)
	if filename == "." || filename == "/" || filename == "archive" {
		return nil, urlError("archive URL does not name a file", rawURL)
	}
	extension := strings.ToLower(filepath.Ext(filename))
	if extension != ".zip" && extension != ".lha" && extension != ".lzh" {
		return nil, urlError("unsupported archive format", rawURL)
	}
	return parsedURL, nil
}

func urlError(message string, rawURL string) error {
	return fmt.Errorf("%s: %q", message, rawURL)
}

func (client *Client) Download(ctx context.Context, archiveURL *url.URL, outputPath string, label string) (int64, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, archiveURL.String(), nil)
	if err != nil {
		return 0, fmt.Errorf("create download request: %w", err)
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return 0, fmt.Errorf("download file: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return 0, fmt.Errorf("download file: HTTP %s", response.Status)
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return 0, fmt.Errorf("create archive directory: %w", err)
	}
	temporaryFile, err := os.CreateTemp(filepath.Dir(outputPath), "."+filepath.Base(outputPath)+"-*.part")
	if err != nil {
		return 0, fmt.Errorf("create temporary archive: %w", err)
	}
	temporaryPath := temporaryFile.Name()
	defer os.Remove(temporaryPath)

	writer := io.Writer(temporaryFile)
	var bar *pb.ProgressBar
	if client.progress && response.ContentLength > 0 {
		bar = pb.New64(response.ContentLength)
		bar.SetWriter(client.output)
		bar.Set(pb.Bytes, true)
		bar.SetWidth(60)
		bar.Set("prefix", label+" ")
		bar.Start()
		writer = &progressWriter{bar: bar, writer: temporaryFile}
	}
	if bar != nil {
		defer bar.Finish()
	}

	written, copyErr := io.Copy(writer, response.Body)
	closeErr := temporaryFile.Close()
	if copyErr != nil {
		return written, fmt.Errorf("save file: %w", copyErr)
	}
	if closeErr != nil {
		return written, fmt.Errorf("close temporary archive: %w", closeErr)
	}
	if response.ContentLength >= 0 && written != response.ContentLength {
		return written, fmt.Errorf("incomplete download: received %d of %d bytes", written, response.ContentLength)
	}

	if err := os.Remove(outputPath); err != nil && !os.IsNotExist(err) {
		return written, fmt.Errorf("replace archive: %w", err)
	}
	if err := os.Rename(temporaryPath, outputPath); err != nil {
		return written, fmt.Errorf("commit archive: %w", err)
	}
	return written, nil
}

type progressWriter struct {
	bar    *pb.ProgressBar
	writer io.Writer
}

func (writer *progressWriter) Write(data []byte) (int, error) {
	written, err := writer.writer.Write(data)
	writer.bar.Add(written)
	return written, err
}
