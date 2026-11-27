// Lantern Deck: a status panel for Lantern Relay on the ESP32 "Cheap Yellow Display".
//
// It plugs into the PC running Lantern by USB. No WiFi or Bluetooth is used.
// Lantern sends one JSON line every 2 seconds ({"t":"state",...}); the Deck
// draws three screens from it (messages, nodes, Relay) and sends quick replies
// back ({"t":"send","id":N,"text":"ACK"}). It says hello every few seconds so
// Lantern can find it among the PC's USB serial ports.

#include <Arduino.h>
#include <ArduinoJson.h>
#include <SPI.h>
#include <TFT_eSPI.h>
#include <XPT2046_Touchscreen.h>

#ifndef DECK_VERSION
#define DECK_VERSION "dev"
#endif

// ---------- hardware ----------

#define TOUCH_CS 33
#define TOUCH_IRQ 36
#define TOUCH_MOSI 32
#define TOUCH_MISO 39
#define TOUCH_CLK 25

TFT_eSPI tft;
SPIClass touchSpi(VSPI);
XPT2046_Touchscreen touch(TOUCH_CS, TOUCH_IRQ);

static const int W = 320, H = 240;

// ---------- colors (FieldMap / Lantern green on black) ----------

static uint16_t rgb(uint8_t r, uint8_t g, uint8_t b) { return tft.color565(r, g, b); }
static uint16_t C_BG, C_BAR, C_LINE, C_PANEL, C_MINE, C_EDGE, C_TEXT, C_MUTED, C_ACCENT, C_AMBER, C_RED, C_GREY, C_DIM;

static void initColors() {
  C_BG = rgb(5, 8, 6);
  C_BAR = rgb(10, 18, 12);
  C_LINE = rgb(29, 58, 36);
  C_PANEL = rgb(10, 20, 13);
  C_MINE = rgb(16, 38, 26);
  C_EDGE = rgb(46, 92, 58);
  C_TEXT = rgb(201, 245, 211);
  C_MUTED = rgb(143, 191, 153);
  C_ACCENT = rgb(124, 252, 154);
  C_AMBER = rgb(255, 181, 71);
  C_RED = rgb(255, 99, 88);
  C_GREY = rgb(74, 90, 78);
  C_DIM = rgb(138, 154, 142);
}

// ---------- what Lantern told us ----------

struct Msg {
  char from[24];
  char text[164];
  long ago;
  char kind;  // c chat, d direct to Relay, r Relay's action, m sent from Lantern
};
struct Node {
  char name[14];
  long ago;
  char snr[8];
  char bat[6];
};

struct State {
  char clock[6] = "--:--";
  char relay[12] = "off";
  char detail[64] = "";
  char gw[26] = "";
  char port[16] = "";
  int ch = 0;
  int up = 0;
  int q = 0;
  char lastq[64] = "";
  long lastqAgo = 0;
  char ai[12] = "OFF";
  bool search = false;
  Msg msgs[10];
  int nMsgs = 0;
  Node nodes[8];
  int nNodes = 0;
  char replies[4][26] = {"ACK", "EN ROUTE", "ALL CLEAR", "HOLD"};
  int nReplies = 4;
};

static State st;
static bool linked = false;          // heard from Lantern recently
static uint32_t lastStateMs = 0;     // when the last state line arrived
static uint32_t lastHelloMs = 0;

enum Tab { TAB_MSG, TAB_NODES, TAB_RELAY };
static Tab tab = TAB_MSG;

// quick-reply button states
enum BtnMode { B_IDLE, B_ARMED, B_SENDING, B_SENT, B_FAILED };
static BtnMode btnMode[4] = {B_IDLE, B_IDLE, B_IDLE, B_IDLE};
static uint32_t btnSince[4] = {0, 0, 0, 0};
static int sendId = 0;
static int pendingId = -1, pendingBtn = -1;
static char toast[64] = "";
static uint32_t toastUntil = 0;

