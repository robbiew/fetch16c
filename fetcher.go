package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const manifestFilename = ".fetch16c.json"

type Config struct {
	Years        int
	Year         int
	FromYear     int
	ToYear       int
	Path         string
	Pack         string
	SkipExisting bool
	Overwrite    bool
	KeepArchives bool
	Quiet        bool
	Concurrency  int
	Timeout      time.Duration
}

func (config Config) Validate(now time.Time) error {
	if config.Years < 1 {
		return errors.New("--years must be at least 1")
	}
	if config.Year < 0 || config.FromYear < 0 || config.ToYear < 0 {
		return errors.New("years cannot be negative")
	}
	if config.FromYear != 0 && config.ToYear != 0 && config.FromYear > config.ToYear {
		return errors.New("--from-year cannot be newer than --to-year")
	}
	if config.Path == "" {
		return errors.New("--path cannot be empty")
	}
	if config.Concurrency < 1 {
		return errors.New("--concurrency must be at least 1")
	}
	if config.Timeout <= 0 {
		return errors.New("--timeout must be greater than zero")
	}
	_, err := config.SelectedYears(now)
	return err
}

func (config Config) SelectedYears(now time.Time) ([]int, error) {
	currentYear := now.Year()
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

type Summary struct {
	Fetched         int
	Skipped         int
	Failed          int
	DownloadedBytes int64
}

func (summary Summary) Print(writer io.Writer) {
	fmt.Fprintf(writer, "Summary: fetched=%d skipped=%d failed=%d downloaded=%s\n",
		summary.Fetched, summary.Skipped, summary.Failed, formatBytes(summary.DownloadedBytes))
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
	logMutex sync.Mutex
}

func NewFetcher(client *Client, config Config, output io.Writer) *Fetcher {
	return &Fetcher{client: client, config: config, output: output, lookPath: exec.LookPath}
}

type packJob struct {
	year       int
	result     Result
	archiveURL *url.URL
	extension  string
}

type packOutcome struct {
	status string
	bytes  int64
	err    error
}

func (fetcher *Fetcher) Run(ctx context.Context) (Summary, error) {
	years, err := fetcher.config.SelectedYears(time.Now())
	if err != nil {
		return Summary{}, err
	}

	var jobs []packJob
	var failures []string
	matchedPack := false
	for _, year := range years {
		response, err := fetcher.client.FetchYear(ctx, year)
		if err != nil {
			failures = append(failures, fmt.Sprintf("year %d: %v", year, err))
			continue
		}
		for _, result := range response.Results {
			if fetcher.config.Pack != "" && !strings.EqualFold(fetcher.config.Pack, result.Name) {
				continue
			}
			matchedPack = true
			job, err := fetcher.makeJob(year, result)
			if err != nil {
				failures = append(failures, fmt.Sprintf("year %d pack %q: %v", year, displayPackName(result.Name), err))
				continue
			}
			jobs = append(jobs, job)
		}
	}
	if fetcher.config.Pack != "" && !matchedPack {
		failures = append(failures, fmt.Sprintf("pack %q was not found in the selected years", fetcher.config.Pack))
	}

	lhaPath := ""
	hasLHA := false
	for _, job := range jobs {
		if job.extension == ".lha" || job.extension == ".lzh" {
			hasLHA = true
			break
		}
	}
	if hasLHA {
		lhaPath, err = fetcher.lookPath("lha")
		if err != nil {
			remainingJobs := jobs[:0]
			for _, job := range jobs {
				if job.extension == ".lha" || job.extension == ".lzh" {
					failures = append(failures, fmt.Sprintf("year %d pack %q: lha command is required", job.year, job.result.Name))
					continue
				}
				remainingJobs = append(remainingJobs, job)
			}
			jobs = remainingJobs
		}
	}

	summary := Summary{Failed: len(failures)}
	outcomes := fetcher.processJobs(ctx, jobs, lhaPath)
	for outcome := range outcomes {
		summary.DownloadedBytes += outcome.bytes
		switch outcome.status {
		case "fetched":
			summary.Fetched++
		case "skipped":
			summary.Skipped++
		case "failed":
			summary.Failed++
			failures = append(failures, outcome.err.Error())
		}
	}
	if ctx.Err() != nil {
		summary.Failed++
		failures = append(failures, fmt.Sprintf("fetch canceled: %v", ctx.Err()))
	}
	if len(failures) == 0 {
		return summary, nil
	}
	sort.Strings(failures)
	return summary, errors.New(strings.Join(failures, "\n"))
}

func (fetcher *Fetcher) makeJob(year int, result Result) (packJob, error) {
	if err := validatePackName(result.Name); err != nil {
		return packJob{}, err
	}
	archiveURL, err := fetcher.client.ValidateArchiveURL(result.Download)
	if err != nil {
		return packJob{}, err
	}
	return packJob{
		year:       year,
		result:     result,
		archiveURL: archiveURL,
		extension:  strings.ToLower(filepath.Ext(archiveURL.Path)),
	}, nil
}

func validatePackName(name string) error {
	if name == "" || name == "." || name == ".." || filepath.IsAbs(name) || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) {
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

func (fetcher *Fetcher) processJobs(ctx context.Context, jobs []packJob, lhaPath string) <-chan packOutcome {
	jobChannel := make(chan packJob)
	outcomeChannel := make(chan packOutcome)
	workers := fetcher.config.Concurrency
	if workers > len(jobs) {
		workers = len(jobs)
	}

	var waitGroup sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for job := range jobChannel {
				outcomeChannel <- fetcher.processJob(ctx, job, lhaPath)
			}
		}()
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

func (fetcher *Fetcher) processJob(ctx context.Context, job packJob, lhaPath string) packOutcome {
	yearPath := filepath.Join(fetcher.config.Path, fmt.Sprintf("%d", job.year))
	outputDir := filepath.Join(yearPath, job.result.Name)
	if err := ensureContainedPath(yearPath, outputDir); err != nil {
		return failedOutcome(job, 0, err)
	}
	if err := os.MkdirAll(yearPath, 0o755); err != nil {
		return failedOutcome(job, 0, fmt.Errorf("create year directory: %w", err))
	}

	skipped, err := fetcher.shouldSkip(outputDir, job.archiveURL.String())
	if err != nil {
		return failedOutcome(job, 0, err)
	}
	if skipped {
		fetcher.logf("Skipped %d/%s (already complete)\n", job.year, job.result.Name)
		return packOutcome{status: "skipped"}
	}

	archivePath := filepath.Join(yearPath, filepath.Base(job.archiveURL.Path))
	fetcher.logf("Downloading %d/%s\n", job.year, job.result.Name)
	downloadedBytes, err := fetcher.client.Download(ctx, job.archiveURL, archivePath, job.result.Name)
	if err != nil {
		return failedOutcome(job, downloadedBytes, err)
	}

	stagingDir, err := os.MkdirTemp(yearPath, ".fetch16c-*")
	if err != nil {
		fetcher.removeArchive(archivePath)
		return failedOutcome(job, downloadedBytes, fmt.Errorf("create extraction directory: %w", err))
	}
	defer os.RemoveAll(stagingDir)

	if err := extractArchive(ctx, archivePath, stagingDir, lhaPath); err != nil {
		fetcher.removeArchive(archivePath)
		return failedOutcome(job, downloadedBytes, err)
	}
	manifest := Manifest{
		Version:     1,
		Complete:    true,
		Year:        job.year,
		Name:        job.result.Name,
		ArchiveURL:  job.archiveURL.String(),
		ArchiveSize: downloadedBytes,
		CompletedAt: time.Now().UTC(),
	}
	if err := writeManifest(stagingDir, manifest); err != nil {
		fetcher.removeArchive(archivePath)
		return failedOutcome(job, downloadedBytes, err)
	}
	if !fetcher.config.KeepArchives {
		if err := os.Remove(archivePath); err != nil {
			return failedOutcome(job, downloadedBytes, fmt.Errorf("remove archive: %w", err))
		}
	}
	if err := commitDirectory(stagingDir, outputDir, fetcher.config.Overwrite); err != nil {
		return failedOutcome(job, downloadedBytes, err)
	}

	fetcher.logf("Extracted %d/%s\n", job.year, job.result.Name)
	return packOutcome{status: "fetched", bytes: downloadedBytes}
}

func (fetcher *Fetcher) shouldSkip(outputDir string, archiveURL string) (bool, error) {
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
		return false, fmt.Errorf("output path is a symbolic link; use --overwrite to replace it")
	}
	if !info.IsDir() {
		return false, fmt.Errorf("output path exists and is not a directory; use --overwrite to replace it")
	}
	if !fetcher.config.SkipExisting {
		return false, fmt.Errorf("output directory exists; use --overwrite to replace it")
	}

	manifest, err := readManifest(outputDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, fmt.Errorf("output directory has no completion manifest; use --overwrite once to adopt it")
		}
		return false, fmt.Errorf("read completion manifest: %w", err)
	}
	if !manifest.Complete || manifest.ArchiveURL != archiveURL {
		return false, fmt.Errorf("output directory has a stale completion manifest; use --overwrite to replace it")
	}
	return true, nil
}

