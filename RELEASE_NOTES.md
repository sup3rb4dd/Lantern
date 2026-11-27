**Download the `Lantern-v….zip` file** below, unzip it, and copy everything inside its `Lantern` folder to the root of a USB drive (or any folder). Run `Lantern.exe` and use the **Downloads** page to add Wikipedia, books and maps. Windows 64-bit. No install needed.

### New in 1.11
- **Lantern Deck:** plug an ESP32 "Cheap Yellow Display" into the Lantern PC by USB for a touchscreen Relay panel: the team's messages, the nodes the gateway has heard (signal, battery), Relay and AI helper status, and quick-reply buttons (ACK, EN ROUTE, ALL CLEAR, HOLD) that send on the team channel. No WiFi. Flash it from your browser at https://sup3rb4dd.github.io/Lantern/ and Lantern finds it by itself while Relay is on. Set it up under Relay › Settings.
- Relay now keeps track of each node's signal (SNR) and battery.

### New in 1.10.1
- AI helper answers more often: it now reads the top 3 pages it found (not just the best one), picks the right sections of long articles better, and gives what the pages do cover instead of "Not covered here". Answers name the page they used, like "(from 2)".

### New in 1.10
- **New look to match FieldMap:** black with phosphor green, HUD-style panels and a monospace font that works offline.
- **Settings page** to change the colors: four presets (FieldMap Green, Amber Terminal, Night Red for keeping your night vision, Arctic) or pick each color yourself, with a live preview. Choose whether devices viewing over the network use it too, and dark or light maps.

### New in 1.9
- **Share on my network** (Home page, off by default): open Lantern from an iPad, phone or another computer on the same WiFi at `http://<this PC>:8488/`. Visitors can read everything, browse maps and try Relay questions; only the Lantern PC can change settings, download or delete. Optional PIN. Works on a Windows Mobile Hotspot with no internet too.

### New in 1.8.2
- The AI helper now assumes you're off-grid: it no longer tells you to call 911 or see a doctor, and gives the steps you can do yourself. Serious situations are still called serious, with warning signs to watch for.

### New in 1.8.1
- Fixed "no Windows build in the latest llama.cpp release" when setting up the AI helper. llama.cpp publishes new versions several times a day, and the newest one can show up before its Windows files finish uploading; Lantern now uses the newest release that has them.

### New in 1.8
- **AI helper for Lantern Relay** (optional, fully offline). Ask in plain words over the radio: `?ask I have a small cut, how do I close it`. The AI finds the right pages on the drive (Wikipedia, WikiMed, PDF books) and can write a short answer from the best one, naming its source. Medical answers say first when something needs a doctor.
- One button on the Relay page sets it up: it downloads llama.cpp and a free Qwen3 model (about 2.5 GB) into the Lantern folder. Nothing is installed into Windows, it only runs while questions come in, and it's skipped on PCs without enough memory. Without it, `?ask` is a keyword search.
- **Try it here** box on the Relay page, to test questions and commands without a radio.
- Book collections' start pages no longer show up as a useless "home" result.

### New in 1.7.2
- Bullets in Wikibooks' printable PDFs (which put each bullet at the end of the line before) now get their own lines too. No re-read needed.

### New in 1.7.1
- Lists in PDF books (bullets, numbered steps) keep their line breaks instead of running together. The first time 1.7.1 starts, it reads your PDF books once more to pick this up; after that, formatting fixes never need a re-read.

### New in 1.7
- **Lantern Relay**: plug a Meshtastic node into the Lantern PC (USB or WiFi), and your team can search the drive over the mesh, from FieldMap's **Ask Lantern** screen or any Meshtastic app (`?s water purification`). Answers come back in short, paced messages, and only nodes you allow get answers. See [RELAY.md](https://github.com/sup3rb4dd/lantern/blob/main/RELAY.md).
- **Searches inside PDF books**: book collections like Water Treatment, Knots, Food Preparation and the USDA canning guides are read once, so radio searches find the right page ("Purifying Water During an Emergency p.2"). Lantern downloads the small PDF reader it needs on first use.
- Works with real Wikipedia and WikiMed pages, with citations and clutter stripped.
- **Send test message** button to check the gateway node can transmit.

Updating from an earlier version: copy the new files over the old ones. Your downloaded content and settings are kept.
