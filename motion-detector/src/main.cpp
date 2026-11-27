// Motion Detector: a people radar on the ESP32 "Cheap Yellow Display" (ESP32-2432S028R)
// with the Hi-Link HLK-LD2450 24 GHz mmWave sensor.
//
// The LD2450 reports up to 3 moving people (X/Y in mm, speed in cm/s) about 10
// times a second at 256000 baud. They're drawn on a motion-tracker style fan:
// +/-60 degrees, range rings, a sweep and fading blips with trails.
//
// Wiring: sensor 5V and GND to the CYD's P1 connector (VIN, GND); sensor TX to
// IO27 and sensor RX to IO22 on the CN1 connector.
//
// With no sensor it runs a demo (made-up people) so the screen can be tried out;
// it switches to the sensor as soon as frames arrive. No WiFi or Bluetooth.
//
// Touch: top-left = sound on/off, top-right = mirror left/right,
//        bottom-left = demo on/off, bottom-right = range 6/4/3 m. Settings are kept.

#include <Arduino.h>
#include <Preferences.h>
#include <SPI.h>
#include <TFT_eSPI.h>
#include <XPT2046_Touchscreen.h>
#include <math.h>

#ifndef MD_VERSION
#define MD_VERSION "dev"
#endif

// ---------- hardware ----------

#define RADAR_RX 27  // CYD CN1: from the sensor's TX
#define RADAR_TX 22  // CYD CN1: to the sensor's RX
#define RADAR_BAUD 256000

#define TOUCH_CS 33
#define TOUCH_IRQ 36
#define TOUCH_MOSI 32
#define TOUCH_MISO 39
#define TOUCH_CLK 25

#define LED_R 4  // RGB LED, active low
#define LED_G 16
#define LED_B 17
#define SPEAKER 26

TFT_eSPI tft;
TFT_eSprite fb(&tft);  // whole-screen 4-bit frame buffer, so nothing flickers
SPIClass touchSpi(VSPI);
XPT2046_Touchscreen touch(TOUCH_CS, TOUCH_IRQ);
Preferences prefs;

static const int W = 320, H = 240;

// ---------- palette (4-bit sprite: 16 colours) ----------

enum {
  P_BG, P_FAN, P_GRID, P_DIM, P_MUTED, P_TEXT, P_GREEN, P_HOT,
  P_AMBER, P_RED, P_SWEEP1, P_SWEEP2, P_SWEEP3, P_HALO, P_TRAIL, P_BAR
};

static uint16_t c565(uint8_t r, uint8_t g, uint8_t b) { return ((r & 0xF8) << 8) | ((g & 0xFC) << 3) | (b >> 3); }
static uint16_t mix565(uint8_t r1, uint8_t g1, uint8_t b1, uint8_t r2, uint8_t g2, uint8_t b2, float t) {
  return c565(r1 + (r2 - r1) * t, g1 + (g2 - g1) * t, b1 + (b2 - b1) * t);
}

static uint16_t palette[16];

static void initPalette() {
  palette[P_BG] = c565(4, 8, 5);
  palette[P_FAN] = c565(7, 16, 10);
  palette[P_GRID] = c565(24, 52, 32);
  palette[P_DIM] = c565(46, 92, 58);
  palette[P_MUTED] = c565(143, 191, 153);
  palette[P_TEXT] = c565(201, 245, 211);
  palette[P_GREEN] = c565(124, 252, 154);
  palette[P_HOT] = c565(230, 255, 235);
  palette[P_AMBER] = c565(255, 181, 71);
  palette[P_RED] = c565(255, 99, 88);
  palette[P_SWEEP1] = mix565(124, 252, 154, 7, 16, 10, 0.35f);
  palette[P_SWEEP2] = mix565(124, 252, 154, 7, 16, 10, 0.60f);
  palette[P_SWEEP3] = mix565(124, 252, 154, 7, 16, 10, 0.82f);
  palette[P_HALO] = mix565(124, 252, 154, 7, 16, 10, 0.72f);
  palette[P_TRAIL] = mix565(124, 252, 154, 7, 16, 10, 0.50f);
  palette[P_BAR] = c565(8, 15, 10);
}

// ---------- settings ----------

