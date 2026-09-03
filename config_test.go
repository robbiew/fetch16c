package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
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

func TestParseConfigRejectsConflictingSelectors(t *testing.T) {
	var stderr bytes.Buffer
	if _, _, err := parseConfig([]string{"--year", "1996", "--years", "2"}, &stderr); err == nil {
		t.Fatal("parseConfig accepted conflicting year selectors")
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

func TestExecuteReturnsNonzeroOnFetchFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := execute(context.Background(), []string{"--year", "2026", "--quiet", "--path", t.TempDir()}, &stdout, &stderr, server.URL+"/")
	if exitCode != 1 {
		t.Fatalf("execute exit code = %d, want 1", exitCode)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("year 2026")) {
		t.Fatalf("stderr does not identify the failed year: %s", stderr.String())
	}
}

func TestExecuteReturnsTwoForInvalidFlags(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if exitCode := execute(context.Background(), []string{"--concurrency", "0"}, &stdout, &stderr, defaultAPIBaseURL); exitCode != 2 {
		t.Fatalf("execute exit code = %d, want 2", exitCode)
	}
}
