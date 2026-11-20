# Lantern

A portable offline knowledge drive for Windows. Put Lantern on a USB drive, fill it with
Wikipedia, books, survival and repair guides, and street maps, then plug it into any Windows
PC and double-click `Lantern.exe`. Everything runs from the drive with no internet and
nothing installed.

## What it does

- **One-click launcher.** `Lantern.exe` starts a local web server, runs the
  [Kiwix](https://kiwix.org) reader for offline content, and opens the home page in your browser.
- **Built-in Downloads page.** Pick content and press Download, and Lantern fetches it onto the drive:
  - Wikipedia (full with images, text only, or intros only), WikiMed, iFixit, Wikibooks, Wikivoyage
  - Project Gutenberg books, the whole library or by subject (Fiction, Science, Medicine, Agriculture…)
  - Search across the whole [Kiwix library](https://library.kiwix.org) for anything else
  - Offline maps: Oklahoma at street level, the US at city or street level, a world overview, or a custom area
- **Fast, resumable downloads.** For each file, Lantern speed-tests Kiwix's mirror servers
  and downloads over 8 parallel connections from the fastest ones. Interrupted downloads
  resume where they stopped.
- **Offline maps** using [Protomaps](https://protomaps.com) / OpenStreetMap data, shown with
  MapLibre, with search for place names on screen.
- **Space checks.** A drive-space meter warns before you pick more than will fit.
- **Lantern Relay.** Plug a Meshtastic node into the PC and your team can search the drive over the
  mesh, from FieldMap's Ask Lantern screen or any Meshtastic app. Questions are direct messages like
  `?s water purification`; answers come back in short messages. Only nodes you allow get answers.
  See [RELAY.md](RELAY.md).

## Using it

1. Format a drive as **exFAT** (FAT32 can't hold files over 4 GB).
2. Build (below) or download a release, then copy the contents of the `Lantern` folder to the root of the drive.
3. Double-click `Lantern.exe`. The exe isn't code-signed, so Windows SmartScreen will warn:
   click **More info**, then **Run anyway**.
4. Open **Downloads**, choose what you want, and leave the Lantern window open until it finishes.

Approximate sizes (from the Kiwix catalog, November 2026): Wikipedia full 127 GB, Wikipedia text only 53 GB,
all of Project Gutenberg 221 GB (subjects range from under 1 GB to about 40 GB), WikiMed 1.9 GB, iFixit 3.6 GB.

## Building

Requires [Go](https://go.dev/dl) 1.22 or newer. There are no other dependencies. Run the tests with `cd launcher && go test .`

```sh
./build.sh        # Linux, macOS, or Git Bash: creates dist/Lantern and dist/Lantern-vX.Y.Z.zip
build.cmd         # Windows: creates dist\Lantern
```

## Repository layout

```
launcher/   Go source for Lantern.exe
  main.go        startup, paths, local web server
  server.go      HTTP API, Kiwix proxy, static files (localhost only)
  kiwix.go       starts and restarts kiwix-serve
  downloads.go   download queue, Kiwix catalog lookups, Kiwix reader and map installs
  fastdl.go      mirror speed test and parallel, resumable downloads
  relay*.go      Lantern Relay: Meshtastic link, USB serial, Kiwix search, answers
  sys_*.go       Windows job object, hidden child windows, free space, browser launch
web/        Home, Maps, Relay and Downloads pages, plus bundled map libraries (web/lib)
package/    README.txt shipped on the drive
```

On the drive, Lantern keeps everything under a `lantern` folder next to the exe:
`web`, `zim` (Kiwix content), `maps` (`.pmtiles`), `kiwix` and `tools` (downloaded automatically),
`maps-assets` (map fonts and icons), `temp` (unfinished downloads), and `relay.json` (Relay settings).

## Known limitations

- Windows 64-bit only for now.
- iFixit's offline copy has the step-by-step repair guides but not the PDF manuals iFixit hosts.
- Map search only finds names in the part of the map currently loaded on screen.

## Credits

Content comes from Wikipedia, Project Gutenberg, iFixit and others, packaged by
[Kiwix](https://kiwix.org) / openZIM. Map data is © [OpenStreetMap](https://www.openstreetmap.org/copyright)
contributors, via [Protomaps](https://protomaps.com). Bundled libraries are listed in
[`web/lib/THIRD_PARTY.md`](web/lib/THIRD_PARTY.md).
