package main

import (
	"archive/zip"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/m7shapan/njson"
)

var (
	yearsBack  int
	path       string
	currYear   int
	fetchYears []int
	only       int
)

type WriteCounter struct {
	Total uint64
}

type Pack struct {
	Year     []string `njson:"results.1.year"`
	Download []string `njson:"results.#.download"`
	Name     []string `njson:"results.#.name"`
}

const url = "https://api.16colo.rs/v1/year/"

func check(e error) {
	if e != nil {
		panic(e)
	}
}

func (wc *WriteCounter) Write(p []byte) (int, error) {
	n := len(p)
	wc.Total += uint64(n)
	wc.PrintProgress()
	return n, nil
}

func (wc WriteCounter) PrintProgress() {
	// Clear the line by using a character return to go back to the start and remove
	// the remaining characters by filling it with spaces
	fmt.Printf("\r%s", strings.Repeat(" ", 35))

	// Return again and print current status of download
	// We use the humanize package to print the bytes in a meaningful way (e.g. 10 MB)
	fmt.Printf("\rDownloading... %s complete", humanize.Bytes(wc.Total))
}

// isDir returns true if the given path is an existing directory.
func isDir(pathFile string) bool {
	if pathAbs, err := filepath.Abs(pathFile); err != nil {
		return false
	} else if fileInfo, err := os.Stat(pathAbs); os.IsNotExist(err) || !fileInfo.IsDir() {
		return false
	}
	return true
}

// isEmpty returns true if the given path is empty.
// func isEmpty(name string) (bool, error) {
// 	f, err := os.Open(name)
// 	if err != nil {
// 		return false, err
// 	}
// 	defer f.Close()

// 	_, err = f.Readdirnames(1) // Or f.Readdir(1)
// 	if err == io.EOF {
// 		return true, nil
// 	}
// 	return false, err // Either not empty or error, suits both cases
// }

func callAPI() {

	fmt.Println("Starting the application...")

	for _, year := range fetchYears {
		response, err := http.Get(url + strconv.Itoa(year))
		json, _ := ioutil.ReadAll(response.Body)
		if err != nil {
			fmt.Printf("The HTTP request failed with error %s\n", err)
		} else {
			p := Pack{}
			err := njson.Unmarshal([]byte(json), &p)
			if err != nil {
				// do anything
			}

			// fmt.Printf("%+v\n", p.Year[0])

			// Make sure YEAR directort doesn't exist (exit if it does)
			yearDir := path + "/" + p.Year[0]

			if isDir(yearDir) {
				fmt.Println("ABORT! Directory exists: " + yearDir + ". Please remove dir and try again.")
				os.Exit(0)
			} else {
				fmt.Println(yearDir + "... Creating directory... ")
				err = os.Chdir(path)
				check(err)
				err := os.Mkdir(p.Year[0], 0755)
				check(err)
			}

			// get file extension/compression type (zip/lhz/rar)
			filename := p.Download[0]
			var ext = filepath.Ext(filename)

			// download each Pack zip file in Year dir
			fmt.Println(p.Download[0] + "... Downloading... ")
			fileUrl := p.Download[0]
			zipLoc := yearDir + "/" + p.Name[0] + ext
			fmt.Println("ext: ", ext)

			err = DownloadFile(zipLoc, fileUrl)
			if err != nil {
				panic(err)
			}

			fmt.Println("Downloaded: " + fileUrl)

			// Set file permissions

			stats, err := os.Stat(zipLoc)
			if err != nil {
				log.Fatal(err)
			}
			fmt.Printf("Permission File Before: %s\n", stats.Mode())
			err = os.Chmod(zipLoc, 0777)
			if err != nil {
				log.Fatal(err)
			}

			stats, err = os.Stat(zipLoc)
			if err != nil {
				log.Fatal(err)
			}
			fmt.Printf("Permission File After: %s\n", stats.Mode())

			if ext == ".zip" || ext == ".ZIP" {

				// upzip YEAR.ZIP to Year dir

				packDir := yearDir + "/" + p.Name[0]

				files, err := Unzip(zipLoc, packDir)
				if err != nil {
					log.Fatal(err)
				}
				fmt.Println("Extracted:\n" + strings.Join(files, "\n"))

				// Remove zip file from the directory
				e := os.Remove(packDir + ".zip")
				if e != nil {
					log.Fatal(e)
				}

			}

			if ext == ".lha" || ext == ".LHA" {

				// move archive to its Pack directory

				packDir := yearDir + "/" + p.Name[0]
				os.Mkdir(packDir, 0755)
				newloc := yearDir + "/" + p.Name[0] + "/" + p.Name[0] + ext
				os.Rename(zipLoc, newloc)

				// change to working directory

				os.Chdir(packDir)
				newDir, err := os.Getwd()
				if err != nil {
					log.Fatal(err)
				}

				// extract lha file

				fmt.Printf("Extracting: %s.lzh to %s\n", p.Name[0], newDir)

				prg := "lhasa"
				arg1 := "-e"
				arg2 := newloc

				cmd := exec.Command(prg, arg1, arg2)
				cmd.Run()

				// TO DO: remove lha lhz archive

			} else {
				fmt.Println("can't decompress extension " + ext + "...")
			}
		}

	}
}