static uint32_t drawnHeader = 0, drawnBody = 0, drawnButtons = 0;
static bool forceDraw = true;

static void copyStr(char *dst, size_t n, const char *src) {
  if (!src) src = "";
  strncpy(dst, src, n - 1);
  dst[n - 1] = 0;
}

static uint32_t fnv(uint32_t h, const char *s) {
  while (*s) {
    h ^= (uint8_t)*s++;
    h *= 16777619u;
  }
  return h * 16777619u;
}
static uint32_t fnvInt(uint32_t h, long v) {
  char b[16];
  snprintf(b, sizeof b, "%ld", v);
  return fnv(h, b);
}

// "now", "4m", "2h", "3d"
static void agoText(long s, char *out, size_t n) {
  if (s < 0) {
    copyStr(out, n, "never");
  } else if (s < 60) {
    copyStr(out, n, "now");
  } else if (s < 3600) {
    snprintf(out, n, "%ldm", s / 60);
  } else if (s < 172800) {
    snprintf(out, n, "%ldh", s / 3600);
  } else {
    snprintf(out, n, "%ldd", s / 86400);
  }
}
static long agoBucket(long s) { return s < 60 ? 0 : s < 3600 ? s / 60 : s < 172800 ? 100 + s / 3600 : 1000 + s / 86400; }

// ---------- serial link ----------

static void sendHello() {
  Serial.printf("{\"t\":\"hello\",\"fw\":\"lantern-deck\",\"v\":\"%s\"}\n", DECK_VERSION);
  lastHelloMs = millis();
}

static void sendReply(int b) {
  JsonDocument doc;
  doc["t"] = "send";
  doc["id"] = ++sendId;
  doc["text"] = st.replies[b];
  serializeJson(doc, Serial);
  Serial.print('\n');
  pendingId = sendId;
  pendingBtn = b;
  btnMode[b] = B_SENDING;
  btnSince[b] = millis();
}

static void showToast(const char *t) {
  copyStr(toast, sizeof toast, t);
  toastUntil = millis() + 3000;
  forceDraw = true;
}

static void handleLine(const char *line) {
  JsonDocument doc;
  if (deserializeJson(doc, line)) return;
  const char *t = doc["t"] | "";
  if (!strcmp(t, "ack")) {
    int id = doc["id"] | -1;
    if (id == pendingId && pendingBtn >= 0) {
      bool ok = doc["ok"] | false;
      btnMode[pendingBtn] = ok ? B_SENT : B_FAILED;
      btnSince[pendingBtn] = millis();
      if (!ok) showToast(doc["err"] | "Not sent");
      pendingId = pendingBtn = -1;
    }
    return;
  }
  if (strcmp(t, "state")) return;

  copyStr(st.clock, sizeof st.clock, doc["clock"] | "--:--");
  copyStr(st.relay, sizeof st.relay, doc["relay"] | "off");
  copyStr(st.detail, sizeof st.detail, doc["detail"] | "");
  copyStr(st.gw, sizeof st.gw, doc["gw"] | "");
  copyStr(st.port, sizeof st.port, doc["port"] | "");
  st.ch = doc["ch"] | 0;
  st.up = doc["up"] | 0;
  st.q = doc["q"] | 0;
  copyStr(st.lastq, sizeof st.lastq, doc["lastq"] | "");
  st.lastqAgo = doc["lastqAgo"] | 0;
  copyStr(st.ai, sizeof st.ai, doc["ai"] | "OFF");
  st.search = doc["search"] | false;

  st.nMsgs = 0;
  for (JsonObject m : doc["msgs"].as<JsonArray>()) {
    if (st.nMsgs >= 10) break;
    Msg &x = st.msgs[st.nMsgs++];
    copyStr(x.from, sizeof x.from, m["f"] | "?");
    copyStr(x.text, sizeof x.text, m["x"] | "");
    x.ago = m["ago"] | 0;
    const char *k = m["k"] | "c";
    x.kind = k[0];
  }
  st.nNodes = 0;
  for (JsonObject n : doc["nodes"].as<JsonArray>()) {
    if (st.nNodes >= 8) break;
    Node &x = st.nodes[st.nNodes++];
    copyStr(x.name, sizeof x.name, n["n"] | "?");
    x.ago = n["ago"] | -1;
    copyStr(x.snr, sizeof x.snr, n["snr"] | "--");
    copyStr(x.bat, sizeof x.bat, n["bat"] | "--");
  }
  JsonArray r = doc["replies"].as<JsonArray>();
  if (r.size() > 0) {
    int i = 0;
    for (const char *s : r) {
      if (i >= 4) break;
      copyStr(st.replies[i++], sizeof st.replies[0], s);
    }
    st.nReplies = i;
  }
  lastStateMs = millis();
  if (!linked) {
    linked = true;
    forceDraw = true;
  }
}

