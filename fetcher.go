package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	manifestFilename = ".fetch16c.json"
	manifestVersion  = 2
	stagingPrefix    = ".fetch16c-staging-"
	backupPrefix     = ".fetch16c-backup-"
)

// Pack outcome statuses.
const (
	statusFetched     = "fetched"
	statusSkipped     = "skipped"
	statusUnavailable = "unavailable"
	statusUnsupported = "unsupported"
	statusFailed      = "failed"
)

type Config struct {
	Years        int
	Year         int
	FromYear     int
	ToYear       int
	AllYears     bool
	Type         string
	Path         string
	Packs        []string
	Groups       []string
	Extensions   []string
	SkipExisting bool
	Overwrite    bool
	KeepArchives bool
	Quiet        bool
	DryRun       bool
	Concurrency  int
	Retries      int
	Timeout      time.Duration
}

func (config Config) Validate(now time.Time) error {
	if config.Years < 1 {
		return errors.New("--years must be at least 1")
	}
	if config.Year < 0 || config.FromYear < 0 || config.ToYear < 0 {
		return errors.New("years cannot be negative")
	}
	if config.AllYears && (config.Year != 0 || config.FromYear != 0 || config.ToYear != 0) {
		return errors.New("--all cannot be combined with --year, --from-year, or --to-year")
	}
	if config.FromYear != 0 && config.ToYear != 0 && config.FromYear > config.ToYear {
		return errors.New("--from-year cannot be newer than --to-year")
	}
	if config.Type != "" && config.Type != TypePacks && config.Type != TypeMags && config.Type != TypeAll {
		return fmt.Errorf("--type must be %s, %s, or %s", TypePacks, TypeMags, TypeAll)
	}
	if config.Path == "" {
		return errors.New("--path cannot be empty")
	}
	for _, pattern := range config.Packs {
		if strings.TrimSpace(pattern) == "" {
			return errors.New("--pack cannot be empty")
		}
		if _, err := path.Match(pattern, ""); err != nil {
			return fmt.Errorf("--pack pattern %q is invalid: %w", pattern, err)
		}
	}
	for _, group := range config.Groups {
		if strings.TrimSpace(group) == "" {
			return errors.New("--group cannot be empty")
		}
	}
	if config.Concurrency < 1 {
		return errors.New("--concurrency must be at least 1")
	}
	if config.Retries < 0 {
		return errors.New("--retries cannot be negative")
	}
	if config.Timeout <= 0 {
		return errors.New("--timeout must be greater than zero")
	}
	_, err := config.SelectedYears(now)
	return err
}

// SelectedYears returns the years to fetch, newest first. It returns nil when every year is
// selected, because that list comes from the API.
func (config Config) SelectedYears(now time.Time) ([]int, error) {
	currentYear := now.Year()
	if config.AllYears {
		return nil, nil
	}
	if config.Year != 0 {
		return []int{config.Year}, nil
	}
	if config.FromYear != 0 || config.ToYear != 0 {
		fromYear := config.FromYear
		toYear := config.ToYear
		if fromYear == 0 {
			fromYear = toYear
		}
		if toYear == 0 {
			toYear = currentYear
		}
		if fromYear > toYear {
			return nil, errors.New("--from-year cannot be newer than --to-year")
		}
		years := make([]int, 0, toYear-fromYear+1)
		for year := toYear; year >= fromYear; year-- {
			years = append(years, year)
		}
		return years, nil
	}

	years := make([]int, config.Years)
	for index := range years {
		years[index] = currentYear - index
	}
	return years, nil
}

// PackTypes lists the listing types to query for the configured --type.
func (config Config) PackTypes() []string {
	switch config.Type {
	case TypeMags:
		return []string{TypeMags}
	case TypeAll:
		return []string{TypePacks, TypeMags}
	default:
		return []string{TypePacks}
	}
}

// FileFilter returns the extraction filter implied by --extensions, or nil for everything.
func (config Config) FileFilter() fileFilter {
	return extensionFilter(config.Extensions)
}

type Summary struct {
	Fetched         int
	Skipped         int
	Unavailable     int
	Unsupported     int
	Failed          int
	DownloadedBytes int64
}