static float rangeM = 6.0f;
static bool mirrored = false;
static bool soundOn = false;
static bool demoForced = false;

static void loadSettings() {
  prefs.begin("motion", false);
  rangeM = prefs.getFloat("range", 6.0f);
  if (rangeM != 6.0f && rangeM != 4.0f && rangeM != 3.0f) rangeM = 6.0f;
  mirrored = prefs.getBool("mirror", false);
  soundOn = prefs.getBool("sound", false);
}

static void saveSettings() {
  prefs.putFloat("range", rangeM);
  prefs.putBool("mirror", mirrored);
  prefs.putBool("sound", soundOn);
}

// ---------- LD2450 frames ----------

struct Target {
  bool on;
  float x, y;   // metres; x to the right, y straight out
  float speed;  // m/s
};

static Target sensorTargets[3];
static uint32_t lastFrameMs = 0;
static uint32_t frameCount = 0;

// Each scan (one LD2450 frame, about 10 a second) covers the whole field of view at once.
// They're drawn as ripples spreading out over the fan, one per scan.
#define SCAN_RINGS 8
static uint32_t scanTimes[SCAN_RINGS];
static int scanHead = 0;
static float scanHz = 0;
static uint32_t hzStart = 0, hzCount = 0;

static void noteScan() {
  uint32_t now = millis();
  scanTimes[scanHead] = now;
  scanHead = (scanHead + 1) % SCAN_RINGS;
  hzCount++;
  if (now - hzStart >= 2000) {
    scanHz = hzCount * 1000.0f / (now - hzStart);
    hzStart = now;
    hzCount = 0;
  }
}

static int16_t ldSigned(uint16_t v) { return (v & 0x8000) ? (int16_t)(v & 0x7FFF) : -(int16_t)(v & 0x7FFF); }

static uint8_t frameBuf[30];
static int frameLen = 0;
static const uint8_t HEAD[4] = {0xAA, 0xFF, 0x03, 0x00};

static void decodeFrame() {
  for (int s = 0; s < 3; s++) {
    const uint8_t *p = frameBuf + 4 + s * 8;
    bool empty = true;
    for (int i = 0; i < 8; i++)
      if (p[i]) empty = false;
    Target &t = sensorTargets[s];
    t.on = !empty;
    if (empty) continue;
    t.x = ldSigned(p[0] | (p[1] << 8)) / 1000.0f;
    t.y = ldSigned(p[2] | (p[3] << 8)) / 1000.0f;
    t.speed = ldSigned(p[4] | (p[5] << 8)) / 100.0f;
  }
  lastFrameMs = millis();
  frameCount++;
  noteScan();
}

// Reads bytes and decodes each complete frame: AA FF 03 00, 3 x 8 bytes, 55 CC.
static void readRadar() {
  while (Serial2.available()) {
    uint8_t b = Serial2.read();
    if (frameLen < 4) {
      if (b == HEAD[frameLen]) {
        frameBuf[frameLen++] = b;
      } else {
        frameLen = (b == HEAD[0]) ? 1 : 0;
        if (frameLen) frameBuf[0] = b;
      }
      continue;
    }
    frameBuf[frameLen++] = b;
    if (frameLen == 30) {
      if (frameBuf[28] == 0x55 && frameBuf[29] == 0xCC) decodeFrame();
      frameLen = 0;
    }
  }
}

static bool sensorOk() { return frameCount > 0 && millis() - lastFrameMs < 1500; }

// ---------- demo people ----------

struct Walker {
  bool on;
  float x, y, heading, speed, pause;
};
static Walker walkers[3];
static uint32_t nextDemoChange = 0;

static float frand(float a, float b) { return a + (b - a) * (esp_random() / 4294967295.0f); }

