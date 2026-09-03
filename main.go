package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"time"
)

const defaultAPIBaseURL = "https://api.16colo.rs/v1/"

var version = "2.1.0"

func main() {
	os.Exit(runMain())
}

func runMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
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
		fmt.Fprintf(stdout, "fetch16c %s\n", version)
		return 0
	}

	if !config.Quiet {
		fmt.Fprintf(stdout, "Fetch16c %s by robbiew, aka aLPHA64.\n", version)
		fmt.Fprintln(stdout, "https://github.com/robbiew/fetch16c")
	}

	client, err := NewClientWithHTTPClient(apiBaseURL, &http.Client{Timeout: config.Timeout}, stdout, !config.Quiet && config.Concurrency == 1)
	if err != nil {
		fmt.Fprintf(stderr, "fetch16c: %v\n", err)
		return 1
	}

	fetcher := NewFetcher(client, config, stdout)
	summary, runErr := fetcher.Run(ctx)
	summary.Print(stdout)
	if runErr != nil {
		fmt.Fprintf(stderr, "fetch16c: %v\n", runErr)
		return 1
	}
	return 0
}

func parseConfig(args []string, stderr io.Writer) (Config, bool, error) {
	config := Config{}
	showVersion := false
	flags := flag.NewFlagSet("fetch16c", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.IntVar(&config.Years, "years", 1, "number of years to fetch, counting backward from the current year")
	flags.IntVar(&config.Year, "year", 0, "fetch one exact year")
	flags.IntVar(&config.FromYear, "from-year", 0, "oldest year in an inclusive range")
	flags.IntVar(&config.ToYear, "to-year", 0, "newest year in an inclusive range")
	flags.StringVar(&config.Path, "path", "art", "directory in which to store art packs")
	flags.StringVar(&config.Pack, "pack", "", "fetch one pack by exact name")
	flags.BoolVar(&config.SkipExisting, "skip-existing", true, "skip packs with a valid completion manifest")
	flags.BoolVar(&config.Overwrite, "overwrite", false, "replace existing pack directories")
	flags.BoolVar(&config.KeepArchives, "keep-archives", false, "retain downloaded archives after extraction")
	flags.BoolVar(&config.Quiet, "quiet", false, "suppress per-pack output")
	flags.IntVar(&config.Concurrency, "concurrency", 4, "maximum number of packs downloaded concurrently")
	flags.DurationVar(&config.Timeout, "timeout", 2*time.Minute, "timeout for each HTTP request")
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
	if config.Year != 0 && (visited["years"] || config.FromYear != 0 || config.ToYear != 0) {
		err := errors.New("--year cannot be combined with --years, --from-year, or --to-year")
		fmt.Fprintln(stderr, err)
		return config, false, err
	}
	if (config.FromYear != 0 || config.ToYear != 0) && visited["years"] {
		err := errors.New("--years cannot be combined with --from-year or --to-year")
		fmt.Fprintln(stderr, err)
		return config, false, err
	}
	if config.Overwrite {
		config.SkipExisting = false
	}
	if err := config.Validate(time.Now()); err != nil {
		fmt.Fprintln(stderr, err)
		return config, false, err
	}
	return config, showVersion, nil
}
