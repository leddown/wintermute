// Falling-glyph backdrop for the Matrix and Chaos themes.
//
// It exists for a layout reason, not only a decorative one. An admin pane or
// a canary table is a finite amount of content, so on a 21:9 or 32:9 display
// there is always screen left over below and beside it — the wider the
// monitor, the more of the window is empty. The rain gives that space
// something to be, so the void reads as deliberate rather than as a page
// that ran out.
//
// It stays strictly behind the panes, which are opaque: the glyphs are only
// ever visible where there is nothing to read, and nothing has to be dimmed
// or made translucent to accommodate them.
//
// How bright the glyphs are is a per-browser setting, edited from the
// Admin → Appearance tab beside the Chaos knobs: what reads as a faint
// texture on one monitor is either invisible or distracting on the next, and
// it is a display property rather than anything the server has an opinion
// about.
const WintermuteRain = (() => {
  // A glyph cell. Coarse on purpose — this is texture at low opacity, and a
  // finer grid would cost real work every frame to render detail nobody can
  // resolve.
  const CELL = 16;

  const BRIGHTNESS_KEY = "wintermute-rain-brightness";

  // The opacity the rain is faded to at 100%: faint enough to sit behind the
  // panes without competing with them. Kept here rather than in the stylesheet
  // because the brightness setting scales it — the CSS value is the fallback
  // for the moment before this module has run.
  const BASE_OPACITY = 0.18;

  // Brightness is a percentage scaling that fade, so 100 is the weight the
  // theme was designed at and 10 is nearly invisible. The ceiling is 500
  // because that is where the fade reaches 0.9 — beyond it the glyphs are
  // effectively opaque and the control would have nothing left to move.
  const DEFAULT_BRIGHTNESS = 100;
  const MIN_BRIGHTNESS = 10;
  const MAX_BRIGHTNESS = 500;

  // Redraws per second. The rain reads as rain at 12; at 60 it costs five
  // times as much to look almost identical, on a machine whose actual job is
  // scanning shares and talking to an API.
  const FPS = 12;

  // How much of the previous frame is painted over each tick. Lower leaves
  // longer, brighter trails.
  const TRAIL_FADE = 0.08;

  // Katakana, the glyphs the film uses, plus digits — a set with enough
  // shapes that a column never looks like it is repeating.
  const GLYPHS = "アイウエオカキクケコサシスセソタチツテトナニヌネノハヒフヘホマミムメモヤユヨラリルレロワヲン0123456789";

  let canvas = null;
  let ctx = null;
  let columns = [];
  let timer = null;
  let running = false;

  function reduceMotion() {
    return window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  }

  function brightness() {
    const n = parseInt(localStorage.getItem(BRIGHTNESS_KEY), 10);
    if (!Number.isFinite(n)) return DEFAULT_BRIGHTNESS;
    return Math.min(MAX_BRIGHTNESS, Math.max(MIN_BRIGHTNESS, n));
  }

  // applyBrightness sets the canvas's opacity from the current setting.
  //
  // The opacity is what the brightness control has to move. The glyphs are
  // painted at the palette's own weights and then the whole canvas is faded to
  // BASE_OPACITY so it stays behind the panes — which means the alpha a glyph
  // is drawn at is multiplied by 0.18 before it reaches the screen, and no
  // amount of scaling inside the canvas can get past that. Scaling the fade
  // itself is the only lever with real range: 100% is the 0.18 the theme was
  // designed at, and the ceiling is a fully opaque field.
  //
  // Clamped at 1 because opacity saturates there; MAX_BRIGHTNESS is set so the
  // top of the range lands just under it rather than in a dead zone.
  function applyBrightness() {
    if (!canvas) return;
    canvas.style.opacity = String(Math.min(1, BASE_OPACITY * (brightness() / 100)));
  }

  function glyph() {
    return GLYPHS[Math.floor(Math.random() * GLYPHS.length)];
  }

  // resize rebuilds the column heads for the current window. Each column
  // starts at a random height so the rain is already falling when it appears
  // rather than beginning as one flat line across the top.
  function resize() {
    if (!canvas) return;
    const dpr = Math.min(window.devicePixelRatio || 1, 2); // 2 is plenty for glyphs this size
    canvas.width = Math.floor(window.innerWidth * dpr);
    canvas.height = Math.floor(window.innerHeight * dpr);
    canvas.style.width = window.innerWidth + "px";
    canvas.style.height = window.innerHeight + "px";

    ctx = canvas.getContext("2d");
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.font = `${CELL}px "Courier New", Courier, monospace`;
    ctx.textBaseline = "top";

    const count = Math.ceil(window.innerWidth / CELL);
    columns = new Array(count);
    for (let i = 0; i < count; i++) {
      columns[i] = Math.random() * (window.innerHeight / CELL);
    }
    ctx.fillStyle = "#000";
    ctx.fillRect(0, 0, window.innerWidth, window.innerHeight);
  }

  function tick() {
    if (!ctx) return;
    const w = window.innerWidth;
    const h = window.innerHeight;

    // Fade rather than clear: what is left of the previous frames is the
    // trail behind each falling head.
    ctx.fillStyle = `rgba(0, 0, 0, ${TRAIL_FADE})`;
    ctx.fillRect(0, 0, w, h);

    for (let i = 0; i < columns.length; i++) {
      const x = i * CELL;
      const y = columns[i] * CELL;

      // The head is brighter than its trail, which is what makes a column
      // read as falling rather than as a static string of characters.
      ctx.fillStyle = "rgba(140, 255, 170, 0.85)";
      ctx.fillText(glyph(), x, y);
      ctx.fillStyle = "rgba(0, 255, 65, 0.55)";
      ctx.fillText(glyph(), x, y - CELL);

      // Past the bottom, restart high up — but only sometimes, so the
      // columns drift out of step instead of marching in rows.
      if (y > h && Math.random() > 0.975) {
        columns[i] = 0;
      } else {
        columns[i] += 1;
      }
    }
  }

  // paintStill draws a single frame of scattered glyph runs. This is what
  // prefers-reduced-motion gets: the same texture at the same weight, holding
  // still. The preference is about movement, not contrast — dimming it to
  // nothing would take the theme away rather than just the animation.
  function paintStill() {
    if (!ctx) return;
    ctx.fillStyle = "#000";
    ctx.fillRect(0, 0, window.innerWidth, window.innerHeight);
    const rows = window.innerHeight / CELL;
    for (let i = 0; i < columns.length; i++) {
      // Runs of varying length at varying heights, so the field reads as rain
      // caught mid-fall rather than as an even wash of characters.
      const runLength = 4 + Math.floor(Math.random() * 11);
      const top = Math.random() * rows;
      for (let j = 0; j < runLength; j++) {
        // Each run fades along its length, the way a trail does.
        ctx.fillStyle = `rgba(0, 255, 65, ${0.75 - (j / runLength) * 0.5})`;
        ctx.fillText(glyph(), i * CELL, (top + j) * CELL);
      }
    }
  }

  function ensureCanvas() {
    if (canvas) return;
    canvas = document.getElementById("matrix-rain");
    if (!canvas) return;
    resize();
    window.addEventListener("resize", () => {
      resize();
      if (running && reduceMotion()) paintStill();
    });
    // A backgrounded tab has nothing to animate for, and browsers throttle
    // the timer unevenly, which makes the rain lurch on return.
    document.addEventListener("visibilitychange", () => {
      if (document.hidden) {
        stopTimer();
      } else if (running && !reduceMotion()) {
        startTimer();
      }
    });
  }

  function startTimer() {
    stopTimer();
    timer = setInterval(tick, 1000 / FPS);
  }

  function stopTimer() {
    if (timer) clearInterval(timer);
    timer = null;
  }

  function start() {
    ensureCanvas();
    if (!canvas) return;
    running = true;
    canvas.hidden = false;
    applyBrightness();
    if (reduceMotion()) {
      paintStill();
      return;
    }
    startTimer();
  }

  function stop() {
    running = false;
    stopTimer();
    if (canvas) canvas.hidden = true;
  }

  function config() {
    return { brightness: brightness() };
  }

  // setConfig persists the setting and applies it now. The fade is a property
  // of the canvas element rather than of the pixels in it, so changing it
  // takes effect on the frame already on screen — there is nothing to repaint
  // and no wait for the trails to turn over.
  function setConfig(next) {
    const n = parseInt(next.brightness, 10);
    localStorage.setItem(
      BRIGHTNESS_KEY,
      String(Number.isFinite(n)
        ? Math.min(MAX_BRIGHTNESS, Math.max(MIN_BRIGHTNESS, n))
        : DEFAULT_BRIGHTNESS),
    );
    applyBrightness();
  }

  return {
    start, stop, config, setConfig,
    MIN_BRIGHTNESS, MAX_BRIGHTNESS, DEFAULT_BRIGHTNESS,
  };
})();
