# fetch16c

Fetches art packs from the [16colo.rs](https://16colo.rs) API and extracts them into directories organized by year and pack name:

```text
art/
└── 2026/
    └── pack-name/
        ├── artwork.ans
        └── .fetch16c.json
```

Downloads and extraction are staged in temporary paths. A pack replaces its destination only after extraction succeeds, and a completion manifest makes later runs resumable.

## Requirements

- Go 1.25 or newer to build from source.
- The `lha` command to extract historical `.lha` and `.lzh` packs. ZIP extraction is built in.

Install `lha` through apt, Homebrew, or another system package manager when fetching years that contain LHA archives. Fetch16c checks for the command before downloading those packs.

## Build

```sh
go build -o fetch16c .
```

## Usage

Fetch the current year into `art`:

```sh
./fetch16c
```

Fetch an exact year or inclusive range:

```sh
./fetch16c --year 1996 --path ~/art
./fetch16c --from-year 1995 --to-year 1997 --path ~/art
```

Fetch one exact pack from the selected year:

```sh
./fetch16c --year 2026 --pack chromeintro
```

The original count-based form remains supported:

```sh
./fetch16c --years 4 --path ~/art
```

Key options:

| Option | Default | Purpose |
| --- | --- | --- |
| `--year YEAR` | | Fetch one exact year |
| `--from-year YEAR` | | Oldest year in an inclusive range |
| `--to-year YEAR` | current year | Newest year in an inclusive range |
| `--years N` | `1` | Fetch N years backward from the current year |
| `--pack NAME` | | Fetch one exact pack name |
| `--path PATH` | `art` | Set the output directory |
| `--concurrency N` | `4` | Limit concurrent pack downloads |
| `--timeout DURATION` | `2m` | Limit each HTTP request |
| `--skip-existing` | `true` | Skip packs with valid completion manifests |
| `--overwrite` | `false` | Atomically replace existing pack directories |
| `--keep-archives` | `false` | Retain downloaded archive files |
| `--quiet` | `false` | Suppress per-pack output |

Set `--concurrency 1` to display a byte progress bar. Concurrent runs print pack-level progress instead.

Existing pack directories created by versions before 2.1 do not have completion manifests. Use `--overwrite` once to replace and adopt them; subsequent runs will skip them safely.

Fetch16c exits nonzero if any selected year or pack fails. Its final summary reports fetched, skipped, and failed counts plus total downloaded bytes.

## Testing

Run local tests and static analysis:

```sh
go test -race ./...
go vet ./...
```

Run the opt-in smoke test against the live API and a current art pack:

```sh
FETCH16C_LIVE_TEST=1 go test -run '^TestLive' -count=1 -v
```


