# fetch16c
Utilizes the 16colo.rs api to download art packs, un-archives only files with .ans, asc or .diz extensions and places them into pack folders, organized by year and 16Colo.rs spack name:


```
path
|-- year
    |--- packName
         |--- file.ans
         |--- file.asc
         |--- file.diz
```

## BUILD:
Utilizes "Terminal progress bar for Go" for printing download progress in console.

- clone this repo
- ```go get github.com/cheggaaa/pb/v3```
- ```go build .```

## REQUIRED FOR UN-ARCHIVING!

(install via apt, homebrew, etc.)
- .zip files require unzip to be installed. 
- .lzh files require lhasa to be installed.

## USAGE:

```./fetch16c -years [number] -path [path/to/download]```

Example: ```./fetch16c -years 4 -path /home/robbiew/art``` would grab 4 years of packs from the current year.

If you just want the current year's pack, use `-years 1`

Tested on Ubuntu 22.04.