static char lineBuf[6144];
static size_t lineLen = 0;

static void readSerial() {
  while (Serial.available()) {
    int c = Serial.read();
    if (c == '\n') {
      lineBuf[lineLen] = 0;
      if (lineLen > 0 && lineBuf[0] == '{') handleLine(lineBuf);
      lineLen = 0;
    } else if (c != '\r') {
      if (lineLen < sizeof lineBuf - 1) {
        lineBuf[lineLen++] = (char)c;
      } else {
        lineLen = 0;  // too long: drop it
      }
    }
  }
}

// ---------- drawing helpers ----------

static const int HEADER_H = 22, TAB_Y = 212, TAB_H = 28;
static const int BTN_Y = 180, BTN_H = 30;

static void textAt(const char *s, int x, int y, uint16_t fg, uint16_t bg, int font, uint8_t datum = TL_DATUM) {
  tft.setTextDatum(datum);
  tft.setTextColor(fg, bg);
  tft.drawString(s, x, y, font);
}

// Fits s into maxW pixels, adding ".." if it had to be cut.
static void fitText(const char *s, int maxW, int font, char *out, size_t n) {
  copyStr(out, n, s);
  if (tft.textWidth(out, font) <= maxW) return;
  size_t len = strlen(out);
  while (len > 0) {
    out[--len] = 0;
    char tmp[200];
    snprintf(tmp, sizeof tmp, "%s..", out);
    if (tft.textWidth(tmp, font) <= maxW) {
      copyStr(out, n, tmp);
      return;
    }
  }
}

// Word-wraps s into up to maxLines lines of maxW pixels. Returns the line count.
static int wrapText(const char *s, int maxW, int font, char lines[][90], int maxLines) {
  int count = 0;
  const char *p = s;
  while (*p && count < maxLines) {
    while (*p == ' ') p++;
    if (!*p) break;
    char cur[90] = "";
    size_t curLen = 0;
    const char *lastBreak = nullptr;
    size_t breakLen = 0;
    const char *q = p;
    while (*q) {
      if (curLen >= sizeof cur - 1) break;
      cur[curLen] = *q;
      cur[curLen + 1] = 0;
      if (tft.textWidth(cur, font) > maxW) {
        cur[curLen] = 0;
        break;
      }
      if (*q == ' ') {
        lastBreak = q;
        breakLen = curLen;
      }
      curLen++;
      q++;
    }
    if (*q && lastBreak && lastBreak > p) {  // break at the last space
      cur[breakLen] = 0;
      q = lastBreak + 1;
    }
    if (count == maxLines - 1 && *q) {  // last line and more text: end with ".."
      char tmp[96];
      snprintf(tmp, sizeof tmp, "%s..", cur);
      while (tft.textWidth(tmp, font) > maxW && strlen(cur) > 0) {
        cur[strlen(cur) - 1] = 0;
        snprintf(tmp, sizeof tmp, "%s..", cur);
      }
      copyStr(cur, sizeof cur, tmp);
    }
    copyStr(lines[count++], 90, cur);
    p = q;
  }
  return count;
}

