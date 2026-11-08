# Changelog

## 2.2.0

### Fixed

- Years 1990 and 1991 can be fetched again. The API serves the packs literally named `1990` and `1991` as JSON numbers, which previously broke decoding of the whole year.
- Packs that 16colo.rs lists without an archive (52 of them, mostly from the 1990s) are reported as unavailable instead of failing the run.
- Archive URLs containing `#` are no longer truncated at the hash. When the listed archive URL is not found on the server, fetch16c tries the plain pack name with the same extension, which recovers dozens of historical packs; archives missing under both names are reported as unavailable instead of failing the run.
- Pagination now requests the API's maximum page size, detects the API answering an out-of-range page with page 1 again, and ignores duplicate entries.
- The request timeout no longer caps the total duration of a download. It bounds each API request and the longest stall tolerated during a download, so large packs on slow links complete.
- Redirects are followed only to 16colo.rs hosts; an archive URL on an official host with a nonstandard port is rejected.
- Pressing Ctrl-C twice now quits immediately. The first press lets in-flight packs finish; the exit status is 130.
- ZIP entries that are symbolic links are no longer written as regular files.
- ZIP packs compressed with PKZIP 1.x Shrink or Implode, which the Go standard library does not support, now extract natively. About 80 archives from 1990 to 1994 (including nested ZIPs) use these methods.
- ZIP members whose stored data is corrupt are skipped and recorded under `damaged_files` in the manifest instead of failing the whole pack. Info-ZIP rejects the same members, so the damage is in the archives 16colo.rs serves.

### Added

- `--art-only` and `--extensions` extract only ANSI, ASCII, and other textmode art files, for use with BBS and terminal art viewers. The filter is recorded in each manifest, so a run with a different filter reports the pack as stale.
- `--all` fetches every year in the archive, and `--pack` without a year selector searches every year.
- `--pack` accepts shell patterns, repeats, and comma-separated lists; `--group` filters by releasing group.
- `--type packs|mags|all` fetches magazines as well as art packs.
- `--dry-run` previews what a run would do without downloading.
- `--retries` with exponential backoff and `Retry-After` support for transient API and download failures.
- ARJ packs are supported, and LHA, LZH, and ARJ packs can be extracted with `lha`, `7z`, `7zz`, `7za`, or `unar`, whichever is installed. Missing tools mark packs as unsupported instead of failing the run.
- ZIP extraction preserves original file modification times and converts DOS code page 437 filenames to UTF-8.
- Downloaded archives kept with `--keep-archives` carry the server's `Last-Modified` time.
- Staging directories and partial downloads left by an interrupted run are cleaned up on the next run.
- Manifests (version 2) record the gallery URL, groups, archive name, file count, and extraction filter.
- A `User-Agent` header identifies the client to the API.
- `--version` reports the Go version and platform, and `go install` builds report their module version.
- Prebuilt release binaries for Linux, macOS, and Windows via GoReleaser, plus lint and vulnerability checks in CI across Linux, macOS, and Windows.

### Changed

- Requires Go 1.26 or newer to build.
- The summary line reports unavailable and unsupported counts alongside fetched, skipped, and failed.
- The staging directory prefix is now `.fetch16c-staging-`.

## 2.1.0

- Staged downloads and extraction with atomic replacement, completion manifests, and resumable runs.
- Exact year and year range selection, single pack selection, and configurable concurrency and timeout.
