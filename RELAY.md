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
| `?ask I have a small cut, how do I close it` | With the AI helper: a short answer from the best page, then the best pages. Without it: a search on the question's key words |
| `?s water purification` | Top 3 matches from everything on the drive |
| `water purification` | Same as `?s` (plain words are a search). With the AI helper on, a plain question of 3 or more words goes to `?ask` |
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
summary (section 0), or `§N Name` for section N (e.g. `§2 Boiling`). Smaller headings appear
inline as `— Name —`.
```
[1/6] Water purification [Wiki]
Water purification is the process of removing undesirable chemicals…
[2/6] The goal is to produce water fit for specific purposes…
```

**PDF books.** Book collections (like Water Treatment) are mostly PDFs, which Kiwix's search
can't look inside. Lantern reads each PDF once (with pdftotext, downloaded on first use) and
keeps its own index in `lantern/pdfindex`. A PDF result names its page: `2 Emergency Water
Treatment p.3 [Water]`. Opening it starts at that page. In a PDF, section 0 is an overview and
section N is page N, so `?go 14` reads page 14 and `?toc` lists the pages as `14 p.14 Title`.
Scanned PDFs (pictures of pages) have no text and are skipped. The Relay page shows progress.

**Sections.** `☰ p/P`, then one `N Name` line per section (0 is always the summary):
```
☰ 1/1
0 Summary
1 History
2 Boiling
```

**AI answers.** `🤖 ` then a short answer, ending `(from 1)`, which names the result it came
from. It is followed by the usual search-results message (header `🔎 ` plus the question), so
`?1`, `?2`, `?more` work as after a search. In "just find the pages" mode there is no 🤖 message.
```
🤖 Clean with running water and press to stop bleeding. Deep or gaping cuts need closing: pull the edges together with strips. (from 1)
🔎 I have a small cut, how do I close it
1 Wound closure [Med]
2 First Aid p.48 [Wikibooks]
3 Laceration [Med]
```

**Notices.** Start with `Lantern: `, for example `Lantern: search first: ?s words`.

## AI helper

Optional. The Relay page's **Set up AI helper** button downloads two things into the Lantern
folder: llama.cpp's `llama-server` (Windows Vulkan build, which uses a graphics card when there is
one and falls back to the processor) into `lantern/tools/llama`, and a free Qwen3 model into
`lantern/models` (Qwen3 4B, about 2.5 GB, on PCs with 8 GB of memory or more; Qwen3 1.7B on
smaller ones). Nothing is installed into Windows, so the drive still runs on any PC.

The AI only runs while questions come in: Lantern starts `llama-server` on the first `?ask`
(loading takes up to a minute from a USB drive) and stops it after 15 idle minutes. It is skipped
if the PC doesn't have enough free memory, and `?ask` falls back to a keyword search whenever the
AI is off or fails.

How `?ask` works: the AI turns the question into library search words ("small cut, stitch it" →
"wound closure", "laceration"), Lantern searches Wikipedia, WikiMed and the PDF books with them,
the AI picks the most useful pages, and (in answer mode) writes up to 3 short sentences using only
the top 3 pages, naming the one it used most ("from 2"). It assumes you're off-grid: it never tells you to call 911 or see a doctor
(those lines are also filtered out if the model adds them anyway). For serious medical
situations it says so plainly, puts the most important steps first and names warning signs.

The Relay page has a **Try it here** box to ask from the PC without a radio. To use an AI server
that's already running instead (LM Studio, Ollama's OpenAI API, another llama-server), start
Lantern with `LANTERN_AI_URL=http://127.0.0.1:PORT`.

## Lantern Deck

An optional touchscreen panel (an ESP32 "Cheap Yellow Display") plugged into the Lantern PC by
USB. It shows the team's messages, the nodes the gateway has heard and Relay's status, and has
quick-reply buttons (`ACK`, `EN ROUTE`, `ALL CLEAR`, `HOLD` by default) that send on the reply
channel. Lantern finds it on USB by itself while Relay is on. Flash it from
https://sup3rb4dd.github.io/lantern/ and see [deck/README.md](deck/README.md).

| Setting | Default | Meaning |
|---|---|---|
| `deck` | `auto` | `auto`, `off`, or the Deck's port like `COM7` |
| `deckReplies` | 4 buttons | Up to 4 quick replies, 24 characters each |

## Why not GMRS

FieldMap's GMRS mode can't reach Relay on purpose: automated replies are a gray area under the
GMRS rules, so Relay is Meshtastic only.
