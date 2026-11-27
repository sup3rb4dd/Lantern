# Motion Detector

A people radar on the ESP32 "Cheap Yellow Display" (ESP32-2432S028R) with the Hi-Link
**HLK-LD2450** 24 GHz mmWave sensor. Up to 3 moving people show as blips on a motion-tracker
fan (±60°, out to 6 m / 20 ft) with trails, the distance to each, a contact count and the
nearest distance. No WiFi or Bluetooth.

It's its own firmware: flashing it replaces whatever was on the CYD (Lantern Deck, for example),
and flashing Lantern Deck replaces it.

**Flash it from the browser:** https://sup3rb4dd.github.io/Lantern/motion-detector/ (Chrome or Edge on a computer).

Without the sensor it runs a demo (amber `DEMO-NO SENSOR`), and switches to the sensor as soon
as it hears one.

## Wiring

| LD2450 | CYD |
|---|---|
| 5V | P1 `VIN` |
| GND | P1 `GND` |
| TX | CN1 `IO27` |
| RX | CN1 `IO22` |

CN1's 3.3V pin isn't used: the sensor needs 5V. The sensor's data pins are 3.3 V, so they connect
straight to the CYD. Power the CYD from USB.

## Touch

Tap a corner: top-left sound (ping on each new contact, speaker on the SPEAK connector), top-right
mirror left/right, bottom-left demo on/off, bottom-right range 6/4/3 m. Settings survive power-off.
The RGB LED is amber while anyone is in view and red when someone is within 1.5 m.

## Building

```
pip install platformio
cd motion-detector
pio run                 # both versions + .pio/build/<env>/motion-detector-<env>.bin (flash at 0x0)
pio run -e cyd -t upload
```

`cyd` is for most boards; `cyd-inverted` for boards whose colors come out inverted. The
`deck.yml` workflow builds both and publishes the flasher.