static void dot(int x, int y, uint16_t c) { tft.fillCircle(x, y, 4, c); }

static uint16_t relayColor() {
  if (!strcmp(st.relay, "on")) return C_ACCENT;
  if (!strcmp(st.relay, "connecting")) return C_AMBER;
  if (!strcmp(st.relay, "problem")) return C_RED;
  return C_GREY;
}

// ---------- header and tabs ----------

static void drawHeader(bool force) {
  uint32_t h = fnv(2166136261u, st.clock);
  h = fnv(h, st.relay);
  h = fnvInt(h, st.up);
  h = fnvInt(h, linked);
  if (!force && h == drawnHeader) return;
  drawnHeader = h;
  tft.fillRect(0, 0, W, HEADER_H, C_BAR);
  tft.drawFastHLine(0, HEADER_H - 1, W, C_LINE);
  textAt("LANTERN", 6, 3, C_ACCENT, C_BAR, 2);
  textAt(st.clock, W - 6, 3, C_TEXT, C_BAR, 2, TR_DATUM);
  char up[12];
  snprintf(up, sizeof up, "%d UP", st.up);
  int x = W - 6 - tft.textWidth(st.clock, 2) - 12;
  textAt(up, x, 3, C_MUTED, C_BAR, 2, TR_DATUM);
  x -= tft.textWidth(up, 2) + 12;
  textAt("RELAY", x, 3, C_TEXT, C_BAR, 2, TR_DATUM);
  x -= tft.textWidth("RELAY", 2) + 8;
  dot(x, 11, linked ? relayColor() : C_GREY);
}

static void drawTabs() {
  const char *names[3] = {"MSG", "NODES", "RELAY"};
  tft.drawFastHLine(0, TAB_Y, W, C_LINE);
  int w = W / 3;
  for (int i = 0; i < 3; i++) {
    int x = i * w;
    int ww = i == 2 ? W - x : w;
    bool on = i == (int)tab;
    tft.fillRect(x, TAB_Y + 1, ww, TAB_H - 1, on ? C_ACCENT : C_BG);
    if (i > 0 && !on && (int)tab != i - 1) tft.drawFastVLine(x, TAB_Y + 1, TAB_H - 1, C_LINE);
    textAt(names[i], x + ww / 2, TAB_Y + 7, on ? C_BG : C_MUTED, on ? C_ACCENT : C_BG, 2, TC_DATUM);
  }
}

// ---------- messages screen ----------

static void msgColors(const Msg &m, uint16_t &bg, uint16_t &edge, uint16_t &name) {
  bg = C_PANEL;
  edge = C_LINE;
  name = C_TEXT;
  if (m.kind == 'm') {
    bg = C_MINE;
    edge = C_EDGE;
    name = C_ACCENT;
  } else if (m.kind == 'r') {
    name = C_AMBER;
  } else if (m.kind == 'd') {
    name = C_MUTED;
  }
}

static uint32_t msgHash() {
  uint32_t h = 2166136261u;
  for (int i = 0; i < st.nMsgs; i++) {
    h = fnv(h, st.msgs[i].from);
    h = fnv(h, st.msgs[i].text);
    h = fnvInt(h, agoBucket(st.msgs[i].ago));
  }
  h = fnv(h, st.relay);
  h = fnv(h, toastUntil > millis() ? toast : "");
  return h;
}

