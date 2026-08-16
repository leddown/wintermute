// Chaos theme engine: the Matrix theme plus periodic colour glitches. On a
// timer it recolours a random sample of the characters currently on screen,
// clearing the previous sample first so each cycle is a fresh glitch. A view
// re-render throws the sample away with the rest of the DOM, so app.js also
// calls repaint() on every view change.
//
// Both knobs live in localStorage (the theme choice itself already does) and
// are edited from the Admin → Appearance tab:
//   - interval: seconds between glitches
//   - density:  characters recoloured per 300 characters on screen
const WintermuteChaos = (() => {
  const INTERVAL_KEY = "wintermute-chaos-interval";
  const DENSITY_KEY = "wintermute-chaos-density";

  // Density is expressed per this many characters, so the effect scales with
  // however much text a view happens to render.
  const DENSITY_BASE = 300;

  const DEFAULT_INTERVAL = 60;
  const DEFAULT_DENSITY = 2;

  const MIN_INTERVAL = 1;
  const MAX_INTERVAL = 3600;

  // Elements whose text must not be split: either it isn't rendered as text
  // nodes we can wrap, or wrapping would corrupt the control.
  const SKIP_TAGS = new Set([
    "SCRIPT", "STYLE", "NOSCRIPT", "TEXTAREA", "INPUT", "SELECT", "OPTION",
  ]);

  let timer = null;
  let applied = [];

  function clampInt(value, min, max, fallback) {
    const n = parseInt(value, 10);
    if (!Number.isFinite(n)) return fallback;
    return Math.min(max, Math.max(min, n));
  }

  function intervalSeconds() {
    return clampInt(localStorage.getItem(INTERVAL_KEY), MIN_INTERVAL, MAX_INTERVAL, DEFAULT_INTERVAL);
  }

  function density() {
    return clampInt(localStorage.getItem(DENSITY_KEY), 0, DENSITY_BASE, DEFAULT_DENSITY);
  }

  function randomColour() {
    // Full saturation at mid lightness stays legible on the Matrix black.
    return `hsl(${Math.floor(Math.random() * 360)}, 100%, 60%)`;
  }

  // ── DOM sampling ─────────────────────────────────────────────────────────

  function textNodes() {
    const root = document.getElementById("app");
    if (!root) return [];
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, {
      acceptNode(node) {
        if (!node.nodeValue || !/\S/.test(node.nodeValue)) return NodeFilter.FILTER_REJECT;
        const parent = node.parentElement;
        if (!parent || SKIP_TAGS.has(parent.tagName)) return NodeFilter.FILTER_REJECT;
        if (parent.classList.contains("chaos-char")) return NodeFilter.FILTER_REJECT;
        // Bare text inside a flex or grid container is an anonymous item;
        // wrapping one character would promote it to an item of its own and
        // stack it on a separate line, shattering the layout.
        const display = getComputedStyle(parent).display;
        if (display.includes("flex") || display.includes("grid")) return NodeFilter.FILTER_REJECT;
        return NodeFilter.FILTER_ACCEPT;
      },
    });
    const out = [];
    for (let n = walker.nextNode(); n; n = walker.nextNode()) out.push(n);
    return out;
  }

  // candidates lists every recolourable character as a {node, offset} pair.
  // Whitespace is skipped (colouring a space shows nothing), as is either
  // half of a surrogate pair — splitting one apart would corrupt the glyph.
  function candidates(nodes) {
    const out = [];
    for (const node of nodes) {
      const text = node.nodeValue;
      for (let i = 0; i < text.length; i++) {
        const code = text.charCodeAt(i);
        if (code >= 0xd800 && code <= 0xdfff) continue;
        if (!/\S/.test(text[i])) continue;
        out.push({ node, offset: i });
      }
    }
    return out;
  }

  function pick(spots, count) {
    // Partial Fisher-Yates: correct even when count approaches spots.length,
    // unlike rejection sampling.
    const idx = spots.map((_, i) => i);
    for (let i = 0; i < count; i++) {
      const j = i + Math.floor(Math.random() * (idx.length - i));
      const tmp = idx[i];
      idx[i] = idx[j];
      idx[j] = tmp;
    }
    return idx.slice(0, count).map((i) => spots[i]);
  }

  // ── apply / clear ────────────────────────────────────────────────────────

  function clear() {
    for (const span of applied) {
      const parent = span.parentNode;
      // A view that re-rendered has already discarded its spans.
      if (!parent) continue;
      parent.replaceChild(document.createTextNode(span.textContent), span);
      parent.normalize();
    }
    applied = [];
  }

  function paint(picks) {
    // Offsets shift as soon as a node is split, so group by node and work
    // from the end backwards — the head keeps the offsets still to come.
    const byNode = new Map();
    for (const p of picks) {
      if (!byNode.has(p.node)) byNode.set(p.node, []);
      byNode.get(p.node).push(p.offset);
    }

    for (const [node, offsets] of byNode) {
      offsets.sort((a, b) => b - a);
      for (const offset of offsets) {
        if (!node.parentNode) break;
        const target = node.splitText(offset);
        target.splitText(1);
        const span = document.createElement("span");
        span.className = "chaos-char";
        span.style.color = randomColour();
        target.parentNode.insertBefore(span, target);
        span.appendChild(target);
        applied.push(span);
      }
    }
  }

  function tick() {
    clear();
    const n = density();
    if (n <= 0) return;
    const spots = candidates(textNodes());
    if (!spots.length) return;
    const count = Math.min(spots.length, Math.round((spots.length * n) / DENSITY_BASE));
    if (count < 1) return;
    paint(pick(spots, count));
  }

  // ── lifecycle ────────────────────────────────────────────────────────────

  // The SPA renders its first view asynchronously after load, so a tick fired
  // the moment chaos starts can find an empty page and glitch nothing until
  // the next interval. Wait briefly for content before the opening glitch;
  // the interval itself runs on schedule regardless. A pending retry aborts
  // if the timer has since been stopped.
  function firstTick(attempts) {
    if (!timer) return;
    if (attempts > 0 && !candidates(textNodes()).length) {
      setTimeout(() => firstTick(attempts - 1), 250);
      return;
    }
    tick();
  }

  function start() {
    stop();
    timer = setInterval(tick, intervalSeconds() * 1000);
    firstTick(20);
  }

  function stop() {
    if (timer) clearInterval(timer);
    timer = null;
    clear();
  }

  // repaint re-samples immediately, for when a view re-render has discarded
  // the current sample. The interval is deliberately left running rather than
  // restarted, so the regular cadence stays on schedule. A null timer means
  // chaos is not the active theme, so there is nothing to repaint.
  function repaint() {
    if (!timer) return;
    tick();
  }

  function config() {
    return { intervalSeconds: intervalSeconds(), density: density() };
  }

  // setConfig persists both knobs and restarts the timer when chaos is the
  // active theme, so a save takes effect immediately.
  function setConfig(next) {
    localStorage.setItem(
      INTERVAL_KEY,
      String(clampInt(next.intervalSeconds, MIN_INTERVAL, MAX_INTERVAL, DEFAULT_INTERVAL)),
    );
    localStorage.setItem(
      DENSITY_KEY,
      String(clampInt(next.density, 0, DENSITY_BASE, DEFAULT_DENSITY)),
    );
    if (document.documentElement.dataset.theme === "chaos") start();
  }

  return { start, stop, repaint, config, setConfig, DENSITY_BASE };
})();