func (summary Summary) Print(writer io.Writer) {
	fmt.Fprintf(writer, "Summary: fetched=%d skipped=%d unavailable=%d unsupported=%d failed=%d downloaded=%s\n",
		summary.Fetched, summary.Skipped, summary.Unavailable, summary.Unsupported, summary.Failed, formatBytes(summary.DownloadedBytes))
}

func (summary *Summary) count(outcome packOutcome) {
	summary.DownloadedBytes += outcome.bytes
	switch outcome.status {
	case statusFetched:
		summary.Fetched++
	case statusSkipped:
		summary.Skipped++
	case statusUnavailable:
		summary.Unavailable++
	case statusUnsupported:
		summary.Unsupported++
	case statusFailed:
		summary.Failed++
	}
}

func formatBytes(bytes int64) string {
	const unit = int64(1024)
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	divisor, exponent := unit, 0
	for value := bytes / unit; value >= unit; value /= unit {
		divisor *= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(divisor), "KMGTPE"[exponent])
}

type Fetcher struct {
	client   *Client
	config   Config
	output   io.Writer
	lookPath func(string) (string, error)
	now      func() time.Time
	logMutex sync.Mutex
}

func NewFetcher(client *Client, config Config, output io.Writer) *Fetcher {
	fetcher := &Fetcher{client: client, config: config, output: output, lookPath: exec.LookPath, now: time.Now}
	client.SetLogger(fetcher.logf)
	return fetcher
}

type packJob struct {
	result     Result
	archiveURL *url.URL
	// alternateURLs are tried, in order, when the listed archive URL is not found.
	alternateURLs []*url.URL
	extension     string
	extractor     *resolvedExtractor
	// status is set when the job was classified before any download: unavailable,
	// unsupported, or failed. Jobs with an empty status are ready to fetch.
	status string
	reason error
}

func (job packJob) label() string {
	return fmt.Sprintf("%d/%s", job.result.Year, displayPackName(job.result.Name))
}

func (job packJob) candidateURLs() []*url.URL {
	return append([]*url.URL{job.archiveURL}, job.alternateURLs...)
}

func (job packJob) candidateURLStrings() []string {
	var urls []string
	for _, candidate := range job.candidateURLs() {
		urls = append(urls, candidate.String())
	}
	return urls
}

// alternateArchiveURLs guesses where an archive lives when the listed link is wrong. Archives
// whose file names contain punctuation such as '#', '$', '~', or spaces are often stored under
// the plain pack name instead.
func alternateArchiveURLs(primary *url.URL, result Result) []*url.URL {
	candidate := *primary
	candidate.Path = path.Join(path.Dir(primary.Path), result.Name+path.Ext(primary.Path))
	candidate.RawPath = ""
	if candidate.Path == primary.Path {
		return nil
	}
	return []*url.URL{&candidate}
}

func isNotFound(err error) bool {
	var statusErr *httpError
	return errors.As(err, &statusErr) && (statusErr.status == http.StatusNotFound || statusErr.status == http.StatusGone)
}

type packOutcome struct {
	status string
	bytes  int64
	err    error
}

func (fetcher *Fetcher) Run(ctx context.Context) (Summary, error) {
	years, err := fetcher.selectYears(ctx)
	if err != nil {
		return Summary{}, err
	}

	jobs, failures := fetcher.collectJobs(ctx, years)
	if ctx.Err() == nil && !fetcher.config.DryRun {
		for _, yearPath := range fetcher.yearPaths(jobs) {
			if err := cleanupStagingArtifacts(yearPath); err != nil {
				failures = append(failures, fmt.Sprintf("clean %s: %v", yearPath, err))
			}
		}
	}

	var summary Summary
	var ready []packJob
	for _, job := range jobs {
		if job.status == "" {
			ready = append(ready, job)
			continue
		}
		outcome := fetcher.reportClassified(job)
		summary.count(outcome)
		if outcome.err != nil {
			failures = append(failures, outcome.err.Error())
		}
	}

	if fetcher.config.DryRun {
		if !fetcher.config.Quiet && len(ready) > 0 {
			fetcher.logf("Dry run: nothing will be downloaded.\n")
		}
		for _, job := range ready {
			outcome := fetcher.planJob(job)
			summary.count(outcome)
			if outcome.err != nil {
				failures = append(failures, outcome.err.Error())
			}
		}
	} else {
		for outcome := range fetcher.processJobs(ctx, ready) {
			summary.count(outcome)
			if outcome.err != nil {
				failures = append(failures, outcome.err.Error())
			}
		}
	}

	if ctx.Err() != nil {
		summary.Failed++
		failures = append(failures, fmt.Sprintf("fetch canceled: %v", ctx.Err()))
	}
	if len(failures) == 0 {
		return summary, nil
	}
	slices.Sort(failures)
	return summary, errors.New(strings.Join(failures, "\n"))
}

