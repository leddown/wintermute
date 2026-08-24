// Owns the theme choice, brought across from morpheus. The initial theme is
// applied by /theme-init.js (before first paint, to avoid a flash); this
// module handles switching and persisting the choice from then on.
//
// Each theme is a palette plus at most one background engine:
//   dark    — palette only
//   matrix  — palette + the falling-glyph rain
//   chaos   — palette + the colour-glitch timer, and no rain: it is its own
//             effect rather than the rain with something on top
//   40k     — palette + the CRT fritz overlay
//
// The picker lives in Admin → Appearance, which app.js renders on demand, so
// this module owns no DOM of its own: it exposes the vocabulary (themes,
// labels, current, set) and lets the pane draw whatever control it likes. That
// is also why apply() must work with nothing on screen listening — it runs at
// load, long before anyone opens Admin.
//
// The theme is a per-browser choice in localStorage, not server state: it is a
// property of the screen you are looking at, and the same install is read from
// a phone and a 32:9 monitor.
const WintermuteTheme = (() => {
  const STORAGE_KEY = "wintermute-theme";
  const LIFT_KEY = "wintermute-text-lift";
  const THEMES = ["dark", "matrix", "chaos", "40k"];
  const LABELS = {
    dark: "Dark",
    matrix: "Matrix",
    chaos: "Chaos",
    "40k": "40K",
  };

  // How far the text is lifted towards white, as a percentage of the way
  // there. Every palette here is dark and tuned on one screen; the same
  // colours on a phone in daylight, or on a panel with the contrast wound
  // down, can be genuinely hard to read. This does not touch the backgrounds
  // or the accents — only the two variables the text is drawn in — so the
  // theme survives being made legible.
  //
  // 100 is the palette exactly as designed rather than 0, because the pane
  // already speaks that way about the rain and one vocabulary is enough.
  const MIN_BRIGHTNESS = 100;
  const MAX_BRIGHTNESS = 175;
  const DEFAULT_BRIGHTNESS = 100;

  function clampBrightness(value) {
    const n = parseInt(value, 10);
    if (!Number.isFinite(n)) return DEFAULT_BRIGHTNESS;
    return Math.min(MAX_BRIGHTNESS, Math.max(MIN_BRIGHTNESS, n));
  }

  function brightness() {
    return clampBrightness(localStorage.getItem(LIFT_KEY));
  }

  function applyBrightness(value) {
    // The stylesheet mixes towards white by this much, so the percentage the
    // pane shows is one more than the fraction actually mixed in.
    document.documentElement.style.setProperty(
      "--text-lift", `${clampBrightness(value) - 100}%`);
  }

  function setBrightness(value) {
    const n = clampBrightness(value);
    localStorage.setItem(LIFT_KEY, String(n));
    applyBrightness(n);
    return n;
  }

  function apply(theme) {
    document.documentElement.dataset.theme = theme;

    // Each engine is stopped outright rather than hidden when its theme is not
    // on, so nothing is being animated or repainted behind a page that never
    // shows it.
    if (theme === "chaos") {
      WintermuteChaos.start();
    } else {
      WintermuteChaos.stop();
    }
    if (theme === "matrix") {
      WintermuteRain.start();
    } else {
      WintermuteRain.stop();
    }
    if (theme === "40k") {
      WintermuteFritz.start();
    } else {
      WintermuteFritz.stop();
    }
  }

  function current() {
    const theme = document.documentElement.dataset.theme;
    return THEMES.includes(theme) ? theme : "dark";
  }

  function set(theme) {
    if (!THEMES.includes(theme)) return;
    localStorage.setItem(STORAGE_KEY, theme);
    apply(theme);
  }

  function toggle() {
    set(THEMES[(THEMES.indexOf(current()) + 1) % THEMES.length]);
  }

  apply(current());
  // Applied again here even though /theme-init.js already did it before first
  // paint: that file runs from <head> and this module has to work if it ever
  // does not, the same way apply() runs with nothing on screen listening.
  applyBrightness(brightness());

  return {
    toggle, set, current, THEMES, LABELS,
    brightness, setBrightness, applyBrightness,
    MIN_BRIGHTNESS, MAX_BRIGHTNESS, DEFAULT_BRIGHTNESS,
  };
})();