static void demoStep(float dt, Target out[3]) {
  uint32_t now = millis();
  if (now > nextDemoChange) {
    nextDemoChange = now + (uint32_t)frand(4000, 9000);
    int count = 0, freeSlot = -1;
    for (int i = 0; i < 3; i++) {
      if (walkers[i].on) count++;
      else if (freeSlot < 0) freeSlot = i;
    }
    if (freeSlot >= 0 && (count < 2 || frand(0, 1) < 0.4f)) {
      Walker &w = walkers[freeSlot];
      float r = frand(1.0f, rangeM * 0.9f), a = frand(-50, 50) * DEG_TO_RAD;
      w = {true, r * sinf(a), r * cosf(a), frand(0, TWO_PI), frand(0.3f, 1.2f), 0};
    } else if (count > 0 && frand(0, 1) < 0.5f) {
      int i = (int)frand(0, 2.99f);
      walkers[i].on = false;
    }
  }
  for (int i = 0; i < 3; i++) {
    Walker &w = walkers[i];
    out[i].on = w.on;
    if (!w.on) continue;
    float v = 0;
    if (w.pause > 0) {
      w.pause -= dt;
    } else {
      w.heading += frand(-0.8f, 0.8f) * dt;
      w.x += sinf(w.heading) * w.speed * dt;
      w.y += cosf(w.heading) * w.speed * dt;
      float r = hypotf(w.x, w.y), ang = atan2f(w.x, w.y) * RAD_TO_DEG;
      if (r > rangeM * 0.95f || r < 0.6f || fabsf(ang) > 55) w.heading = atan2f(-w.x, rangeM * 0.5f - w.y);
      if (frand(0, 1) < 0.003f) w.pause = frand(1, 3);
      v = w.speed;
    }
    out[i] = {true, w.x, w.y, v};
  }
}

// ---------- display ----------

static const int BAR_H = 22, FOOT_Y = 226;
static const int FAN = 60;  // degrees each side
static int cx = W / 2, cy = 222, radius = 175;

static void polar(float rPx, float deg, int &x, int &y) {
  float a = deg * DEG_TO_RAD;
  x = cx + (int)lroundf(rPx * sinf(a));
  y = cy - (int)lroundf(rPx * cosf(a));
}

static void toScreen(float xm, float ym, int &x, int &y) {
  if (mirrored) xm = -xm;
  float s = radius / rangeM;
  x = cx + (int)lroundf(xm * s);
  y = cy - (int)lroundf(ym * s);
}

static void drawArcLine(int rPx, uint8_t col) {
  int px, py;
  polar(rPx, -FAN, px, py);
  for (int d = -FAN + 3; d <= FAN; d += 3) {
    int x, y;
    polar(rPx, d, x, y);
    fb.drawLine(px, py, x, y, col);
    px = x;
    py = y;
  }
}

static void drawGrid() {
  // filled fan
  int px, py;
  polar(radius, -FAN, px, py);
  for (int d = -FAN + 3; d <= FAN; d += 3) {
    int x, y;
    polar(radius, d, x, y);
    fb.fillTriangle(cx, cy, px, py, x, y, P_FAN);
    px = x;
    py = y;
  }
  int rings = (int)rangeM;
  for (int m = 1; m <= rings; m++) {
    int r = (int)lroundf(radius * m / rangeM);
    drawArcLine(r, m == rings ? P_DIM : P_GRID);
  }
  for (int d = -FAN; d <= FAN; d += 30) {
    int x, y;
    polar(radius, d, x, y);
    fb.drawLine(cx, cy, x, y, abs(d) == FAN ? P_DIM : P_GRID);
  }
  // ring labels just right of the centre line
  fb.setTextDatum(ML_DATUM);
  fb.setTextColor(P_MUTED, P_FAN);
  for (int m = 1; m <= rings; m++) {
    int r = (int)lroundf(radius * m / rangeM);
    char b[6];
    snprintf(b, sizeof b, "%dm", m);
    fb.drawString(b, cx + 4, cy - r + 6, 1);
  }
  fb.fillCircle(cx, cy, 3, P_GREEN);
}

// scan ripples: one per frame, spreading over the whole fan (the sensor sees all of it at once)
static const uint32_t RIPPLE_MS = 450;

