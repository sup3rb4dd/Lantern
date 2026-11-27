# Lantern Deck

A touchscreen status panel for Lantern Relay, on the ESP32 "Cheap Yellow Display"
(ESP32-2432S028R, 2.8" 320×240). It plugs into the PC running Lantern by USB and
uses no WiFi or Bluetooth.

**Flash it from the browser:** https://sup3rb4dd.github.io/lantern/ (Chrome or Edge on a computer).

## Screens

- **MSG**: the team's channel messages, questions sent to Relay and what Relay did, newest at
  the bottom. Four quick-reply buttons: tap one, then tap **SEND?** within 3 seconds. Replies go
  out on Relay's reply channel. Change them in Lantern under Relay › Settings.
- **NODES**: every node the gateway has heard, with last heard, SNR and battery (`USB` = on power).
  Grey = not heard in 15 minutes.
- **RELAY**: Relay on/off and port, the gateway node and channel, questions today, the AI helper's
  state, and the last question (or the problem, if Relay can't reach its node).

With no word from Lantern for 12 seconds it shows **Waiting for Lantern** with setup steps.

## How it talks to Lantern

JSON, one object per line, over the CYD's USB serial port at 115200 baud.

| Direction | Line |
|---|---|
| Deck → Lantern | `{"t":"hello","fw":"lantern-deck","v":"1.0.0"}` every 2 s (5 s once linked) |
| Deck → Lantern | `{"t":"send","id":3,"text":"ACK"}` when a quick reply is sent |
| Lantern → Deck | `{"t":"state",...}` every 2 s: clock, Relay state, nodes, last 10 messages, quick replies |
| Lantern → Deck | `{"t":"ack","id":3,"ok":true}` (or `"ok":false,"err":"..."`) |

Lantern finds the Deck by listening for the hello line on USB serial ports that Relay isn't
using, while Relay is on. It only listens on a port when one appears (the Deck was plugged in) or
every few minutes, so other USB devices are left alone. Settings in `lantern/relay.json`:
`deck` (`auto`, `off`, or a port like `COM7`) and `deckReplies` (up to 4, 24 characters each).
Text is sent as plain ASCII (emoji are dropped) because the Deck uses the display's built-in fonts.

## Building

```
pip install platformio
cd deck
pio run                 # both versions, plus .pio/build/<env>/lantern-deck-<env>.bin (flash at 0x0)
pio run -e cyd -t upload
```

Two builds: `cyd` (most boards) and `cyd-inverted` (boards whose colors come out inverted, often
the two-USB-port version). The `deck.yml` workflow builds both and publishes the web flasher on
every push to `main` that touches `deck/`.