static void drawMessages() {
  const int top = HEADER_H + 2, bottom = BTN_Y - 4;
  tft.fillRect(0, HEADER_H, W, bottom - HEADER_H + 2, C_BG);
  int y = bottom;
  if (toastUntil > millis()) {
    tft.fillRect(4, bottom - 18, W - 8, 18, C_RED);
    char t[70];
    fitText(toast, W - 16, 2, t, sizeof t);
    textAt(t, 8, bottom - 17, C_BG, C_RED, 2);
    y -= 22;
  }
  if (st.nMsgs == 0) {
    const char *t = !strcmp(st.relay, "on") ? "No messages yet." : "Relay isn't on.";
    textAt(t, W / 2, top + 50, C_MUTED, C_BG, 2, TC_DATUM);
    if (strcmp(st.relay, "on")) textAt("Turn it on in Lantern > Relay.", W / 2, top + 72, C_DIM, C_BG, 2, TC_DATUM);
    return;
  }
  for (int i = st.nMsgs - 1; i >= 0; i--) {
    const Msg &m = st.msgs[i];
    bool mine = m.kind == 'm';
    int x = mine ? 44 : 4;
    int w = W - 4 - x;
    char lines[2][90];
    int n = wrapText(m.text, w - 10, 2, lines, 2);
    if (n == 0) {
      n = 1;
      lines[0][0] = 0;
    }
    int hgt = 4 + 9 + n * 16 + 3;
    if (y - hgt < top) break;
    y -= hgt;
    uint16_t bg, edge, name;
    msgColors(m, bg, edge, name);
    tft.fillRoundRect(x, y, w, hgt, 4, bg);
    tft.drawRoundRect(x, y, w, hgt, 4, edge);
    char ago[8], from[30];
    agoText(m.ago, ago, sizeof ago);
    fitText(m.from, w - 50, 1, from, sizeof from);
    textAt(from, x + 5, y + 3, name, bg, 1);
    textAt(ago, x + w - 5, y + 3, C_MUTED, bg, 1, TR_DATUM);
    for (int l = 0; l < n; l++) textAt(lines[l], x + 5, y + 12 + l * 16, C_TEXT, bg, 2);
    y -= 3;
  }
}

static uint32_t buttonHash() {
  uint32_t h = 2166136261u;
  for (int i = 0; i < st.nReplies; i++) {
    h = fnv(h, st.replies[i]);
    h = fnvInt(h, btnMode[i]);
  }
  return h;
}

static void drawButtons() {
  tft.fillRect(0, BTN_Y - 2, W, BTN_H + 4, C_BG);
  int n = st.nReplies > 0 ? st.nReplies : 1;
  int gap = 4, w = (W - 8 - gap * (n - 1)) / n;
  for (int i = 0; i < st.nReplies; i++) {
    int x = 4 + i * (w + gap);
    uint16_t bg = C_PANEL, edge = C_EDGE, fg = C_TEXT;
    const char *label = st.replies[i];
    switch (btnMode[i]) {
      case B_ARMED: bg = C_AMBER; edge = C_AMBER; fg = C_BG; label = "SEND?"; break;
      case B_SENDING: label = "..."; break;
      case B_SENT: bg = C_ACCENT; edge = C_ACCENT; fg = C_BG; label = "SENT"; break;
      case B_FAILED: bg = C_RED; edge = C_RED; fg = C_BG; label = "FAILED"; break;
      default: break;
    }
    tft.fillRoundRect(x, BTN_Y, w, BTN_H, 5, bg);
    tft.drawRoundRect(x, BTN_Y, w, BTN_H, 5, edge);
    char t[30];
    fitText(label, w - 6, 2, t, sizeof t);
    textAt(t, x + w / 2, BTN_Y + 7, fg, bg, 2, TC_DATUM);
  }
}

// ---------- nodes screen ----------

static uint32_t nodeHash() {
  uint32_t h = 2166136261u;
  for (int i = 0; i < st.nNodes; i++) {
    h = fnv(h, st.nodes[i].name);
    h = fnvInt(h, agoBucket(st.nodes[i].ago));
    h = fnv(h, st.nodes[i].snr);
    h = fnv(h, st.nodes[i].bat);
  }
  return h;
}

