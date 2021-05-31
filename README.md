# fetch16c
Utilizes the 16colo.rs api to download art packs, un-archives them and places into folders:


```
path
   |---year
       |--- packName
       |--- packName
   |---year
       |--- packName
       |--- packName

```
          
Support .zip archives internally, .lzh requires lhasa to be installed.

usage:

```./fetch16c -years [number] -path [path/to/download]```

Example: ```./fetch16c -years 4 -path /home/robbiew/art``` would grab 4 years of packs from the current year.

If you just want the current year's pack, use `-years 0`

For Linux, Rpi and Mac...