func (fetcher *Fetcher) removeArchive(archivePath string) {
	if !fetcher.config.KeepArchives {
		_ = os.Remove(archivePath)
	}
}

func (fetcher *Fetcher) logf(format string, values ...interface{}) {
	if fetcher.config.Quiet {
		return
	}
	fetcher.logMutex.Lock()
	defer fetcher.logMutex.Unlock()
	fmt.Fprintf(fetcher.output, format, values...)
}

func failedOutcome(job packJob, downloadedBytes int64, err error) packOutcome {
	return packOutcome{
		status: "failed",
		bytes:  downloadedBytes,
		err:    fmt.Errorf("year %d pack %q: %w", job.year, displayPackName(job.result.Name), err),
	}
}

func ensureContainedPath(root string, candidate string) error {
	relativePath, err := filepath.Rel(root, candidate)
	if err != nil || relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(os.PathSeparator)) {
		return errors.New("path escapes its output directory")
	}
	return nil
}

type Manifest struct {
	Version     int       `json:"version"`
	Complete    bool      `json:"complete"`
	Year        int       `json:"year"`
	Name        string    `json:"name"`
	ArchiveURL  string    `json:"archive_url"`
	ArchiveSize int64     `json:"archive_size"`
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

	backupDir, err := os.MkdirTemp(filepath.Dir(outputDir), ".fetch16c-backup-*")
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