static void drawNodes() {
  tft.fillRect(0, HEADER_H, W, TAB_Y - HEADER_H, C_BG);
  const int cx[4] = {8, 150, 212, 268};
  const char *heads[4] = {"NODE", "HEARD", "SNR", "BATT"};
  for (int i = 0; i < 4; i++) textAt(heads[i], cx[i] + (i == 0 ? 14 : 0), HEADER_H + 6, C_MUTED, C_BG, 1);
  if (st.nNodes == 0) {
    textAt("No nodes heard yet.", W / 2, 90, C_MUTED, C_BG, 2, TC_DATUM);
    return;
  }
  int y = HEADER_H + 18;
  for (int i = 0; i < st.nNodes && y + 22 <= TAB_Y - 2; i++) {
    const Node &n = st.nodes[i];
    bool live = n.ago >= 0 && n.ago < 900;
    tft.fillRoundRect(4, y, W - 8, 22, 3, C_PANEL);
    tft.drawRoundRect(4, y, W - 8, 22, 3, C_LINE);
    dot(cx[0] + 4, y + 11, live ? C_ACCENT : C_GREY);
    char name[16], ago[8];
    fitText(n.name, cx[1] - cx[0] - 22, 2, name, sizeof name);
    agoText(n.ago, ago, sizeof ago);
    textAt(name, cx[0] + 14, y + 3, live ? C_TEXT : C_DIM, C_PANEL, 2);
    textAt(ago, cx[1], y + 3, live ? C_TEXT : C_DIM, C_PANEL, 2);
    textAt(n.snr, cx[2], y + 3, C_TEXT, C_PANEL, 2);
    textAt(n.bat, cx[3], y + 3, C_TEXT, C_PANEL, 2);
    y += 25;
  }
}

// ---------- Relay screen ----------

static void tile(int x, int y, int w, int h, const char *label, const char *value, uint16_t valColor, const char *sub, int valFont = 2) {
  tft.fillRoundRect(x, y, w, h, 4, C_PANEL);
  tft.drawRoundRect(x, y, w, h, 4, C_LINE);
  textAt(label, x + 7, y + 5, C_MUTED, C_PANEL, 1);
  char v[40];
  fitText(value, w - 14, valFont, v, sizeof v);
  textAt(v, x + 7, y + 16, valColor, C_PANEL, valFont);
  if (sub && *sub) {
    char s[40];
    fitText(sub, w - 14, 1, s, sizeof s);
    textAt(s, x + 7, y + h - 12, C_MUTED, C_PANEL, 1);
  }
}

static uint32_t relayHash() {
  uint32_t h = fnv(2166136261u, st.relay);
  h = fnv(h, st.detail);
  h = fnv(h, st.gw);
  h = fnv(h, st.port);
  h = fnvInt(h, st.ch);
  h = fnvInt(h, st.q);
  h = fnv(h, st.ai);
  h = fnv(h, st.lastq);
  h = fnvInt(h, agoBucket(st.lastqAgo));
  h = fnvInt(h, st.search);
  return h;
}

