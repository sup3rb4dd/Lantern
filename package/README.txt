LANTERN - offline knowledge drive
=================================

SETTING UP THE DRIVE (one time)
1. Format the drive as exFAT. (FAT32 can't hold files bigger than 4 GB, and
   Wikipedia is one ~100 GB file.) In Windows: right-click the drive in File
   Explorer > Format > File system: exFAT.
2. Copy everything in this folder (Lantern.exe, README.txt and the "lantern"
   folder) to the top level of the drive.
3. Double-click Lantern.exe. Your browser opens to the Lantern home page.
   - Windows may show "Windows protected your PC" because the app isn't signed.
     Click "More info", then "Run anyway".
   - If Windows Firewall asks, you can click Cancel. Lantern only talks to
     your own computer.
4. Click Downloads, pick what you want, and press Download. Leave the
   Lantern window open until it finishes. Big items take hours.
   If a download stops, press Download again and it continues from where
   it left off.

USING IT (no internet needed)
- Plug the drive into any Windows PC and double-click Lantern.exe.
- Close the black Lantern window when you're done.
- Use "Safely Remove Hardware" before unplugging, especially after downloads.

WHAT'S IN THE "lantern" FOLDER
  web\          the home page, map viewer and downloads page
  zim\          Wikipedia, books and guides (Kiwix .zim files)
  maps\         offline maps (.pmtiles files)
  kiwix\        the Kiwix reader (downloaded automatically)
  tools\        the map download tool (downloaded automatically)
  maps-assets\  map fonts and icons (downloaded automatically)
  temp\         unfinished downloads (safe to empty when nothing is downloading)
  source\       Lantern's source code (Go), if you want to change it

You can also copy .zim files from https://library.kiwix.org straight into
the zim folder, and they will show up the next time Lantern starts.

CREDITS
Content: Wikipedia, Project Gutenberg and others via Kiwix (kiwix.org).
Maps: (c) OpenStreetMap contributors, via Protomaps (protomaps.com).
Map display: MapLibre GL JS, PMTiles, @protomaps/basemaps.
