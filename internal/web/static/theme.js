// Owns the dark/matrix/chaos theme cycle, brought across from morpheus. The
// initial theme is applied by /theme-init.js (before first paint, to avoid a
// flash); this module just syncs the toggle button label and handles switching
// + persisting the choice from then on. Chaos renders as Matrix, so it also
// drives the WintermuteChaos glitch timer.
//
// The theme is a per-browser choice in localStorage, not server state: it is a
// property of the screen you are looking at, and the same install is read from
// a phone and a 32:9 monitor.
const WintermuteTheme = (() => {
  const STORAGE_KEY = "wintermute-theme";
  const THEMES = ["dark", "matrix", "chaos"];
  const LABELS = {
    dark: "Theme: Dark",
    matrix: "Theme: Matrix",
    chaos: "Theme: Chaos",
  };
  const button = document.getElementById("theme-toggle-btn");

  function apply(theme) {
    document.documentElement.dataset.theme = theme;
    button.textContent = LABELS[theme] || LABELS.dark;
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

  function toggle() {
    const current = document.documentElement.dataset.theme;
    const next = THEMES[(THEMES.indexOf(current) + 1) % THEMES.length];
    localStorage.setItem(STORAGE_KEY, next);
    apply(next);
  }

  apply(document.documentElement.dataset.theme || "dark");
  button.addEventListener("click", toggle);

  return { toggle };
})();