static void drawScan() {
  uint32_t now = millis();
  for (int i = 0; i < SCAN_RINGS; i++) {
    uint32_t t = scanTimes[i];
    if (!t) continue;
    uint32_t age = now - t;
    if (age >= RIPPLE_MS) continue;
    int r = (int)(radius * (float)age / RIPPLE_MS);
    if (r < 4) continue;
    uint8_t col = age < RIPPLE_MS / 3 ? P_SWEEP1 : age < RIPPLE_MS * 2 / 3 ? P_SWEEP2 : P_SWEEP3;
    drawArcLine(r, col);
  }
  // the fan's edges light up briefly on each scan: everything inside is measured together
  uint32_t last = scanTimes[(scanHead + SCAN_RINGS - 1) % SCAN_RINGS];
  if (last && now - last < 90) {
    int x, y;
    polar(radius, -FAN, x, y);
    fb.drawLine(cx, cy, x, y, P_SWEEP1);
    polar(radius, FAN, x, y);
    fb.drawLine(cx, cy, x, y, P_SWEEP1);
    drawArcLine(radius, P_SWEEP1);
  }
}

// tracked contacts: smoothing and trails
#define TRAIL 24
struct Track {
  bool live;          // in the latest frame
  uint32_t lastSeen;  // ms
  float sx, sy;       // smoothed position
  float tx[TRAIL], ty[TRAIL];
  uint32_t tt[TRAIL];
  int n, head;
};
static Track tracks[3];
static uint32_t lastTrailMs = 0;
static uint32_t flashUntil = 0;

// ping: two quick falling notes on the CYD's speaker connector
static uint32_t pingStep = 0, pingAt = 0;
static void startPing() {
  if (!soundOn) return;
  pingStep = 1;
  pingAt = millis();
  ledcWriteTone(0, 1500);
}
static void tickPing() {
  if (!pingStep) return;
  uint32_t t = millis() - pingAt;
  if (pingStep == 1 && t > 70) {
    ledcWriteTone(0, 1100);
    pingStep = 2;
  } else if (pingStep == 2 && t > 180) {
    ledcWriteTone(0, 0);
    pingStep = 0;
  }
}

static void setLed(bool r, bool g, bool b) {
  digitalWrite(LED_R, r ? LOW : HIGH);
  digitalWrite(LED_G, g ? LOW : HIGH);
  digitalWrite(LED_B, b ? LOW : HIGH);
}

static void updateTracks(const Target t[3]) {
  uint32_t now = millis();
  bool addTrail = now - lastTrailMs > 150;
  if (addTrail) lastTrailMs = now;
  for (int i = 0; i < 3; i++) {
    Track &k = tracks[i];
    bool on = t[i].on && hypotf(t[i].x, t[i].y) <= rangeM * 1.05f;
    if (on) {
      if (now - k.lastSeen > 3000) {  // a new contact
        k.n = k.head = 0;
        k.sx = t[i].x;
        k.sy = t[i].y;
        flashUntil = now + 600;
        startPing();
      }
      k.sx += (t[i].x - k.sx) * 0.45f;
      k.sy += (t[i].y - k.sy) * 0.45f;
      k.lastSeen = now;
      if (addTrail) {
        k.tx[k.head] = k.sx;
        k.ty[k.head] = k.sy;
        k.tt[k.head] = now;
        k.head = (k.head + 1) % TRAIL;
        if (k.n < TRAIL) k.n++;
      }
    }
    k.live = on;
  }
}

static void drawContacts(int &count, float &nearest) {
  uint32_t now = millis();
  count = 0;
  nearest = 99;
  // trails first
  for (int i = 0; i < 3; i++) {
    Track &k = tracks[i];
    if (now - k.lastSeen > 4000) continue;
    for (int j = 0; j < k.n; j++) {
      int idx = (k.head - 1 - j + TRAIL * 2) % TRAIL;
      uint32_t age = now - k.tt[idx];
      if (age > 3500 || j == 0) continue;
      int x, y;
      toScreen(k.tx[idx], k.ty[idx], x, y);
      fb.fillCircle(x, y, 2, age < 1200 ? P_TRAIL : age < 2400 ? P_SWEEP2 : P_SWEEP3);
    }
  }
  for (int i = 0; i < 3; i++) {
    Track &k = tracks[i];
    uint32_t age = now - k.lastSeen;
    if (age > 1500) continue;  // gone: fade out
    int x, y;
    toScreen(k.sx, k.sy, x, y);
    bool faded = !k.live && age > 500;
    float pulse = 0.5f + 0.5f * sinf(now / 160.0f);
    fb.fillCircle(x, y, 9 + (int)(3 * pulse), faded ? P_SWEEP3 : P_HALO);
    fb.fillCircle(x, y, 6, faded ? P_SWEEP2 : P_GREEN);
    fb.fillCircle(x, y, 2, faded ? P_SWEEP1 : P_HOT);
    float r = hypotf(k.sx, k.sy);
    char b[10];
    snprintf(b, sizeof b, "%.1fm", r);
    fb.setTextDatum(ML_DATUM);
    fb.setTextColor(faded ? P_DIM : P_TEXT);
    int lx = x + 15;
    if (lx + 34 > W) lx = x - 15 - fb.textWidth(b, 1);
    fb.drawString(b, lx, y, 1);
    if (k.live) {
      count++;
      if (r < nearest) nearest = r;
    }
  }
}