func (fetcher *Fetcher) selectYears(ctx context.Context) ([]int, error) {
	years, err := fetcher.config.SelectedYears(fetcher.now())
	if err != nil {
		return nil, err
	}
	if years != nil {
		return years, nil
	}
	return fetcher.client.ListYears(ctx)
}

// collectJobs lists the selected years and turns every matching pack into a job.
func (fetcher *Fetcher) collectJobs(ctx context.Context, years []int) ([]packJob, []string) {
	var jobs []packJob
	var failures []string
	matchedPatterns := make(map[string]bool)
	extractors := make(map[string]*resolvedExtractor)
	extractorErrors := make(map[string]error)

listing:
	for _, year := range years {
		for _, packType := range fetcher.config.PackTypes() {
			if ctx.Err() != nil {
				break listing
			}
			response, err := fetcher.client.FetchYear(ctx, year, packType)
			if err != nil {
				if ctx.Err() != nil {
					break listing
				}
				failures = append(failures, fmt.Sprintf("year %d: %v", year, err))
				continue
			}
			for _, result := range response.Results {
				if result.Year == 0 {
					result.Year = year
				}
				if !fetcher.selects(result, matchedPatterns) {
					continue
				}
				job := fetcher.classify(result)
				if job.status == "" && job.extension != ".zip" {
					if _, seen := extractors[job.extension]; !seen {
						extractors[job.extension], extractorErrors[job.extension] = findExtractor(fetcher.lookPath, job.extension)
					}
					if extractorErrors[job.extension] != nil {
						job.status, job.reason = statusUnsupported, extractorErrors[job.extension]
					} else {
						job.extractor = extractors[job.extension]
					}
				}
				jobs = append(jobs, job)
			}
		}
	}
	for _, pattern := range fetcher.config.Packs {
		if !matchedPatterns[strings.ToLower(pattern)] {
			failures = append(failures, fmt.Sprintf("pack %q was not found in the selected years", pattern))
		}
	}
	return jobs, failures
}

// selects applies the --pack and --group filters.
func (fetcher *Fetcher) selects(result Result, matchedPatterns map[string]bool) bool {
	if len(fetcher.config.Groups) > 0 {
		inGroup := slices.ContainsFunc(result.Groups, func(group string) bool {
			return slices.ContainsFunc(fetcher.config.Groups, func(wanted string) bool { return strings.EqualFold(group, wanted) })
		})
		if !inGroup {
			return false
		}
	}
	if len(fetcher.config.Packs) == 0 {
		return true
	}
	selected := false
	for _, pattern := range fetcher.config.Packs {
		if matchesPackPattern(pattern, result.Name) {
			matchedPatterns[strings.ToLower(pattern)] = true
			selected = true
		}
	}
	return selected
}

// matchesPackPattern compares a pack name to a --pack value without regard to case. Values
// containing *, ?, or [ are shell-style patterns; anything else must match exactly.
func matchesPackPattern(pattern string, name string) bool {
	pattern = strings.ToLower(pattern)
	name = strings.ToLower(name)
	if !strings.ContainsAny(pattern, `*?[`) {
		return pattern == name
	}
	matched, err := path.Match(pattern, name)
	return err == nil && matched
}

// classify validates a listing entry and marks it unavailable, unsupported, or failed when it
// cannot be fetched. Fetchable entries get their archive URL and extension.
func (fetcher *Fetcher) classify(result Result) packJob {
	job := packJob{result: result}
	if err := validatePackName(result.Name); err != nil {
		job.status, job.reason = statusFailed, err
		return job
	}
	archiveURL, err := fetcher.client.ValidateArchiveURL(result.Download)
	switch {
	case errors.Is(err, errNoArchive):
		job.status, job.reason = statusUnavailable, err
	case errors.Is(err, errUnsupportedArchive):
		job.status, job.reason = statusUnsupported, err
	case err != nil:
		job.status, job.reason = statusFailed, err
	default:
		job.archiveURL = archiveURL
		job.alternateURLs = alternateArchiveURLs(archiveURL, result)
		job.extension = strings.ToLower(filepath.Ext(archiveURL.Path))
	}
	return job
}

