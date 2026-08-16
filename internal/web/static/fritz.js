// CRT "fritz" engine for the 40K theme: the picture of a heavy, half-broken
// display that has been left running far too long.
//
// It is three things layered, in order of how often you notice them:
//
//   1. Scanlines and a vignette, always on. Static, costs nothing, and is what
//      makes the palette read as a screen rather than a colour scheme.
//   2. A roll bar drifting slowly down the glass, the way an unsynced picture
//      does. One CSS animation.
//   3. Bursts: every few seconds the picture fritzes — brief tear bands at
//      random heights with a colour fringe, and a flicker across the whole
//      panel. This is the part that has to be irregular, so it is driven from
//      a timer rather than a keyframe loop.
//
// Everything lives in one overlay with `pointer-events: none`, above the app
// but below the toast and the dialog. Nothing here touches the app's own DOM:
// an earlier attempt displaced #app itself, which works visually but makes
// #app a containing block, and the system gauges are `position: fixed` inside
// it — they would have been dragged along by every flicker. The overlay is
// self-contained precisely so no effect can reach the layout.
//
// Both knobs are per-browser localStorage, edited from Admin → Appearance,
// exactly like the rain brightness and the chaos weights.
const WintermuteFritz = (() => {
  const INTERVAL_KEY = "wintermute-fritz-interval";
  const INTENSITY_KEY = "wintermute-fritz-intensity";

  // Seconds between bursts. The actual wait is randomised around this so the
  // fault never falls into a rhythm — a predictable glitch stops reading as a
  // fault and starts reading as a decoration.
  const DEFAULT_INTERVAL = 7;
  const MIN_INTERVAL = 1;
  const MAX_INTERVAL = 600;

  // Intensity scales how far the whole effect is pushed: 0 leaves the static
  // scanlines and nothing else, 100 is the weight the theme was drawn at, and
  // the ceiling is deliberately well past comfortable.
  const DEFAULT_INTENSITY = 100;
  const MIN_INTENSITY = 0;
  const MAX_INTENSITY = 300;

  let timer = null;
  let running = false;

  function clampInt(value, min, max, fallback) {
    const n = parseInt(value, 10);
    if (!Number.isFinite(n)) return fallback;
    return Math.min(max, Math.max(min, n));
  }

  function intervalSeconds() {
    return clampInt(localStorage.getItem(INTERVAL_KEY), MIN_INTERVAL, MAX_INTERVAL, DEFAULT_INTERVAL);
  }

  function intensity() {
    return clampInt(localStorage.getItem(INTENSITY_KEY), MIN_INTENSITY, MAX_INTENSITY, DEFAULT_INTENSITY);
  }

  function overlay() {
    return document.getElementById("fritz");
  }

  // Someone who has asked the system for less motion gets the scanlines and
  // the vignette — which do not move — and none of the rest.
  function reducedMotion() {
    return window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  }

  // A burst is a handful of tear bands plus a whole-panel flicker. The bands
  // are removed on a timer rather than by animationend: an interrupted or
  // never-fired animation would otherwise leave one on screen permanently.
  function burst() {
    const box = overlay();
    if (!box) return;
    const scale = intensity() / 100;
    if (scale <= 0) return;

    box.classList.add("fritz-flicker");
    setTimeout(() => box.classList.remove("fritz-flicker"), 90 + Math.random() * 120);

    const bands = Math.max(1, Math.round((1 + Math.random() * 3) * Math.min(scale, 2)));
    for (let i = 0; i < bands; i++) {
      const band = document.createElement("div");
      band.className = "fritz-band";
      band.style.top = `${Math.random() * 100}%`;
      band.style.height = `${2 + Math.random() * 26}px`;
      // The fringe is the giveaway that a picture has torn rather than simply
      // dimmed: the band carries a sliver of colour off to one side.
      band.style.setProperty("--shift", `${(Math.random() * 12 - 6) * scale}px`);
      band.style.opacity = String(Math.min(1, (0.25 + Math.random() * 0.5) * scale));
      box.append(band);
      setTimeout(() => band.remove(), 120 + Math.random() * 260);
    }
  }

  // schedule waits a randomised multiple of the configured interval, so the
  // gap between faults is never the same twice.
  function schedule() {
    const base = intervalSeconds() * 1000;
    const wait = base * (0.45 + Math.random() * 1.1);
    timer = setTimeout(() => {
      if (!running) return;
      // A backgrounded tab is not worth glitching for, but the loop keeps
      // going so it resumes the moment the tab comes back.
      if (!document.hidden && !reducedMotion()) burst();
      schedule();
    }, wait);
  }

  function start() {
    const box = overlay();
    if (box) {
      box.hidden = false;
      box.style.setProperty("--fritz-intensity", String(intensity() / 100));
    }
    if (running) return;
    running = true;
    schedule();
  }

  function stop() {
    running = false;
    if (timer) clearTimeout(timer);
    timer = null;
    const box = overlay();
    if (box) {
      box.hidden = true;
      box.classList.remove("fritz-flicker");
      for (const band of box.querySelectorAll(".fritz-band")) band.remove();
    }
  }

  function config() {
    return { intervalSeconds: intervalSeconds(), intensity: intensity() };
  }

  // setConfig persists both knobs and restarts the loop when 40K is the active
  // theme, so a save takes effect immediately rather than at the next burst.
  function setConfig(next) {
    localStorage.setItem(
      INTERVAL_KEY,
      String(clampInt(next.intervalSeconds, MIN_INTERVAL, MAX_INTERVAL, DEFAULT_INTERVAL)),
    );
    localStorage.setItem(
      INTENSITY_KEY,
      String(clampInt(next.intensity, MIN_INTENSITY, MAX_INTENSITY, DEFAULT_INTENSITY)),
    );
    if (document.documentElement.dataset.theme === "40k") {
      stop();
      start();
    }
  }

  return {
    start, stop, burst, config, setConfig,
    MIN_INTERVAL, MAX_INTERVAL, MIN_INTENSITY, MAX_INTENSITY,
  };
})();
