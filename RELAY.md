# Lantern Relay

Lantern Relay answers questions sent over a Meshtastic mesh with short answers from the
content on the Lantern drive. It is built into `Lantern.exe` and needs no internet: search
runs on the drive with the full-text index inside each `.zim` file.

```
phone (FieldMap or Meshtastic app) ──BLE── field node ~~~ mesh ~~~ gateway node 🤖 ──USB── PC running Lantern
```

## Setting it up

1. **Gateway node.** Flash stock Meshtastic, set region US, and scan your team's channel QR code
   so it shares the same private channel. Put 🤖 in its name (e.g. `Lantern Relay 🤖`).
   Mount the antenna high.
2. Plug the node into the Lantern PC by USB. Close the Meshtastic app or web flasher on the PC,
   because only one program can use the port.
3. Open Lantern, go to **Relay**, add your team's nodes to **Who can ask**, and press **Turn Relay on**.
   The **Node connection** setting is usually *Find it automatically*. For a node on WiFi,
   choose *WiFi* and enter its address (Meshtastic's TCP API, port 4403).

Settings are saved in `lantern/relay.json` on the drive:

| Setting | Default | Meaning |
|---|---|---|
| `enabled` | `false` | Connect and answer when Lantern starts |
| `port` | `auto` | `auto`, a port like `COM5` or `/dev/ttyACM0`, or `tcp:ADDRESS` |
| `channel` | `0` | Channel index replies are sent on (0 = primary) |
| `allowedNodes` | `[]` | Node IDs that may ask, like `!a1b2c3d4`. Everyone else is ignored silently. |
| `chunkBytes` | `200` | Most bytes in one message (80–228). Emoji take 4 bytes. |
| `pauseSeconds` | `5` | Wait after each message is acknowledged (or after 30 s with no acknowledgement) |
| `messagesPerReply` | `3` | Messages sent per answer before the asker sends `?more` |

## Commands

Questions must be **direct messages to the gateway node**. Channel messages are ignored, so
bot traffic never lands in team chat.

| Send | Gets back |
|---|---|
| `?s water purification` | Top 3 matches from everything on the drive |
| `water purification` | Same as `?s` (plain words are a search) |
| `?1` `?2` `?3` (or `?4`… after `?more`) | The start of that result |
| `?more` | The next part, continuing into the next section; after a search, the next 3 results |
| `?toc` | The article's sections |
| `?go 4` | Section 4 |
| `?help` | The command list |

A new command from the same node cancels any of its replies still waiting to go out.

## Reply formats

FieldMap's Ask Lantern screen reads these, so keep them stable. Each is one Meshtastic text message.

**Search results.** The first line is `🔎 ` plus the query. Each result line is `N Title [Source]`:
```
🔎 water purification
1 Water purification [Wiki]
2 Emergency water treatment [Water]
3 Water filter [Wiki]
```
No results: the second line is `No matches. Try fewer or different words.`
Source labels: `Wiki`, `Med` (WikiMed), `Book` (Gutenberg), `iFixit`, `Wikibooks`, `Travel`, `wikiHow`,
`Survival`, `Water`, `Food`, `Knots`, or the first word of the ZIM's title.

**Article text.** `[p/P] ` then text, where p/P counts messages within the current section.
The first message of a section has a heading line: the article title and source for the
summary (section 0), or `§ ` + the section name otherwise. Smaller headings appear inline as `— Name —`.
```
[1/6] Water purification [Wiki]
Water purification is the process of removing undesirable chemicals…
[2/6] The goal is to produce water fit for specific purposes…
```

**Sections.** `☰ p/P`, then one `N Name` line per section (0 is always the summary):
```
☰ 1/1
0 Summary
1 History
2 Boiling
```

**Notices.** Start with `Lantern: `, for example `Lantern: search first: ?s words`.

## Why not GMRS

FieldMap's GMRS mode can't reach Relay on purpose: automated replies are a gray area under the
GMRS rules, so Relay is Meshtastic only.