func validatePackName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.IsAbs(name) || filepath.Base(name) != name || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("unsafe pack name %q", name)
	}
	return nil
}

func displayPackName(name string) string {
	if name == "" {
		return "<unnamed>"
	}
	return name
}

func (fetcher *Fetcher) yearPaths(jobs []packJob) []string {
	var paths []string
	for _, job := range jobs {
		if job.status != "" {
			continue
		}
		yearPath := fetcher.yearPath(job.result.Year)
		if !slices.Contains(paths, yearPath) {
			paths = append(paths, yearPath)
		}
	}
	return paths
}

func (fetcher *Fetcher) yearPath(year int) string {
	return filepath.Join(fetcher.config.Path, fmt.Sprintf("%d", year))
}

// reportClassified logs and converts a job that was classified before downloading.
func (fetcher *Fetcher) reportClassified(job packJob) packOutcome {
	switch job.status {
	case statusUnavailable:
		fetcher.logf("Unavailable %s (%v)\n", job.label(), job.reason)
		return packOutcome{status: statusUnavailable}
	case statusUnsupported:
		fetcher.logf("Unsupported %s (%v)\n", job.label(), job.reason)
		return packOutcome{status: statusUnsupported}
	default:
		return failedOutcome(job, 0, job.reason)
	}
}

// planJob reports what a real run would do with a ready job.
func (fetcher *Fetcher) planJob(job packJob) packOutcome {
	outputDir := filepath.Join(fetcher.yearPath(job.result.Year), job.result.Name)
	skipped, err := fetcher.shouldSkip(outputDir, job.candidateURLStrings())
	if err != nil {
		fetcher.logf("Would fail %s (%v)\n", job.label(), err)
		return failedOutcome(job, 0, err)
	}
	if skipped {
		fetcher.logf("Would skip %s (already complete)\n", job.label())
		return packOutcome{status: statusSkipped}
	}
	fetcher.logf("Would fetch %s (%s)\n", job.label(), path.Base(job.archiveURL.Path))
	return packOutcome{status: statusFetched}
}

