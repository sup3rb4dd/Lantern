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
  Book collections (Water Treatment, Knots, Food Preparation…) are PDFs, so Lantern reads them
  once with a small PDF reader it downloads for you and searches inside them too.
  An optional **AI helper** (runs on the PC, fully offline) answers plain questions like
  `?ask I have a small cut, how do I close it` with the right pages and a short answer from them.
  See [RELAY.md](RELAY.md).
- **Lantern Deck.** An optional touchscreen panel for Relay on an ESP32 "Cheap Yellow Display",
  plugged into the PC by USB (no WiFi): team messages, nodes heard, Relay status and quick replies.
  Flash it from the browser at https://sup3rb4dd.github.io/Lantern/. See [deck/README.md](deck/README.md).
- **Share on my network** (optional): open Lantern from an iPad, phone or another computer on
  the same WiFi. Visitors can read; only the Lantern PC can change things. Optional PIN.

## Works with FieldMap

Lantern pairs with [FieldMap](https://github.com/sup3rb4dd/fieldmap), a free, simple ATAK-style
team map for Android that runs over Meshtastic and GMRS radios. Together they turn Lantern into a
reference desk your team can reach over the mesh, with no cell service or internet anywhere:

```
FieldMap (phone) ──Bluetooth── your Meshtastic node ~~~ mesh ~~~ gateway node 🤖 ──USB/WiFi── PC running Lantern
```

1. Leave Lantern running at home or base camp with a Meshtastic node plugged in (or on WiFi), and
   turn on **Relay**. Add your team's nodes to its allowed list.
2. In the field, open FieldMap's **☰ ▸ Ask Lantern**, pick the gateway node once, and type a question,
   like `bleach drops per gallon`.
3. Lantern searches everything on the drive (Wikipedia, WikiMed, and inside its PDF guides) and
   radios back the top matches. Tap one to read it a few messages at a time.
4. Every answer is saved on the phone, so it can be read again later with no signal at all.

Setup details and the message format are in [RELAY.md](RELAY.md). Any Meshtastic app can ask too,
by sending the gateway node a direct message such as `?s water purification`.

## Using it

1. Format a drive as **exFAT** (FAT32 can't hold files over 4 GB).
2. Download the latest zip from [Releases](https://github.com/sup3rb4dd/lantern/releases/latest) (or build it, below), then copy the contents of its `Lantern` folder to the root of the drive.
3. Double-click `Lantern.exe`. The exe isn't code-signed, so Windows SmartScreen will warn:
   click **More info**, then **Run anyway**.
4. Open **Downloads**, choose what you want, and leave the Lantern window open until it finishes.

Approximate sizes (from the Kiwix catalog, November 2026): Wikipedia full 127 GB, Wikipedia text only 53 GB,
all of Project Gutenberg 221 GB (subjects range from under 1 GB to about 40 GB), WikiMed 1.9 GB, iFixit 3.6 GB.

## Building

Requires [Go](https://go.dev/dl) 1.24 or newer. There are no other dependencies. Run the tests with `cd launcher && go test .`

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
  deck.go        Lantern Deck: finds the CYD panel on USB and feeds it Relay's status
  ai.go          AI helper: downloads and runs llama-server; relay_ask.go is ?ask
  relay_pdf.go   PDF books: reads PDFs in ZIMs once (pdftext.go, zimdir.go), searches pages
  sys_*.go       Windows job object, hidden child windows, free space, browser launch
web/        Home, Maps, Relay and Downloads pages, plus bundled map libraries (web/lib)
package/    README.txt shipped on the drive
deck/       Lantern Deck firmware (PlatformIO) and its web flasher
motion-detector/  Motion Detector: LD2450 people radar firmware for the CYD (separate from Lantern)
```

On the drive, Lantern keeps everything under a `lantern` folder next to the exe:
`web`, `zim` (Kiwix content), `maps` (`.pmtiles`), `kiwix` and `tools` (downloaded automatically),
`maps-assets` (map fonts and icons), `temp` (unfinished downloads), `relay.json` (Relay settings),
and `models` (the AI helper's model, if set up).

## Known limitations

- Windows 64-bit only for now.
- iFixit's offline copy has the step-by-step repair guides but not the PDF manuals iFixit hosts.
- Map search only finds names in the part of the map currently loaded on screen.

## Credits

Content comes from Wikipedia, Project Gutenberg, iFixit and others, packaged by
[Kiwix](https://kiwix.org) / openZIM. Map data is © [OpenStreetMap](https://www.openstreetmap.org/copyright)
contributors, via [Protomaps](https://protomaps.com). Bundled libraries are listed in
[`web/lib/THIRD_PARTY.md`](web/lib/THIRD_PARTY.md).
