package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSelectedYears(t *testing.T) {
	now := time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		config Config
		want   []int
	}{
		{name: "count", config: Config{Years: 3}, want: []int{2026, 2025, 2024}},
		{name: "exact", config: Config{Years: 1, Year: 1996}, want: []int{1996}},
		{name: "range", config: Config{Years: 1, FromYear: 1995, ToYear: 1997}, want: []int{1997, 1996, 1995}},
		{name: "open range", config: Config{Years: 1, FromYear: 2024}, want: []int{2026, 2025, 2024}},
		{name: "all", config: Config{Years: 1, AllYears: true}, want: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			years, err := test.config.SelectedYears(now)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(years, test.want) {
				t.Fatalf("SelectedYears() = %v, want %v", years, test.want)
			}
		})
	}
}

func TestParseConfigRejectsConflictsAndBadValues(t *testing.T) {
	tests := [][]string{
		{"--year", "1996", "--years", "2"},
		{"--year", "1996", "--all"},
		{"--years", "2", "--from-year", "1995"},
		{"--all", "--to-year", "2000"},
		{"--type", "bogus"},
		{"--retries", "-1"},
		{"--concurrency", "0"},
		{"--pack", "[unclosed"},
		{"--timeout", "0s"},
		{"positional"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stderr bytes.Buffer
			if _, _, err := parseConfig(args, &stderr); err == nil {
				t.Fatalf("parseConfig(%v) accepted invalid arguments", args)
			}
			if stderr.Len() == 0 {
				t.Fatal("parseConfig did not explain the rejection")
			}
		})
	}
}

func TestParseConfigOverwriteDisablesSkip(t *testing.T) {
	var stderr bytes.Buffer
	config, _, err := parseConfig([]string{"--overwrite"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if config.SkipExisting {
		t.Fatal("--overwrite did not disable --skip-existing")
	}
}

func TestParseConfigPackWithoutYearSearchesAllYears(t *testing.T) {
	var stderr bytes.Buffer
	config, _, err := parseConfig([]string{"--pack", "chromeintro,acid-*", "--pack", "Blocktronics*"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !config.AllYears {
		t.Fatal("--pack without a year selector did not select every year")
	}
	if want := []string{"chromeintro", "acid-*", "Blocktronics*"}; !reflect.DeepEqual(config.Packs, want) {
		t.Fatalf("Packs = %v, want %v", config.Packs, want)
	}

	config, _, err = parseConfig([]string{"--pack", "chromeintro", "--year", "2026"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if config.AllYears {
		t.Fatal("--pack with --year selected every year")
	}
}

func TestParseConfigExtensions(t *testing.T) {
	var stderr bytes.Buffer
	config, _, err := parseConfig([]string{"--extensions", ".ANS, txt", "--extensions", "nfo,ans"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ans", "nfo", "txt"}; !reflect.DeepEqual(config.Extensions, want) {
		t.Fatalf("Extensions = %v, want %v", config.Extensions, want)
	}

	config, _, err = parseConfig([]string{"--art-only", "--extensions", "mp3"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	for _, extension := range append([]string{"mp3"}, artExtensions...) {
		if !config.FileFilter()("file." + strings.ToUpper(extension)) {
			t.Fatalf("--art-only filter rejected .%s", extension)
		}
	}
	if config.FileFilter()("viewer.exe") {
		t.Fatal("--art-only filter accepted an executable")
	}

	config, _, err = parseConfig(nil, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if config.FileFilter() != nil {
		t.Fatal("default configuration filters files")
	}
}

func TestParseConfigVersionAndHelp(t *testing.T) {
	var stderr bytes.Buffer
	_, showVersion, err := parseConfig([]string{"--version"}, &stderr)
	if err != nil || !showVersion {
		t.Fatalf("--version: showVersion = %v, error = %v", showVersion, err)
	}
	_, _, err = parseConfig([]string{"--help"}, &stderr)
	if err == nil || !strings.Contains(stderr.String(), "Examples:") || !strings.Contains(stderr.String(), "--art-only") {
		t.Fatalf("--help error = %v, output = %s", err, stderr.String())
	}
}

func TestPackTypes(t *testing.T) {
	if got := (Config{}).PackTypes(); !reflect.DeepEqual(got, []string{TypePacks}) {
		t.Fatalf("default PackTypes = %v", got)
	}
	if got := (Config{Type: TypeAll}).PackTypes(); !reflect.DeepEqual(got, []string{TypePacks, TypeMags}) {
		t.Fatalf("all PackTypes = %v", got)
	}
}

func TestMatchesPackPattern(t *testing.T) {
	tests := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"chromeintro", "ChromeIntro", true},
		{"chrome", "chromeintro", false},
		{"acid-*", "ACID-53A", true},
		{"acid-5?", "acid-53", true},
		{"acid-5?", "acid-53a", false},
	}
	for _, test := range tests {
		if got := matchesPackPattern(test.pattern, test.name); got != test.want {
			t.Errorf("matchesPackPattern(%q, %q) = %v, want %v", test.pattern, test.name, got, test.want)
		}
	}
}