static char toast[32] = "";
static uint32_t toastUntil = 0;
static void showToast(const char *t) {
  strncpy(toast, t, sizeof toast - 1);
  toastUntil = millis() + 1500;
}

static void drawBars(int count, float nearest, bool demo) {
  // top
  fb.fillRect(0, 0, W, BAR_H, P_BAR);
  fb.drawFastHLine(0, BAR_H, W, P_GRID);
  fb.setTextDatum(ML_DATUM);
  fb.setTextColor(P_GREEN, P_BAR);
  fb.drawString("MOTION", 6, BAR_H / 2 + 1, 2);
  char b[32];
  snprintf(b, sizeof b, "%d", count);
  fb.setTextColor(count ? P_AMBER : P_MUTED, P_BAR);
  fb.setTextDatum(MR_DATUM);
  fb.drawString(b, 150, BAR_H / 2 + 1, 2);
  fb.setTextDatum(ML_DATUM);
  fb.setTextColor(P_MUTED, P_BAR);
  fb.drawString(count == 1 ? "CONTACT" : "CONTACTS", 155, BAR_H / 2 + 1, 1);
  fb.setTextDatum(MR_DATUM);
  if (count) {
    snprintf(b, sizeof b, "%.1fm %dft", nearest, (int)lroundf(nearest * 3.281f));
    fb.setTextColor(nearest < 1.5f ? P_RED : P_TEXT, P_BAR);
  } else {
    strcpy(b, "CLEAR");
    fb.setTextColor(P_MUTED, P_BAR);
  }
  fb.drawString(b, W - 6, BAR_H / 2 + 1, 2);

  // bottom corners (also the touch zones)
  fb.setTextDatum(BL_DATUM);
  if (demo) {
    fb.setTextColor(P_AMBER, P_BG);
    fb.drawString(demoForced ? "DEMO" : "DEMO-NO SENSOR", 4, H - 2, 1);
  } else if (sensorOk()) {
    fb.setTextColor(P_GREEN, P_BG);
    char ok[24];
    snprintf(ok, sizeof ok, "SENSOR OK %.0fHz", scanHz);
    fb.drawString(ok, 4, H - 2, 1);
  } else {
    fb.setTextColor(P_RED, P_BG);
    fb.drawString("SENSOR LOST", 4, H - 2, 1);
  }
  fb.setTextDatum(BR_DATUM);
  fb.setTextColor(P_MUTED, P_BG);
  snprintf(b, sizeof b, "%s%s %dm", soundOn ? "SND " : "", mirrored ? "MIR" : "", (int)rangeM);
  fb.drawString(b, W - 4, H - 2, 1);

  if (millis() < toastUntil) {
    fb.setTextDatum(MC_DATUM);
    int tw = fb.textWidth(toast, 2) + 16;
    fb.fillRoundRect(W / 2 - tw / 2, 40, tw, 22, 4, P_AMBER);
    fb.setTextColor(P_BG, P_AMBER);
    fb.drawString(toast, W / 2, 51, 2);
  }
  if (millis() < flashUntil) {
    fb.drawRect(0, 0, W, H, P_AMBER);
    fb.drawRect(1, 1, W - 2, H - 2, P_AMBER);
    fb.drawRect(2, 2, W - 4, H - 4, P_AMBER);
  }
}

// ---------- touch ----------

static bool wasTouched = false;
static uint32_t lastTouchMs = 0;

