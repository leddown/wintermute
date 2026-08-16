// Owns the dark/matrix/chaos theme choice, brought across from morpheus. The
// initial theme is applied by /theme-init.js (before first paint, to avoid a
// flash); this module handles switching and persisting the choice from then
// on. Chaos renders as Matrix, so it also drives the WintermuteChaos glitch
// timer.
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
  const THEMES = ["dark", "matrix", "chaos"];
  const LABELS = {
    dark: "Dark",
    matrix: "Matrix",
    chaos: "Chaos",
  };

  function apply(theme) {
    document.documentElement.dataset.theme = theme;
    if (theme === "chaos") {
      WintermuteChaos.start();
    } else {
      WintermuteChaos.stop();
    }
    // Chaos is Matrix plus glitches, so it gets the backdrop too. Every other
    // theme stops it outright rather than hiding it, so a canvas isn't being
    // repainted behind a page that never shows it.
    if (theme === "matrix" || theme === "chaos") {
      WintermuteRain.start();
    } else {
      WintermuteRain.stop();
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