static void drawRelay() {
  tft.fillRect(0, HEADER_H, W, TAB_Y - HEADER_H, C_BG);
  const int x1 = 6, x2 = 163, tw = 151, th = 50, y1 = HEADER_H + 5, y2 = y1 + th + 5;
  const char *rv = !strcmp(st.relay, "on") ? "ON" : !strcmp(st.relay, "connecting") ? "CONNECTING" : !strcmp(st.relay, "problem") ? "PROBLEM" : "OFF";
  char sub[40] = "";
  if (!strcmp(st.relay, "on")) snprintf(sub, sizeof sub, "USB %s", st.port);
  tile(x1, y1, tw, th, "RELAY", rv, relayColor(), sub);
  char ch[16];
  snprintf(ch, sizeof ch, "channel %d", st.ch);
  tile(x2, y1, tw, th, "GATEWAY NODE", st.gw[0] ? st.gw : "--", C_TEXT, ch);
  char q[8];
  snprintf(q, sizeof q, "%d", st.q);
  tile(x1, y2, tw, th, "QUESTIONS TODAY", q, C_TEXT, "", 4);
  uint16_t aiC = !strcmp(st.ai, "READY") ? C_ACCENT : !strcmp(st.ai, "ERROR") ? C_RED : (!strcmp(st.ai, "OFF") || !strcmp(st.ai, "NOT SET UP")) ? C_DIM : C_AMBER;
  tile(x2, y2, tw, th, "AI HELPER", st.ai, aiC, st.search ? "search ready" : "no books yet");

  int y3 = y2 + th + 5, h3 = TAB_Y - 5 - y3;
  tft.fillRoundRect(x1, y3, W - 12, h3, 4, C_PANEL);
  tft.drawRoundRect(x1, y3, W - 12, h3, 4, C_LINE);
  bool problem = !strcmp(st.relay, "problem") && st.detail[0];
  if (problem) {
    textAt("PROBLEM", x1 + 7, y3 + 5, C_RED, C_PANEL, 1);
  } else {
    char label[40], ago[8];
    agoText(st.lastqAgo, ago, sizeof ago);
    if (st.lastq[0]) {
      snprintf(label, sizeof label, "LAST QUESTION  %s", ago);
    } else {
      copyStr(label, sizeof label, "LAST QUESTION");
    }
    textAt(label, x1 + 7, y3 + 5, C_MUTED, C_PANEL, 1);
  }
  char lines[2][90];
  const char *body = problem ? st.detail : st.lastq[0] ? st.lastq : "None yet.";
  int n = wrapText(body, W - 26, 2, lines, 2);
  for (int i = 0; i < n; i++) textAt(lines[i], x1 + 7, y3 + 17 + i * 16, problem ? C_TEXT : st.lastq[0] ? C_TEXT : C_DIM, C_PANEL, 2);
}

// ---------- waiting screen ----------

static void drawWaiting() {
  tft.fillScreen(C_BG);
  textAt("LANTERN DECK", W / 2, 30, C_ACCENT, C_BG, 4, TC_DATUM);
  textAt("Waiting for Lantern...", W / 2, 70, C_TEXT, C_BG, 2, TC_DATUM);
  textAt("1. Plug me into the Lantern PC by USB.", 18, 108, C_MUTED, C_BG, 2);
  textAt("2. In Lantern, turn Relay on.", 18, 130, C_MUTED, C_BG, 2);
  textAt("3. Close any Meshtastic app or web", 18, 152, C_MUTED, C_BG, 2);
  textAt("    flasher that has my port open.", 18, 170, C_MUTED, C_BG, 2);
  char v[32];
  snprintf(v, sizeof v, "firmware %s  |  no WiFi", DECK_VERSION);
  textAt(v, W / 2, 218, C_GREY, C_BG, 1, TC_DATUM);
}

// ---------- main loop ----------

static bool showingWaiting = false;

static void draw() {
  if (!linked) {
    if (!showingWaiting || forceDraw) drawWaiting();
    showingWaiting = true;
    forceDraw = false;
    return;
  }
  bool full = forceDraw || showingWaiting;
  if (full) {
    tft.fillScreen(C_BG);
    drawTabs();
    drawnBody = drawnButtons = 0;
  }
  showingWaiting = false;
  drawHeader(full);
  uint32_t h;
  switch (tab) {
    case TAB_MSG:
      h = msgHash() ^ 0x1;
      if (full || h != drawnBody) {
        drawMessages();
        drawnBody = h;
      }
      h = buttonHash();
      if (full || h != drawnButtons) {
        drawButtons();
        drawnButtons = h;
      }
      break;
    case TAB_NODES:
      h = nodeHash() ^ 0x2;
      if (full || h != drawnBody) {
        drawNodes();
        drawnBody = h;
      }
      break;
    case TAB_RELAY:
      h = relayHash() ^ 0x3;
      if (full || h != drawnBody) {
        drawRelay();
        drawnBody = h;
      }
      break;
  }
  forceDraw = false;
}

