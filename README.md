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

## Features

- Fetch the current year, an exact year, a range, or the whole archive back to 1990.
- Fetch single packs by name or shell pattern, or every pack by a group, from any year.
- Fetch art packs, magazines, or both.
- Extract only ANSI, ASCII, and other textmode art files with `--art-only`, ready for a BBS or terminal art viewer.
- Preview everything with `--dry-run` before downloading a byte.
- Retries with backoff for flaky connections, stall detection for hung downloads, and a resumable completion manifest per pack.
- ZIP extraction is built in, including the PKZIP 1.x Shrink and Implode methods used by packs from the early 1990s. LHA, LZH, and ARJ packs use `lha`, `7z`, or `unar` when one is installed.
- Members that are corrupt inside the archive itself are skipped and listed in the pack's manifest, so the rest of the pack is still usable.
- Historical ZIP packs keep their original file dates, and DOS code page 437 filenames are converted to UTF-8.

## Install

Download a prebuilt binary for Linux, macOS, or Windows from the [releases page](https://github.com/robbiew/fetch16c/releases), or install with Go 1.26 or newer:

```sh
go install github.com/robbiew/fetch16c@latest
```

To build from a checkout:

```sh
go build -o fetch16c .
```

### Optional extractors

ZIP packs need nothing extra, whatever PKZIP version made them. A few historical packs are LHA, LZH, or ARJ archives, and fetch16c unpacks them with the first of these tools it finds on your `PATH`:

| Format | Tools, in order of preference |
| --- | --- |
| `.lha`, `.lzh` | `lha` (the `lhasa` package on Homebrew and most Linux distributions), `7z`, `7zz`, `7za`, `unar` |
| `.arj` | `arj`, `7z`, `7zz`, `7za`, `unar` |

Without a suitable tool those packs are reported as unsupported and skipped. The run still succeeds.

## Usage

Fetch the current year into `art`:

```sh
fetch16c
```

Fetch an exact year or inclusive range:

```sh
fetch16c --year 1996 --path ~/art
fetch16c --from-year 1995 --to-year 1997 --path ~/art
```

Fetch the entire archive, keeping only art files:

```sh
fetch16c --all --art-only --path ~/art
```

Fetch one pack. Without a year selector, fetch16c searches every year for it:

```sh
fetch16c --pack chromeintro
fetch16c --year 1996 --pack 'acid-*' --pack 'ice-*'
fetch16c --pack acid-53a,acid-53b
```

Fetch everything a group released, or preview what a run would do:

```sh
fetch16c --all --group blocktronics
fetch16c --year 1996 --dry-run
```

Fetch magazines as well as packs:

```sh
fetch16c --year 1996 --type all
```

The original count-based form remains supported:

```sh
fetch16c --years 4 --path ~/art
```

### Options

| Option | Default | Purpose |
| --- | --- | --- |
| `--year YEAR` | | Fetch one exact year |
| `--from-year YEAR` | | Oldest year in an inclusive range |
| `--to-year YEAR` | current year | Newest year in an inclusive range |
| `--years N` | `1` | Fetch N years backward from the current year |
| `--all` | `false` | Fetch every year in the archive |
| `--type TYPE` | `packs` | Fetch `packs`, `mags`, or `all` |
| `--pack NAME` | | Fetch packs matching a name or shell pattern; repeatable or comma-separated |
| `--group NAME` | | Fetch packs released by a group; repeatable or comma-separated |
| `--art-only` | `false` | Extract only textmode art and text files |
| `--extensions LIST` | | Extract only files with these extensions; repeatable or comma-separated |
| `--path PATH` | `art` | Set the output directory |
| `--dry-run` | `false` | List what would be fetched without downloading |
| `--concurrency N` | `4` | Limit concurrent pack downloads |
| `--retries N` | `3` | Retry each failed request this many times |
| `--timeout DURATION` | `2m` | Limit each API request, and the longest stall allowed during a download |
| `--skip-existing` | `true` | Skip packs with valid completion manifests |
| `--overwrite` | `false` | Atomically replace existing pack directories |
| `--keep-archives` | `false` | Retain downloaded archive files |
| `--quiet` | `false` | Suppress per-pack output |
| `--version` | | Print the version and exit |

Pack names and patterns are matched without regard to case. Patterns use shell syntax: `*`, `?`, and `[...]`.

Set `--concurrency 1` to display a byte progress bar. Concurrent runs print pack-level progress instead.

### Art-only extraction

`--art-only` keeps files with these extensions and drops everything else, such as executables, music, and images:

`ans`, `ansi`, `asc`, `txt`, `nfo`, `diz`, `mem`, `lit`, `ice`, `pcb`, `avt`, `bin`, `xb`, `adf`, `idf`, `tnd`, `rip`

Use `--extensions` to choose your own list. Adding `--extensions` alongside `--art-only` extends the art list. The filter is recorded in each pack's manifest, so a later run with a different filter reports the pack as stale instead of silently skipping it. Use `--overwrite` to re-extract it with the new filter.

### Pack outcomes

Every selected pack ends in one of these states:

| State | Meaning |
| --- | --- |
| fetched | Downloaded and extracted |
| skipped | Already complete according to its manifest |
| unavailable | 16colo.rs lists the pack but publishes no archive for it, or the server has no file at the listed URL |
| unsupported | The archive format cannot be extracted on this system |
| failed | Something went wrong; details are printed to stderr |

Some historical archives are listed under a file name with punctuation such as `#` or `$` that the server does not actually serve. When the listed URL is not found, fetch16c also tries the plain pack name with the same extension before giving up.

Fetch16c exits with status 1 if any pack or year failed, 130 if interrupted, and 0 otherwise. Unavailable and unsupported packs do not cause a failure. Pressing Ctrl-C once lets in-flight packs finish; pressing it again quits immediately.

### Resuming and existing directories

Each extracted pack contains a `.fetch16c.json` manifest recording the archive URL, size, file count, groups, gallery URL, and extraction filter. Later runs skip packs whose manifest matches, so an interrupted run can be repeated to pick up where it left off. Staging directories and partial downloads left behind by an interrupted run are cleaned up at the start of the next one.

Existing pack directories created by versions before 2.1 do not have completion manifests. Use `--overwrite` once to replace and adopt them; subsequent runs will skip them safely.

## Testing

Run local tests and static analysis:

```sh
go test -race ./...
go vet ./...
golangci-lint run ./...
```

Run the opt-in smoke tests against the live API:

```sh
FETCH16C_LIVE_TEST=1 go test -run '^TestLive' -count=1 -v .
```

## Releasing

Tag a commit with `vX.Y.Z` and push the tag. The release workflow runs the tests, then GoReleaser builds archives for Linux, macOS, and Windows on amd64 and arm64 and publishes them to GitHub Releases.
