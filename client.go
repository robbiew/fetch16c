package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cheggaaa/pb/v3"
)

const (
	// apiPageSize is the largest page the 16colo.rs API serves.
	apiPageSize = 500
	// maxBackoff caps the exponential delay between retries.
	maxBackoff = 30 * time.Second
	// maxRetryAfter caps how long a Retry-After header can make the client wait.
	maxRetryAfter = time.Minute
	// defaultTimeout bounds API requests and the longest allowed stall during a download.
	defaultTimeout = 2 * time.Minute
)

// Pack types accepted by the year listing endpoint.
const (
	TypePacks = "packs"
	TypeMags  = "mags"
	TypeAll   = "all"
)

var (
	errNoArchive          = errors.New("no archive is published for this pack")
	errUnsupportedArchive = errors.New("unsupported archive format")
)

// Result is one pack (or magazine) entry from a listing.
type Result struct {
	Year     int      `json:"year"`
	Name     string   `json:"name"`
	Download string   `json:"download"`
	Gallery  string   `json:"gallery"`
	Archive  string   `json:"archive"`
	Groups   []string `json:"groups,omitempty"`
}

// UnmarshalJSON tolerates the loose typing of the live API: pack names that are bare numbers
// (the 1990 and 1991 packs), null fields, and group lists that mix strings and numbers.
func (result *Result) UnmarshalJSON(data []byte) error {
	var raw struct {
		Year     flexString      `json:"year"`
		Name     flexString      `json:"name"`
		Download flexString      `json:"download"`
		Gallery  flexString      `json:"gallery"`
		Archive  flexString      `json:"archive"`
		Groups   json.RawMessage `json:"groups"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	year := 0
	if raw.Year != "" {
		parsed, err := strconv.Atoi(string(raw.Year))
		if err != nil {
			return fmt.Errorf("pack %q has an invalid year %q", raw.Name, raw.Year)
		}
		year = parsed
	}
	*result = Result{
		Year:     year,
		Name:     string(raw.Name),
		Download: string(raw.Download),
		Gallery:  string(raw.Gallery),
		Archive:  string(raw.Archive),
		Groups:   parseGroups(raw.Groups),
	}
	return nil
}

// flexString decodes a JSON string, number, boolean, or null as text.
type flexString string

func (value *flexString) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	switch {
	case len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")):
		*value = ""
	case trimmed[0] == '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return err
		}
		*value = flexString(text)
	case trimmed[0] == '{' || trimmed[0] == '[':
		return fmt.Errorf("expected a string, got %s", trimmed)
	default:
		*value = flexString(trimmed)
	}
	return nil
}

func parseGroups(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var list []flexString
	if err := json.Unmarshal(raw, &list); err != nil {
		var single flexString
		if err := json.Unmarshal(raw, &single); err != nil {
			return nil
		}
		list = []flexString{single}
	}
	var groups []string
	for _, item := range list {
		if item != "" {
			groups = append(groups, string(item))
		}
	}
	return groups
}

// Response is a page of listing results plus its paging metadata.
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
		Filter any    `json:"filter"`
		Type   string `json:"type"`
		Groups bool   `json:"groups"`
	} `json:"options"`
}

// YearCounts is the per-year summary served by the bare year endpoint.
type YearCounts struct {
	Packs int `json:"packs"`
	Mags  int `json:"mags"`
}

// Client talks to the 16colo.rs API and downloads archives.
type Client struct {
	httpClient *http.Client
	apiBaseURL *url.URL
	output     io.Writer
	progress   bool
	timeout    time.Duration
	retries    int
	userAgent  string
	sleep      func(context.Context, time.Duration) error
	logf       func(format string, values ...any)
}

// ClientOptions configures a Client. Zero values select sensible defaults.
type ClientOptions struct {
	// HTTPClient is used when set; otherwise a client with a header timeout is built.
	HTTPClient *http.Client
	// Timeout bounds each API request and the longest stall tolerated during a download.
	Timeout time.Duration
	// Retries is how many times a transient failure is retried.
	Retries int
	// Output receives the byte progress bar when Progress is set.
	Output   io.Writer
	Progress bool
	// UserAgent identifies the client to the API.
	UserAgent string
}

func NewClient(apiBaseURL string, options ClientOptions) (*Client, error) {
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
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if options.Retries < 0 {
		return nil, errors.New("retries cannot be negative")
	}

	var httpClient http.Client
	if options.HTTPClient != nil {
		httpClient = *options.HTTPClient
	} else {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = timeout
		httpClient = http.Client{Transport: transport}
	}
	output := options.Output
	if output == nil {
		output = io.Discard
	}
	client := &Client{
		httpClient: &httpClient,
		apiBaseURL: parsedURL,
		output:     output,
		progress:   options.Progress,
		timeout:    timeout,
		retries:    options.Retries,
		userAgent:  options.UserAgent,
		sleep:      sleepContext,
	}
	httpClient.CheckRedirect = client.checkRedirect
	return client, nil
}

// SetLogger directs retry notices to the given printf-style function.
func (client *Client) SetLogger(logf func(format string, values ...any)) {
	client.logf = logf
}

func (client *Client) log(format string, values ...any) {
	if client.logf != nil {
		client.logf(format, values...)
	}
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// checkRedirect follows redirects only to trusted hosts over an acceptable scheme.
func (client *Client) checkRedirect(request *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("stopped after 5 redirects")
	}
	if !client.isTrustedHost(request.URL) || !client.isAcceptableScheme(request.URL) {
		return fmt.Errorf("refusing redirect to %q", request.URL.Redacted())
	}
	return nil
}

func (client *Client) isTrustedHost(target *url.URL) bool {
	host := target.Hostname()
	if target.Host == client.apiBaseURL.Host {
		return true
	}
	return target.Port() == "" && (host == "api.16colo.rs" || host == "16colo.rs")
}

func (client *Client) isAcceptableScheme(target *url.URL) bool {
	if target.Scheme == "https" {
		return true
	}
	return target.Host == client.apiBaseURL.Host && target.Scheme == client.apiBaseURL.Scheme
}

// httpError reports a response with an unsuccessful status code.
type httpError struct {
	status     int
	text       string
	retryAfter time.Duration
}

func (err *httpError) Error() string {
	return "HTTP " + err.text
}

// transientError marks a failure worth retrying, such as a network error or a stalled download.
type transientError struct {
	err error
}

func (err *transientError) Error() string { return err.err.Error() }
func (err *transientError) Unwrap() error { return err.err }

func newHTTPError(response *http.Response) *httpError {
	return &httpError{
		status:     response.StatusCode,
		text:       response.Status,
		retryAfter: parseRetryAfter(response.Header.Get("Retry-After")),
	}
}

func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return min(max(time.Duration(seconds)*time.Second, 0), maxRetryAfter)
	}
	if at, err := http.ParseTime(value); err == nil {
		return min(max(time.Until(at), 0), maxRetryAfter)
	}
	return 0
}

func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func backoff(attempt int) time.Duration {
	delay := min(500*time.Millisecond<<min(attempt, 10), maxBackoff)
	jitter := time.Duration(rand.Int64N(int64(delay)/2 + 1))
	return delay + jitter
}

// retryDelay reports how long to wait before retrying err, or false if err is permanent.
func retryDelay(err error, attempt int) (time.Duration, bool) {
	var statusErr *httpError
	if errors.As(err, &statusErr) {
		if !isRetryableStatus(statusErr.status) {
			return 0, false
		}
		if statusErr.retryAfter > 0 {
			return statusErr.retryAfter, true
		}
		return backoff(attempt), true
	}
	var transient *transientError
	if errors.As(err, &transient) {
		return backoff(attempt), true
	}
	return 0, false
}

// withRetry runs attempt until it succeeds, fails permanently, or the retry budget is spent.
func (client *Client) withRetry(ctx context.Context, what string, attempt func() error) error {
	for try := 0; ; try++ {
		err := attempt()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil || try >= client.retries {
			return err
		}
		delay, ok := retryDelay(err, try)
		if !ok {
			return err
		}
		client.log("Retrying %s in %s (%v)\n", what, delay.Round(time.Millisecond), err)
		if sleepErr := client.sleep(ctx, delay); sleepErr != nil {
			return err
		}
	}
}

func (client *Client) newRequest(ctx context.Context, target string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if client.userAgent != "" {
		request.Header.Set("User-Agent", client.userAgent)
	}
	return request, nil
}

// ListYears returns every year the API knows about, newest first.
func (client *Client) ListYears(ctx context.Context) ([]int, error) {
	var counts map[string]YearCounts
	listURL := client.apiBaseURL.ResolveReference(&url.URL{Path: "year/"})
	if err := client.fetchJSON(ctx, listURL, &counts); err != nil {
		return nil, fmt.Errorf("list years: %w", err)
	}
	years := make([]int, 0, len(counts))
	for key := range counts {
		year, err := strconv.Atoi(key)
		if err != nil {
			continue
		}
		years = append(years, year)
	}
	if len(years) == 0 {
		return nil, errors.New("list years: API returned no years")
	}
	slices.SortFunc(years, func(a, b int) int { return b - a })
	return years, nil
}

// FetchYear lists every pack of the given type released in year, following all pages.
func (client *Client) FetchYear(ctx context.Context, year int, packType string) (Response, error) {
	if packType == "" {
		packType = TypePacks
	}
	yearURL := client.apiBaseURL.ResolveReference(&url.URL{Path: fmt.Sprintf("year/%d", year)})
	query := yearURL.Query()
	query.Set("type", packType)
	yearURL.RawQuery = query.Encode()
	return client.fetchListing(ctx, yearURL, fmt.Sprintf("year %d", year))
}

func (client *Client) fetchListing(ctx context.Context, listURL *url.URL, label string) (Response, error) {
	var combined Response
	seen := make(map[string]bool)
	for page := 1; ; page++ {
		pageURL := *listURL
		query := pageURL.Query()
		query.Set("page", strconv.Itoa(page))
		query.Set("pagesize", strconv.Itoa(apiPageSize))
		pageURL.RawQuery = query.Encode()

		var response Response
		if err := client.fetchJSON(ctx, &pageURL, &response); err != nil {
			return combined, fmt.Errorf("fetch %s page %d: %w", label, page, err)
		}
		if page == 1 {
			combined.Page = response.Page
		}
		if response.Page.Page != 0 && response.Page.Page != page {
			// The API answers out-of-range page numbers with the first page again.
			return combined, fmt.Errorf("fetch %s: API returned page %d instead of page %d", label, response.Page.Page, page)
		}
		for _, result := range response.Results {
			key := strings.ToLower(result.Name)
			if key != "" {
				if seen[key] {
					continue
				}
				seen[key] = true
			}
			combined.Results = append(combined.Results, result)
		}
		if page >= max(response.Page.Pages, 1) || len(response.Results) == 0 {
			break
		}
	}
	if combined.Page.Total > 0 && len(combined.Results) < combined.Page.Total {
		return combined, fmt.Errorf("fetch %s: API returned %d of %d packs", label, len(combined.Results), combined.Page.Total)
	}
	return combined, nil
}

// fetchJSON GETs target and decodes its JSON body, retrying transient failures.
func (client *Client) fetchJSON(ctx context.Context, target *url.URL, value any) error {
	return client.withRetry(ctx, target.Path, func() error {
		attemptCtx, cancel := context.WithTimeout(ctx, client.timeout)
		defer cancel()

		request, err := client.newRequest(attemptCtx, target.String())
		if err != nil {
			return fmt.Errorf("create API request: %w", err)
		}
		response, err := client.httpClient.Do(request)
		if err != nil {
			return client.requestError(ctx, attemptCtx, "fetch API response", err)
		}
		defer response.Body.Close()
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return fmt.Errorf("fetch API response: %w", newHTTPError(response))
		}
		if err := json.NewDecoder(response.Body).Decode(value); err != nil {
			if attemptCtx.Err() != nil {
				return client.requestError(ctx, attemptCtx, "fetch API response", err)
			}
			return fmt.Errorf("decode API response: %w", err)
		}
		return nil
	})
}

// requestError classifies a transport failure: a caller cancellation is permanent, while a
// per-attempt timeout or network error is transient.
func (client *Client) requestError(parent context.Context, attemptCtx context.Context, what string, err error) error {
	if parent.Err() != nil {
		return fmt.Errorf("%s: %w", what, parent.Err())
	}
	if errors.Is(attemptCtx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return &transientError{fmt.Errorf("%s: timed out after %s", what, client.timeout)}
	}
	return &transientError{fmt.Errorf("%s: %w", what, err)}
}

// archiveDownloadURL rewrites a 16colo.rs download link to the API host that serves archives.
// Archive names may contain a literal '#', which the API does not escape, so it is encoded
// before parsing to keep it in the path instead of becoming a fragment.
func archiveDownloadURL(rawURL string) string {
	rawURL = strings.ReplaceAll(rawURL, "#", "%23")
	parsedURL, err := url.Parse(rawURL)
	if err != nil || parsedURL.Host != "16colo.rs" || !strings.HasPrefix(parsedURL.Path, "/archive/") {
		return rawURL
	}
	parsedURL.Host = "api.16colo.rs"
	return parsedURL.String()
}

// ValidateArchiveURL checks that a listing's download URL points at a supported archive on a
// trusted host. It returns errNoArchive when the listing has no archive and
// errUnsupportedArchive for formats fetch16c cannot unpack.
func (client *Client) ValidateArchiveURL(rawURL string) (*url.URL, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, errNoArchive
	}
	parsedURL, err := url.Parse(archiveDownloadURL(rawURL))
	if err != nil {
		return nil, fmt.Errorf("invalid archive URL: %w", err)
	}
	if parsedURL.User != nil || parsedURL.Host == "" {
		return nil, urlError("archive URL must have a host", rawURL)
	}
	if !client.isTrustedHost(parsedURL) {
		return nil, urlError("unexpected archive host", rawURL)
	}
	if !client.isAcceptableScheme(parsedURL) {
		return nil, urlError("archive URL must use HTTPS", rawURL)
	}
	if !strings.HasPrefix(parsedURL.Path, "/archive/") {
		return nil, urlError("archive URL has an unexpected path", rawURL)
	}
	if strings.HasSuffix(parsedURL.Path, "/") {
		return nil, errNoArchive
	}

	filename := filepath.Base(parsedURL.Path)
	if filename == "." || filename == "/" || filename == "archive" {
		return nil, errNoArchive
	}
	extension := strings.ToLower(filepath.Ext(filename))
	if !isSupportedArchiveExtension(extension) {
		return nil, fmt.Errorf("%w %q", errUnsupportedArchive, extension)
	}
	return parsedURL, nil
}

func urlError(message string, rawURL string) error {
	return fmt.Errorf("%s: %q", message, rawURL)
}

// Download saves archiveURL to outputPath through a temporary file, retrying transient
// failures. A download that receives no data for the client timeout is abandoned and retried.
func (client *Client) Download(ctx context.Context, archiveURL *url.URL, outputPath string, label string) (int64, error) {
	var written int64
	err := client.withRetry(ctx, "download of "+label, func() error {
		var err error
		written, err = client.downloadOnce(ctx, archiveURL, outputPath, label)
		return err
	})
	return written, err
}

func (client *Client) downloadOnce(ctx context.Context, archiveURL *url.URL, outputPath string, label string) (int64, error) {
	downloadCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var stalled atomic.Bool
	watchdog := time.AfterFunc(client.timeout, func() {
		stalled.Store(true)
		cancel()
	})
	defer watchdog.Stop()
	classify := func(what string, err error) error {
		if ctx.Err() != nil {
			return fmt.Errorf("%s: %w", what, ctx.Err())
		}
		if stalled.Load() {
			return &transientError{fmt.Errorf("%s: stalled with no data for %s", what, client.timeout)}
		}
		return &transientError{fmt.Errorf("%s: %w", what, err)}
	}

	request, err := client.newRequest(downloadCtx, archiveURL.String())
	if err != nil {
		return 0, fmt.Errorf("create download request: %w", err)
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		return 0, classify("download file", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return 0, fmt.Errorf("download file: %w", newHTTPError(response))
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return 0, fmt.Errorf("create archive directory: %w", err)
	}
	temporaryFile, err := os.CreateTemp(filepath.Dir(outputPath), "."+filepath.Base(outputPath)+"-*.part")
	if err != nil {
		return 0, fmt.Errorf("create temporary archive: %w", err)
	}
	temporaryPath := temporaryFile.Name()
	defer func() { _ = os.Remove(temporaryPath) }()

	writer := io.Writer(temporaryFile)
	if client.progress && response.ContentLength > 0 {
		bar := pb.New64(response.ContentLength)
		bar.SetWriter(client.output)
		bar.Set(pb.Bytes, true)
		bar.SetWidth(60)
		bar.Set("prefix", label+" ")
		bar.Start()
		defer bar.Finish()
		writer = &progressWriter{bar: bar, writer: temporaryFile}
	}

	body := &watchdogReader{reader: response.Body, watchdog: watchdog, timeout: client.timeout}
	written, copyErr := io.Copy(writer, body)
	closeErr := temporaryFile.Close()
	if copyErr != nil {
		return written, classify("save file", copyErr)
	}
	if closeErr != nil {
		return written, fmt.Errorf("close temporary archive: %w", closeErr)
	}
	if response.ContentLength >= 0 && written != response.ContentLength {
		return written, &transientError{fmt.Errorf("incomplete download: received %d of %d bytes", written, response.ContentLength)}
	}
	if modified, err := http.ParseTime(response.Header.Get("Last-Modified")); err == nil {
		_ = os.Chtimes(temporaryPath, modified, modified)
	}
	if err := os.Rename(temporaryPath, outputPath); err != nil {
		return written, fmt.Errorf("commit archive: %w", err)
	}
	return written, nil
}

// watchdogReader pushes the stall timer forward every time data arrives.
type watchdogReader struct {
	reader   io.Reader
	watchdog *time.Timer
	timeout  time.Duration
}

func (reader *watchdogReader) Read(data []byte) (int, error) {
	read, err := reader.reader.Read(data)
	if read > 0 {
		reader.watchdog.Reset(reader.timeout)
	}
	return read, err
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