static void onTap(int x, int y) {
  if (y < 50 && x < 110) {
    soundOn = !soundOn;
    showToast(soundOn ? "SOUND ON" : "SOUND OFF");
    if (soundOn) startPing();
  } else if (y < 50 && x > W - 110) {
    mirrored = !mirrored;
    showToast(mirrored ? "MIRRORED" : "NOT MIRRORED");
  } else if (y > H - 50 && x < 110) {
    demoForced = !demoForced;
    showToast(demoForced ? "DEMO ON" : "DEMO OFF");
  } else if (y > H - 50 && x > W - 110) {
    rangeM = rangeM == 6.0f ? 4.0f : rangeM == 4.0f ? 3.0f : 6.0f;
    char b[16];
    snprintf(b, sizeof b, "RANGE %dm", (int)rangeM);
    showToast(b);
    for (auto &w : walkers) w.on = false;
  } else {
    return;
  }
  saveSettings();
}

static void readTouch() {
  bool down = touch.touched();
  if (down && !wasTouched && millis() - lastTouchMs > 200) {
    TS_Point p = touch.getPoint();
    int x = constrain(map(p.x, 200, 3700, 0, W), 0, W - 1);
    int y = constrain(map(p.y, 240, 3800, 0, H), 0, H - 1);
    lastTouchMs = millis();
    onTap(x, y);
  }
  wasTouched = down;
}

// ---------- main ----------

static uint32_t startMs, lastLoopMs;

void setup() {
  Serial.begin(115200);
  Serial2.setRxBufferSize(1024);
  Serial2.begin(RADAR_BAUD, SERIAL_8N1, RADAR_RX, RADAR_TX);
  pinMode(LED_R, OUTPUT);
  pinMode(LED_G, OUTPUT);
  pinMode(LED_B, OUTPUT);
  setLed(false, false, false);
  ledcSetup(0, 2000, 8);
  ledcAttachPin(SPEAKER, 0);
  ledcWriteTone(0, 0);

  loadSettings();
  tft.init();
  tft.setRotation(1);
#ifdef MD_INVERT
  tft.invertDisplay(true);
#endif
  tft.fillScreen(TFT_BLACK);
  touchSpi.begin(TOUCH_CLK, TOUCH_MISO, TOUCH_MOSI, TOUCH_CS);
  touch.begin(touchSpi);
  touch.setRotation(1);

  initPalette();
  fb.setColorDepth(4);
  if (!fb.createSprite(W, H)) {
    tft.setTextColor(TFT_RED, TFT_BLACK);
    tft.drawString("Not enough memory for the screen", 10, 110, 2);
    while (true) delay(1000);
  }
  fb.createPalette(palette, 16);
  startMs = lastLoopMs = millis();
  Serial.printf("Motion Detector %s: radar on RX %d / TX %d at %d baud\n", MD_VERSION, RADAR_RX, RADAR_TX, RADAR_BAUD);
}

void loop() {
  readRadar();
  readTouch();
  tickPing();

  uint32_t now = millis();
  float dt = (now - lastLoopMs) / 1000.0f;
  if (dt < 0.04f) {  // about 25 frames a second
    delay(2);
    return;
  }
  if (dt > 0.2f) dt = 0.2f;
  lastLoopMs = now;

  bool demo = demoForced || (frameCount == 0 && now - startMs > 3000);
  Target t[3];
  if (demo) {
    // the demo "sensor" reports 10 times a second, like the real one
    static Target demoT[3];
    static uint32_t lastDemoScan = 0;
    if (now - lastDemoScan >= 100) {
      demoStep(lastDemoScan ? (now - lastDemoScan) / 1000.0f : 0.1f, demoT);
      lastDemoScan = now;
      noteScan();
    }
    memcpy(t, demoT, sizeof t);
  } else {
    memcpy(t, sensorTargets, sizeof t);
    if (!sensorOk())
      for (auto &x : t) x.on = false;
  }
  updateTracks(t);

  fb.fillSprite(P_BG);
  drawGrid();
  drawScan();
  int count;
  float nearest;
  drawContacts(count, nearest);
  drawBars(count, nearest, demo);
  fb.pushSprite(0, 0);

  // LED: red when someone is close, amber-ish while anyone is in view, off when clear
  if (count && nearest < 1.5f) setLed(true, false, false);
  else if (count) setLed(true, true, false);
  else setLed(false, false, false);
}