// Unzip will decompress a zip archive, moving all files and folders
// within the zip file (parameter 1) to an output directory (parameter 2).
func Unzip(src string, dest string) ([]string, error) {

	var filenames []string

	r, err := zip.OpenReader(src)
	if err != nil {
		return filenames, err
	}
	defer r.Close()

	for _, f := range r.File {

		// Store filename/path for returning and using later on
		fpath := filepath.Join(dest, f.Name)

		// Check for ZipSlip. More Info: http://bit.ly/2MsjAWE
		if !strings.HasPrefix(fpath, filepath.Clean(dest)+string(os.PathSeparator)) {
			return filenames, fmt.Errorf("%s: illegal file path", fpath)
		}

		filenames = append(filenames, fpath)

		if f.FileInfo().IsDir() {
			// Make Folder
			os.MkdirAll(fpath, os.ModePerm)
			continue
		}

		// Make File
		if err = os.MkdirAll(filepath.Dir(fpath), os.ModePerm); err != nil {
			return filenames, err
		}

		outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return filenames, err
		}

		rc, err := f.Open()
		if err != nil {
			return filenames, err
		}

		_, err = io.Copy(outFile, rc)

		// Close the file without defer to close before next iteration of loop
		outFile.Close()
		rc.Close()

		if err != nil {
			return filenames, err
		}
	}
	return filenames, nil
}

// DownloadFile will download a url to a local file. It's efficient because it will
// write as it downloads and not load the whole file into memory. We pass an io.TeeReader
// into Copy() to report progress on the download.
func DownloadFile(filepath string, url string) error {

	// Create the file, but give it a tmp file extension, this means we won't overwrite a
	// file until it's downloaded, but we'll remove the tmp extension once downloaded.
	out, err := os.Create(filepath + ".tmp")
	if err != nil {
		return err
	}

	// Get the data
	resp, err := http.Get(url)
	if err != nil {
		out.Close()
		return err
	}
	defer resp.Body.Close()

	// Create our progress reporter and pass it to be used alongside our writer
	counter := &WriteCounter{}
	if _, err = io.Copy(out, io.TeeReader(resp.Body, counter)); err != nil {
		out.Close()
		return err
	}

	// The progress use the same line so print a new line once it's finished downloading
	fmt.Print("\n")

	// Close the file without defer so it can happen before Rename()
	out.Close()

	if err = os.Rename(filepath+".tmp", filepath); err != nil {
		return err
	}
	return nil
}

func main() {

	// var onlyPtr *int

	yearsPtr := flag.Int("years", 0, "number of years back to retrieve")
	pathPtr := flag.String("path", "", "path to download files")
	// onlyPtr = flag.Int("only", 1995, " year")

	required := []string{"path"}
	flag.Parse()

	seen := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	for _, req := range required {
		if !seen[req] {
			// or possibly use `log.Fatalf` instead of:
			fmt.Fprintf(os.Stderr, "missing required -%s argument/flag\n", req)
			os.Exit(2) // the same exit code flag.Parse uses

		}
	}

	path = *pathPtr
	yearsBack = *yearsPtr
	// only = *onlyPtr
	t := time.Now()
	currYear = t.Year()

	// if onlyPtr != nil {
	// 	currYear = only
	// 	yearsBack = 0
	// }

	// calculate years needs...

	fmt.Println("Ok, going back " + strconv.Itoa(yearsBack) + " years from " + strconv.Itoa(currYear) + ":")

	startYear := currYear
	for i := (startYear) - yearsBack; i < (startYear + 1); i++ {
		fetchYears = append(fetchYears, i)
	}

	// check if root dir exists
	if isDir(path) {
		fmt.Println("Path exists...")
		callAPI()

	} else {
		fmt.Println("Make sure " + path + " exists and is empty...")
	}

}
