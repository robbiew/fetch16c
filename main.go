package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/m7shapan/njson"
)

var (
	yearsBack  int
	currYear   int
	fetchYears []int
	downLinks  []string
)

type Pack struct {
	Results []string `njson:"results.#.download"`
}

const url = "https://api.16colo.rs/v1/year/"

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
			fmt.Printf("%+v\n", p)

			// Make sure YEAR directort doesn't exist (exit if it does)
			// download YEAR.zip
			// upzip YEAR.ZIP to Year dir

		}
	}
}

func main() {

	yearsPtr := flag.Int("y", 2, "number of years back to retrieve")

	required := []string{"y", "s"}
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

	yearsBack = *yearsPtr
	t := time.Now()
	currYear = t.Year()

	// calculate years needs...

	fmt.Println("Ok, going back " + strconv.Itoa(yearsBack) + " years from " + strconv.Itoa(currYear) + ":")

	startYear := currYear
	for i := (startYear) - yearsBack; i < (startYear + 1); i++ {
		fetchYears = append(fetchYears, i)
	}

	// get packs...

	callAPI()

}
