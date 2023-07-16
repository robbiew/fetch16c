package main

import (
	"archive/zip"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/cheggaaa/pb/v3"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var version = "2.0"

type Result struct {
	Year     int      `json:"year"`
	Name     string   `json:"name"`
	Download string   `json:"download"`
	Gallery  string   `json:"gallery"`
	Archive  string   `json:"archive"`
	Groups   []string `json:"groups"`
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

func main() {
	fmt.Printf("Fetch16c %s by robbiew, aka aLPHA64.\n", version)
	fmt.Println("https://github.com/robbiew/fetch16c")

	years := flag.Int("years", 1, "Number of years to process from the API")
	path := flag.String("path", "art", "Path to download the files")
	flag.Parse()

	currentYear := time.Now().Year()
	for i := 0; i < *years; i++ {
		year := currentYear - i
		apiURL := fmt.Sprintf("https://api.16colo.rs/v1/year/%d", year)
		response, err := fetchAPIResponse(apiURL)
		if err != nil {
			fmt.Printf("Error fetching API response for year %d: %v\n", year, err)
			continue
		}

		// Create the output directory for the year if it doesn't exist
		yearPath := filepath.Join(*path, fmt.Sprintf("%d", year))
		err = os.MkdirAll(yearPath, os.ModePerm)
		if err != nil {
			fmt.Printf("Error creating output directory for year %d: %v\n", year, err)
			continue
		}

		var failedFiles []string

		for _, result := range response.Results {
			archiveURL := result.Download
			filename := filepath.Base(archiveURL)
			outputDir := filepath.Join(yearPath, result.Name)

			// Create the output directory for the pack if it doesn't exist
			err = os.MkdirAll(outputDir, os.ModePerm)
			if err != nil {
				fmt.Printf("Error creating output directory for pack %s: %v\n", result.Name, err)
				continue
			}

			archivePath := filepath.Join(yearPath, filename)

			err = downloadFile(archiveURL, archivePath)
			if err != nil {
				fmt.Printf("Error downloading file: %v\n", err)
				continue
			}

			err = extractArchive(archivePath, outputDir)
			if err != nil {
				fmt.Printf("Error extracting archive: %v\n", err)
				failedFiles = append(failedFiles, result.Name)
				continue
			}

			err = os.Remove(archivePath)
			if err != nil {
				fmt.Printf("Error deleting archive: %v\n", err)
				continue
			}

			fmt.Printf("Extracted archive: %s\n", result.Name)
		}

		if len(failedFiles) > 0 {
			fmt.Println("The following files had errors and were not processed:")
			for _, file := range failedFiles {
				fmt.Println(file)
			}
		}
	}
}

func fetchAPIResponse(url string) (Response, error) {
	var response Response

	resp, err := http.Get(url)
	if err != nil {
		return response, fmt.Errorf("failed to fetch API response: %v", err)
	}
	defer resp.Body.Close()

	err = json.NewDecoder(resp.Body).Decode(&response)
	if err != nil {
		return response, fmt.Errorf("failed to decode API response: %v", err)
	}

	return response, nil
}

func downloadFile(url string, outputPath string) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to download file: %v", err)
	}
	defer resp.Body.Close()

	file, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create file: %v", err)
	}
	defer file.Close()

	// Get the total size of the file
	totalSize := resp.ContentLength

	// Create the progress bar
	bar := pb.Full.Start64(totalSize)
	bar.Set(pb.Bytes, true)
	bar.SetWidth(80)

	// Create a custom writer that wraps the progress bar
	writer := &ProgressBarWriter{
		ProgressBar: bar,
		Writer:      file,
	}

	_, err = io.Copy(writer, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to save file: %v", err)
	}

	bar.Finish()

	return nil
}

type ProgressBarWriter struct {
	ProgressBar *pb.ProgressBar
	Writer      io.Writer
}

func (pw *ProgressBarWriter) Write(p []byte) (int, error) {
	n, err := pw.Writer.Write(p)
	pw.ProgressBar.Add(n)
	return n, err
}

func extractArchive(archivePath string, outputDir string) error {
	extension := strings.ToLower(filepath.Ext(archivePath))
	switch extension {
	case ".zip":
		return extractZipArchive(archivePath, outputDir)
	case ".lha":
		return extractLhaArchive(archivePath, outputDir)
	default:
		return fmt.Errorf("unsupported archive format: %s", extension)
	}
}

func extractZipArchive(archivePath string, outputDir string) error {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("failed to open ZIP archive: %v", err)
	}
	defer r.Close()

	for _, f := range r.File {
		err := extractFile(f, outputDir)
		if err != nil {
			return fmt.Errorf("failed to extract file from ZIP archive: %v", err)
		}
	}

	return nil
}

func extractLhaArchive(archivePath string, outputDir string) error {
	cmd := exec.Command("lha", "x", "-w"+outputDir, archivePath)
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("failed to extract LHA archive: %v", err)
	}

	return nil
}

func extractFile(f *zip.File, outputDir string) error {
	path := filepath.Join(outputDir, f.Name)

	if strings.HasPrefix(f.Name, "..") || filepath.IsAbs(path) {
		return fmt.Errorf("invalid file path: %s", f.Name)
	}

	if f.FileInfo().IsDir() {
		os.MkdirAll(path, os.ModePerm)
		return nil
	}

	// Create parent directories if they don't exist
	os.MkdirAll(filepath.Dir(path), os.ModePerm)

	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	w, err := os.Create(path)
	if err != nil {
		return err
	}
	defer w.Close()

	_, err = io.Copy(w, rc)
	if err != nil {
		return err
	}

	return nil
}