static bool wasTouched = false;
static uint32_t lastTouchMs = 0;

static void onTap(int x, int y) {
  if (!linked) return;
  if (y >= TAB_Y) {
    Tab t = (Tab)min(2, x / (W / 3));
    if (t != tab) {
      tab = t;
      for (int i = 0; i < 4; i++)
        if (btnMode[i] == B_ARMED) btnMode[i] = B_IDLE;
      forceDraw = true;
    }
    return;
  }
  if (tab == TAB_MSG && y >= BTN_Y - 4 && y <= BTN_Y + BTN_H + 4 && st.nReplies > 0) {
    int gap = 4, w = (W - 8 - gap * (st.nReplies - 1)) / st.nReplies;
    int b = (x - 4) / (w + gap);
    if (b < 0 || b >= st.nReplies) return;
    if (btnMode[b] == B_ARMED) {
      if (strcmp(st.relay, "on")) {
        btnMode[b] = B_FAILED;
        btnSince[b] = millis();
        showToast("Relay isn't on, so nothing was sent.");
      } else {
        sendReply(b);
      }
    } else if (btnMode[b] == B_IDLE || btnMode[b] == B_SENT || btnMode[b] == B_FAILED) {
      for (int i = 0; i < 4; i++)
        if (btnMode[i] == B_ARMED) btnMode[i] = B_IDLE;
      btnMode[b] = B_ARMED;  // a second tap within 3 seconds sends it
      btnSince[b] = millis();
    }
  }
}

static void readTouch() {
  bool down = touch.touched();
  if (down && !wasTouched && millis() - lastTouchMs > 180) {
    TS_Point p = touch.getPoint();
    int x = map(p.x, 200, 3700, 0, W);
    int y = map(p.y, 240, 3800, 0, H);
    x = constrain(x, 0, W - 1);
    y = constrain(y, 0, H - 1);
    lastTouchMs = millis();
    onTap(x, y);
  }
  wasTouched = down;
}

static void tickButtons() {
  uint32_t now = millis();
  for (int i = 0; i < 4; i++) {
    uint32_t age = now - btnSince[i];
    if ((btnMode[i] == B_ARMED && age > 3000) || ((btnMode[i] == B_SENT || btnMode[i] == B_FAILED) && age > 2000) ||
        (btnMode[i] == B_SENDING && age > 8000)) {
      if (btnMode[i] == B_SENDING) {
        showToast("No answer from Lantern.");
        pendingId = pendingBtn = -1;
      }
      btnMode[i] = B_IDLE;
    }
  }
  if (toastUntil && now > toastUntil) toastUntil = 0;  // the message screen redraws without it
}

void setup() {
  Serial.setRxBufferSize(8192);
  Serial.begin(115200);
  pinMode(TFT_BL, OUTPUT);
  digitalWrite(TFT_BL, HIGH);
  tft.init();
  tft.setRotation(1);
#ifdef DECK_INVERT
  tft.invertDisplay(true);
#endif
  initColors();
  touchSpi.begin(TOUCH_CLK, TOUCH_MISO, TOUCH_MOSI, TOUCH_CS);
  touch.begin(touchSpi);
  touch.setRotation(1);
  drawWaiting();
  showingWaiting = true;
  sendHello();
}

void loop() {
  readSerial();
  uint32_t now = millis();
  if (now - lastHelloMs > (linked ? 5000u : 2000u)) sendHello();
  if (linked && now - lastStateMs > 12000) {
    linked = false;  // Lantern closed, Relay turned off, or unplugged
    forceDraw = true;
  }
  readTouch();
  tickButtons();
  draw();
  delay(10);
}
