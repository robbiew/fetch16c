package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
)

const (
	defaultAPIBaseURL = "https://api.16colo.rs/v1/"
	projectURL        = "https://github.com/robbiew/fetch16c"
	exitInterrupted   = 130
)

// version is overridden at release time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(runMain())
}

func runMain() int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		select {
		case <-signals:
			fmt.Fprintln(os.Stderr, "fetch16c: interrupted; finishing in-flight packs (press Ctrl-C again to quit now)")
			cancel()
			// Restore default handling so a second signal quits immediately.
			signal.Stop(signals)
		case <-ctx.Done():
		}
	}()
	return execute(ctx, os.Args[1:], os.Stdout, os.Stderr, defaultAPIBaseURL)
}

func execute(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer, apiBaseURL string) int {
	config, showVersion, err := parseConfig(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		return 2
	}
	if showVersion {
		fmt.Fprintf(stdout, "fetch16c %s (%s %s/%s)\n", resolveVersion(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return 0
	}

	if !config.Quiet {
		fmt.Fprintf(stdout, "Fetch16c %s by robbiew, aka aLPHA64.\n", resolveVersion())
		fmt.Fprintln(stdout, projectURL)
	}

	client, err := NewClient(apiBaseURL, ClientOptions{
		Timeout:   config.Timeout,
		Retries:   config.Retries,
		Output:    stdout,
		Progress:  !config.Quiet && config.Concurrency == 1,
		UserAgent: fmt.Sprintf("fetch16c/%s (+%s)", resolveVersion(), projectURL),
	})
	if err != nil {
		fmt.Fprintf(stderr, "fetch16c: %v\n", err)
		return 1
	}

	fetcher := NewFetcher(client, config, stdout)
	summary, runErr := fetcher.Run(ctx)
	summary.Print(stdout)
	if summary.Unsupported > 0 {
		fmt.Fprintf(stderr, "fetch16c: %d pack(s) were skipped because their archive format cannot be extracted on this system; install lha, 7z, or unar for .lha, .lzh, and .arj packs\n", summary.Unsupported)
	}
	if runErr != nil {
		fmt.Fprintf(stderr, "fetch16c: %v\n", runErr)
		if ctx.Err() != nil {
			return exitInterrupted
		}
		return 1
	}
	return 0
}

// resolveVersion prefers the release version baked in at build time and falls back to the
// module version recorded by go install.
func resolveVersion() string {
	if version != "" && version != "dev" {
		return strings.TrimPrefix(version, "v")
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	// go install records the tagged version; local builds record a pseudo-version.
	if main := info.Main.Version; main != "" && main != "(devel)" && !strings.HasPrefix(main, "v0.0.0-") {
		return strings.TrimPrefix(main, "v")
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && len(setting.Value) >= 7 {
			return version + "-" + setting.Value[:7]
		}
	}
	return version
}

// stringList collects repeatable, comma-separated flag values.
type stringList []string

func (list *stringList) String() string {
	return strings.Join(*list, ",")
}

func (list *stringList) Set(value string) error {
	for part := range strings.SplitSeq(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			*list = append(*list, part)
		}
	}
	return nil
}

func parseConfig(args []string, stderr io.Writer) (Config, bool, error) {
	config := Config{}
	showVersion := false
	artOnly := false
	var packs, groups, extensions stringList
	flags := flag.NewFlagSet("fetch16c", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { printUsage(stderr, flags) }

	flags.IntVar(&config.Years, "years", 1, "number of years to fetch, counting backward from the current year")
	flags.IntVar(&config.Year, "year", 0, "fetch one exact year")
	flags.IntVar(&config.FromYear, "from-year", 0, "oldest year in an inclusive range")
	flags.IntVar(&config.ToYear, "to-year", 0, "newest year in an inclusive range (default: current year)")
	flags.BoolVar(&config.AllYears, "all", false, "fetch every year the archive has")
	flags.StringVar(&config.Type, "type", TypePacks, "listing type to fetch: packs, mags, or all")
	flags.StringVar(&config.Path, "path", "art", "directory in which to store art packs")
	flags.Var(&packs, "pack", "fetch only packs matching this name or shell pattern (repeatable, comma-separated)")
	flags.Var(&groups, "group", "fetch only packs released by this group (repeatable, comma-separated)")
	flags.BoolVar(&artOnly, "art-only", false, "extract only ANSI, ASCII, and other textmode art files")
	flags.Var(&extensions, "extensions", "extract only files with these extensions (repeatable, comma-separated)")
	flags.BoolVar(&config.SkipExisting, "skip-existing", true, "skip packs with a valid completion manifest")
	flags.BoolVar(&config.Overwrite, "overwrite", false, "replace existing pack directories")
	flags.BoolVar(&config.KeepArchives, "keep-archives", false, "retain downloaded archives after extraction")
	flags.BoolVar(&config.DryRun, "dry-run", false, "list what would be fetched without downloading anything")
	flags.BoolVar(&config.Quiet, "quiet", false, "suppress per-pack output")
	flags.IntVar(&config.Concurrency, "concurrency", 4, "maximum number of packs downloaded concurrently")
	flags.IntVar(&config.Retries, "retries", 3, "retries for each failed request")
	flags.DurationVar(&config.Timeout, "timeout", defaultTimeout, "timeout for each API request and the longest stall allowed during a download")
	flags.BoolVar(&showVersion, "version", false, "print the version and exit")

	if err := flags.Parse(args); err != nil {
		return config, false, err
	}
	if flags.NArg() != 0 {
		err := fmt.Errorf("unexpected arguments: %v", flags.Args())
		fmt.Fprintln(stderr, err)
		return config, false, err
	}

	visited := make(map[string]bool)
	flags.Visit(func(item *flag.Flag) { visited[item.Name] = true })
	rangeSelected := config.FromYear != 0 || config.ToYear != 0
	if config.Year != 0 && (visited["years"] || rangeSelected || config.AllYears) {
		err := errors.New("--year cannot be combined with --years, --from-year, --to-year, or --all")
		fmt.Fprintln(stderr, err)
		return config, false, err
	}
	if visited["years"] && (rangeSelected || config.AllYears) {
		err := errors.New("--years cannot be combined with --from-year, --to-year, or --all")
		fmt.Fprintln(stderr, err)
		return config, false, err
	}
	if config.AllYears && rangeSelected {
		err := errors.New("--all cannot be combined with --from-year or --to-year")
		fmt.Fprintln(stderr, err)
		return config, false, err
	}
	// A pack requested without any year selector is searched for across the whole archive.
	if len(packs) > 0 && !visited["years"] && config.Year == 0 && !rangeSelected {
		config.AllYears = true
	}
	if config.Overwrite {
		config.SkipExisting = false
	}
	config.Packs = packs
	config.Groups = groups
	if artOnly {
		extensions = append(extensions, artExtensions...)
	}
	config.Extensions = normalizeExtensions(extensions)
	if err := config.Validate(time.Now()); err != nil {
		fmt.Fprintln(stderr, err)
		return config, false, err
	}
	return config, showVersion, nil
}

func printUsage(writer io.Writer, flags *flag.FlagSet) {
	fmt.Fprintf(writer, `fetch16c downloads art packs from 16colo.rs and extracts them into PATH/YEAR/PACK.

Usage:
  fetch16c [options]

Examples:
  fetch16c                                   current year into ./art
  fetch16c --year 1996 --path ~/art          one year
  fetch16c --from-year 1995 --to-year 1997   an inclusive range
  fetch16c --all --art-only                  the whole archive, art files only
  fetch16c --pack chromeintro                one pack, found in any year
  fetch16c --year 1996 --pack 'acid-*'       packs matching a pattern
  fetch16c --group blocktronics --dry-run    preview a group's packs

Options:
`)
	flags.PrintDefaults()
	fmt.Fprintf(writer, "\nArt file extensions kept by --art-only: %s\n", strings.Join(artExtensions, ", "))
}