func (fetcher *Fetcher) processJobs(ctx context.Context, jobs []packJob) <-chan packOutcome {
	jobChannel := make(chan packJob)
	outcomeChannel := make(chan packOutcome)
	workers := min(fetcher.config.Concurrency, len(jobs))

	var waitGroup sync.WaitGroup
	for range workers {
		waitGroup.Go(func() {
			for job := range jobChannel {
				outcomeChannel <- fetcher.processJob(ctx, job)
			}
		})
	}
	go func() {
		defer close(jobChannel)
		for _, job := range jobs {
			select {
			case jobChannel <- job:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		waitGroup.Wait()
		close(outcomeChannel)
	}()
	return outcomeChannel
}

func (fetcher *Fetcher) processJob(ctx context.Context, job packJob) packOutcome {
	yearPath := fetcher.yearPath(job.result.Year)
	outputDir := filepath.Join(yearPath, job.result.Name)
	if err := ensureContainedPath(yearPath, outputDir); err != nil {
		return failedOutcome(job, 0, err)
	}
	if err := os.MkdirAll(yearPath, 0o755); err != nil {
		return failedOutcome(job, 0, fmt.Errorf("create year directory: %w", err))
	}

	skipped, err := fetcher.shouldSkip(outputDir, job.candidateURLStrings())
	if err != nil {
		return failedOutcome(job, 0, err)
	}
	if skipped {
		fetcher.logf("Skipped %s (already complete)\n", job.label())
		return packOutcome{status: statusSkipped}
	}

	archivePath := filepath.Join(yearPath, job.result.Name+"-"+path.Base(job.archiveURL.Path))
	fetcher.logf("Downloading %s\n", job.label())
	archiveURL, downloadedBytes, err := fetcher.download(ctx, job, archivePath)
	if isNotFound(err) {
		fetcher.logf("Unavailable %s (archive not found on server)\n", job.label())
		return packOutcome{status: statusUnavailable}
	}
	if err != nil {
		return failedOutcome(job, downloadedBytes, err)
	}

	stagingDir, err := os.MkdirTemp(yearPath, stagingPrefix+"*")
	if err != nil {
		fetcher.removeArchive(archivePath)
		return failedOutcome(job, downloadedBytes, fmt.Errorf("create extraction directory: %w", err))
	}
	defer func() { _ = os.RemoveAll(stagingDir) }()

	extracted, err := extractArchive(ctx, archivePath, stagingDir, job.extractor, fetcher.config.FileFilter())
	if err != nil {
		fetcher.removeArchive(archivePath)
		return failedOutcome(job, downloadedBytes, err)
	}
	fileCount, err := countFiles(stagingDir)
	if err != nil {
		fetcher.removeArchive(archivePath)
		return failedOutcome(job, downloadedBytes, err)
	}
	manifest := Manifest{
		Version:     manifestVersion,
		Complete:    true,
		Year:        job.result.Year,
		Name:        job.result.Name,
		Gallery:     job.result.Gallery,
		Groups:      job.result.Groups,
		ArchiveURL:  archiveURL.String(),
		ArchiveName: path.Base(job.archiveURL.Path),
		ArchiveSize: downloadedBytes,
		FileCount:   fileCount,
		Damaged:     extracted.Damaged,
		Extensions:  normalizeExtensions(fetcher.config.Extensions),
		CompletedAt: fetcher.now().UTC(),
	}
	if err := writeManifest(stagingDir, manifest); err != nil {
		fetcher.removeArchive(archivePath)
		return failedOutcome(job, downloadedBytes, err)
	}
	if err := commitDirectory(stagingDir, outputDir, fetcher.config.Overwrite); err != nil {
		return failedOutcome(job, downloadedBytes, err)
	}
	if !fetcher.config.KeepArchives {
		if err := os.Remove(archivePath); err != nil {
			return failedOutcome(job, downloadedBytes, fmt.Errorf("remove archive: %w", err))
		}
	}

	if len(extracted.Damaged) > 0 {
		fetcher.logf("Extracted %s (%d files; skipped %d damaged: %s)\n", job.label(), fileCount, len(extracted.Damaged), strings.Join(extracted.Damaged, ", "))
	} else {
		fetcher.logf("Extracted %s (%d files)\n", job.label(), fileCount)
	}
	return packOutcome{status: statusFetched, bytes: downloadedBytes}
}

// download fetches the first candidate archive URL that exists and reports which one it used.
// A not-found error is returned only when every candidate is missing.
func (fetcher *Fetcher) download(ctx context.Context, job packJob, archivePath string) (*url.URL, int64, error) {
	candidates := job.candidateURLs()
	for index, candidate := range candidates {
		if index > 0 {
			fetcher.logf("Trying alternate archive %s for %s\n", path.Base(candidate.Path), job.label())
		}
		downloadedBytes, err := fetcher.client.Download(ctx, candidate, archivePath, job.result.Name)
		if err == nil {
			return candidate, downloadedBytes, nil
		}
		if !isNotFound(err) || index == len(candidates)-1 {
			return candidate, downloadedBytes, err
		}
	}
	return nil, 0, errors.New("no archive URL candidates")
}

// shouldSkip reports whether outputDir already holds a complete copy of one of the candidate
// archive URLs, extracted with the configured file filter.
func (fetcher *Fetcher) shouldSkip(outputDir string, archiveURLs []string) (bool, error) {
	info, err := os.Lstat(outputDir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect output directory: %w", err)
	}
	if fetcher.config.Overwrite {
		return false, nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("output path is a symbolic link; use --overwrite to replace it")
	}
	if !info.IsDir() {
		return false, errors.New("output path exists and is not a directory; use --overwrite to replace it")
	}
	if !fetcher.config.SkipExisting {
		return false, errors.New("output directory exists; use --overwrite to replace it")
	}

	manifest, err := readManifest(outputDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, errors.New("output directory has no completion manifest; use --overwrite once to adopt it")
		}
		return false, fmt.Errorf("read completion manifest: %w", err)
	}
	if !manifest.Complete || !slices.Contains(archiveURLs, manifest.ArchiveURL) {
		return false, errors.New("output directory has a stale completion manifest; use --overwrite to replace it")
	}
	if !slices.Equal(manifest.Extensions, normalizeExtensions(fetcher.config.Extensions)) {
		return false, errors.New("output directory was extracted with a different file filter; use --overwrite to replace it")
	}
	return true, nil
}

func (fetcher *Fetcher) removeArchive(archivePath string) {
	if !fetcher.config.KeepArchives {
		_ = os.Remove(archivePath)
	}
}

func (fetcher *Fetcher) logf(format string, values ...any) {
	if fetcher.config.Quiet {
		return
	}
	fetcher.logMutex.Lock()
	defer fetcher.logMutex.Unlock()
	fmt.Fprintf(fetcher.output, format, values...)
}

func failedOutcome(job packJob, downloadedBytes int64, err error) packOutcome {
	return packOutcome{
		status: statusFailed,
		bytes:  downloadedBytes,
		err:    fmt.Errorf("year %d pack %q: %w", job.result.Year, displayPackName(job.result.Name), err),
	}
}

func ensureContainedPath(root string, candidate string) error {
	relativePath, err := filepath.Rel(root, candidate)
	if err != nil || relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(os.PathSeparator)) {
		return errors.New("path escapes its output directory")
	}
	return nil
}

