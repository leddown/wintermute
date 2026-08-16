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
  const THEMES = ["dark", "matrix", "chaos", "40k"];
  const LABELS = {
    dark: "Dark",
    matrix: "Matrix",
    chaos: "Chaos",
    "40k": "40K",
  };

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

  return { toggle, set, current, THEMES, LABELS };
})();