func countFiles(root string) (int, error) {
	count := 0
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			count++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("count extracted files: %w", err)
	}
	return count, nil
}

// cleanupStagingArtifacts removes staging directories and partial downloads that an earlier,
// interrupted run left in a year directory. Replacement backups are kept.
func cleanupStagingArtifacts(yearPath string) error {
	entries, err := os.ReadDir(yearPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case entry.IsDir() && strings.HasPrefix(name, stagingPrefix):
			if err := os.RemoveAll(filepath.Join(yearPath, name)); err != nil {
				return err
			}
		case !entry.IsDir() && strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".part"):
			if err := os.Remove(filepath.Join(yearPath, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Manifest records a completed extraction so later runs can skip or detect stale packs.
type Manifest struct {
	Version     int       `json:"version"`
	Complete    bool      `json:"complete"`
	Year        int       `json:"year"`
	Name        string    `json:"name"`
	Gallery     string    `json:"gallery,omitempty"`
	Groups      []string  `json:"groups,omitempty"`
	ArchiveURL  string    `json:"archive_url"`
	ArchiveName string    `json:"archive_name,omitempty"`
	ArchiveSize int64     `json:"archive_size"`
	FileCount   int       `json:"file_count,omitempty"`
	Damaged     []string  `json:"damaged_files,omitempty"`
	Extensions  []string  `json:"extensions,omitempty"`
	CompletedAt time.Time `json:"completed_at"`
}

func writeManifest(outputDir string, manifest Manifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode completion manifest: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(outputDir, manifestFilename), data, 0o644); err != nil {
		return fmt.Errorf("write completion manifest: %w", err)
	}
	return nil
}

func readManifest(outputDir string) (Manifest, error) {
	var manifest Manifest
	data, err := os.ReadFile(filepath.Join(outputDir, manifestFilename))
	if err != nil {
		return manifest, err
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, err
	}
	return manifest, nil
}

func commitDirectory(stagingDir string, outputDir string, overwrite bool) error {
	_, err := os.Lstat(outputDir)
	if os.IsNotExist(err) {
		if err := os.Rename(stagingDir, outputDir); err != nil {
			return fmt.Errorf("commit extracted pack: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect existing pack: %w", err)
	}
	if !overwrite {
		return errors.New("output directory already exists")
	}

	backupDir, err := os.MkdirTemp(filepath.Dir(outputDir), backupPrefix+"*")
	if err != nil {
		return fmt.Errorf("create replacement backup: %w", err)
	}
	if err := os.Remove(backupDir); err != nil {
		return fmt.Errorf("prepare replacement backup: %w", err)
	}
	if err := os.Rename(outputDir, backupDir); err != nil {
		return fmt.Errorf("back up existing pack: %w", err)
	}
	if err := os.Rename(stagingDir, outputDir); err != nil {
		_ = os.Rename(backupDir, outputDir)
		return fmt.Errorf("commit replacement pack: %w", err)
	}
	if err := os.RemoveAll(backupDir); err != nil {
		return fmt.Errorf("remove replacement backup: %w", err)
	}
	return nil
}
