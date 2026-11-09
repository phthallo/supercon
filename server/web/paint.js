(() => {
  const COLORS = [[255, 255, 255], [0, 0, 0], [255, 0, 0]];
  const WHITE = 0xffffff, RED = 0xff0000;
  const BAYER = [0, 8, 2, 10, 12, 4, 14, 6, 3, 11, 1, 9, 15, 7, 13, 5];
  // Same as Ink in render.go: the palette colour for rgb at cell (x, y).
  function ink(rgb, x, y) {
    const r = (rgb >> 16 & 255) / 255, g = (rgb >> 8 & 255) / 255, b = (rgb & 255) / 255;
    const red = Math.max(0, r - Math.max(g, b));
    const white = 0.299 * r + 0.587 * g + 0.114 * b - 0.299 * red;
    const black = 1 - white - red;
    const t = (BAYER[(y & 3) * 4 + (x & 3)] + 0.5) / 16;
    return t < black ? 1 : t < black + red ? 2 : 0;
  }
  const MAX_CELLS = 10000;
  const viewport = document.getElementById("viewport");
  const stage = document.getElementById("stage");
  const canvas = document.getElementById("canvas");
  const ctx = canvas.getContext("2d");
  const statusEl = document.getElementById("status");
  const coordsEl = document.getElementById("coords");
  const onlineEl = document.getElementById("online");
  const textInput = document.getElementById("text");

  let gridW = 240, gridH = 168;
  let cells = new Uint8Array(gridW * gridH);
  let off = document.createElement("canvas");
  let offCtx, offImg;
  let layer = null;
  let color = 0x000000;
  let tool = "brush";
  let size = 1;
  let readyAt = 0;
  let paintable = true;
  let hover = null;
  let stroke = null; // { start, last, cells: Set of indices }
  let message = null; // error shown until the next paint

  // ---- drawing -------------------------------------------------------------
  function setupOffscreen() {
    off.width = gridW; off.height = gridH;
    offCtx = off.getContext("2d");
    offImg = offCtx.createImageData(gridW, gridH);
  }

  function putCell(i, c) {
    const [r, g, b] = COLORS[c] || COLORS[0];
    const p = i * 4;
    offImg.data[p] = r; offImg.data[p + 1] = g; offImg.data[p + 2] = b; offImg.data[p + 3] = 255;
  }

  function paintRGB() { return tool === "eraser" ? WHITE : color; }
  function inkAt(rgb, i) { return ink(rgb, i % gridW, Math.floor(i / gridW)); }

  let queued = false;
  function redraw() {
    if (queued || !offCtx) return;
    queued = true;
    requestAnimationFrame(() => {
      queued = false;
      offCtx.putImageData(offImg, 0, 0);
      ctx.imageSmoothingEnabled = false;
      ctx.drawImage(off, 0, 0, canvas.width, canvas.height);
      const cw = canvas.width / gridW;
      const preview = stroke ? stroke.cells : tool === "text" && hover && !pointers.size ? textCells(hover) : null;
      if (preview) {
        const rgb = paintRGB();
        for (const i of preview) {
          const [r, g, b] = COLORS[inkAt(rgb, i)];
          ctx.fillStyle = `rgb(${r},${g},${b})`;
          ctx.fillRect((i % gridW) * cw, Math.floor(i / gridW) * cw, cw, cw);
        }
      }
      if (layer && layer.complete) ctx.drawImage(layer, 0, 0, canvas.width, canvas.height);
      if (hover && tool !== "move" && tool !== "text") {
        const [x0, y0] = brushOrigin(hover.x, hover.y);
        ctx.lineWidth = 2;
        ctx.strokeStyle = paintRGB() === RED ? "#000" : "#f00";
        ctx.strokeRect(x0 * cw + 1, y0 * cw + 1, size * cw - 2, size * cw - 2);
      }
    });
  }

  async function loadCanvas() {
    const res = await fetch("/api/canvas", { cache: "no-store" });
    cells = new Uint8Array(await res.arrayBuffer());
    for (let i = 0; i < cells.length; i++) putCell(i, cells[i]);
    redraw();
  }

  function loadLayer(v) {
    const img = new Image();
    img.onload = () => { layer = img; redraw(); };
    img.src = "/api/widgets.png?v=" + encodeURIComponent(v);
  }

  // ---- shapes --------------------------------------------------------------
  // The brush is a size×size square; for even sizes the extra row and column go right and down.
  function brushOrigin(x, y) {
    const h = Math.floor((size - 1) / 2);
    return [x - h, y - h];
  }

  function stamp(set, x, y) {
    const [x0, y0] = brushOrigin(x, y);
    for (let yy = y0; yy < y0 + size; yy++) {
      for (let xx = x0; xx < x0 + size; xx++) {
        if (xx >= 0 && yy >= 0 && xx < gridW && yy < gridH) set.add(yy * gridW + xx);
      }
    }
  }

  function line(set, a, b) {
    let { x, y } = a;
    const dx = Math.abs(b.x - x), dy = -Math.abs(b.y - y);
    const sx = x < b.x ? 1 : -1, sy = y < b.y ? 1 : -1;
    let err = dx + dy;
    for (;;) {
      stamp(set, x, y);
      if (x === b.x && y === b.y) return;
      const e2 = 2 * err;
      if (e2 >= dy) { err += dy; x += sx; }
      if (e2 <= dx) { err += dx; y += sy; }
    }
  }

  // Ellipse outline inside the box from a to b.
  function circle(a, b) {
    const x0 = Math.min(a.x, b.x), x1 = Math.max(a.x, b.x);
    const y0 = Math.min(a.y, b.y), y1 = Math.max(a.y, b.y);
    const cx = (x0 + x1 + 1) / 2, cy = (y0 + y1 + 1) / 2;
    const rx = (x1 - x0 + 1) / 2, ry = (y1 - y0 + 1) / 2;
    const inside = (x, y) => x >= x0 && x <= x1 && y >= y0 && y <= y1 &&
      ((x + 0.5 - cx) / rx) ** 2 + ((y + 0.5 - cy) / ry) ** 2 <= 1;
    const set = new Set();
    for (let y = y0; y <= y1; y++) {
      for (let x = x0; x <= x1; x++) {
        if (!inside(x, y)) continue;
        if (!inside(x - 1, y) || !inside(x + 1, y) || !inside(x, y - 1) || !inside(x, y + 1)) stamp(set, x, y);
      }
    }
    return set;
  }

  // 5×7 font for ASCII 32–126: five column bytes per glyph, bit 0 at the top.
  const FONT =
    "0000000000" + "00005f0000" + "0007000700" + "147f147f14" + "242a7f2a12" + "2313086462" + "3649552250" + "0005030000" +
    "001c224100" + "0041221c00" + "082a1c2a08" + "08083e0808" + "0050300000" + "0808080808" + "0060600000" + "2010080402" +
    "3e5149453e" + "00427f4000" + "4261514946" + "2141454b31" + "1814127f10" + "2745454539" + "3c4a494930" + "0171090503" +
    "3649494936" + "064949291e" + "0036360000" + "0056360000" + "0814224100" + "1414141414" + "0041221408" + "0201510906" +
    "324979413e" + "7e1111117e" + "7f49494936" + "3e41414122" + "7f4141221c" + "7f49494941" + "7f09090101" + "3e41415132" +
    "7f0808087f" + "00417f4100" + "2040413f01" + "7f08142241" + "7f40404040" + "7f0204027f" + "7f0408107f" + "3e4141413e" +
    "7f09090906" + "3e4151215e" + "7f09192946" + "4649494931" + "01017f0101" + "3f4040403f" + "1f2040201f" + "7f2018207f" +
    "6314081463" + "0304780403" + "6151494543" + "007f414100" + "0204081020" + "0041417f00" + "0402010204" + "4040404040" +
    "0001020400" + "2054545478" + "7f48444438" + "3844444420" + "384444487f" + "3854545418" + "087e090102" + "081454543c" +
    "7f08040478" + "00447d4000" + "2040443d00" + "007f102844" + "00417f4000" + "7c04180478" + "7c08040478" + "3844444438" +
    "7c14141408" + "081414187c" + "7c08040408" + "4854545420" + "043f444020" + "3c4040207c" + "1c2040201c" + "3c4030403c" +
    "4428102844" + "0c5050503c" + "4464544c44" + "0008364100" + "00007f0000" + "0041360800" + "0201020402";

  function textCells(at) {
    const set = new Set();
    let cx = at.x;
    for (const ch of textInput.value) {
      let code = ch.charCodeAt(0);
      if (code < 32 || code > 126) code = 63; // "?"
      for (let col = 0; col < 5; col++) {
        const bits = parseInt(FONT.substr(((code - 32) * 5 + col) * 2, 2), 16);
        for (let row = 0; row < 7; row++) {
          if (!(bits >> row & 1)) continue;
          for (let dy = 0; dy < size; dy++) {
            for (let dx = 0; dx < size; dx++) {
              const x = cx + col * size + dx, y = at.y + row * size + dy;
              if (x >= 0 && y >= 0 && x < gridW && y < gridH) set.add(y * gridW + x);
            }
          }
        }
      }
      cx += 6 * size;
    }
    return set;
  }

  function shapeCells(start, end) {
    if (tool === "circle") return circle(start, end);
    if (tool === "text") return textCells(end);
    return null;
  }

  // ---- zoom and pan --------------------------------------------------------
  let scale = 1, tx = 0, ty = 0;
  function clamp() {
    const w = viewport.clientWidth, h = viewport.clientHeight;
    scale = Math.min(Math.max(scale, 1), 16);
    tx = Math.min(0, Math.max(w - w * scale, tx));
    ty = Math.min(0, Math.max(h - h * scale, ty));
  }
  function apply() {
    clamp();
    stage.style.transform = `translate(${tx}px, ${ty}px) scale(${scale})`;
  }
  function zoomAt(factor, cx, cy) {
    const next = Math.min(Math.max(scale * factor, 1), 16);
    const k = next / scale;
    tx = cx - (cx - tx) * k;
    ty = cy - (cy - ty) * k;
    scale = next;
    apply();
  }
  function local(e) {
    const r = viewport.getBoundingClientRect();
    return { x: e.clientX - r.left, y: e.clientY - r.top };
  }
  function cellAt(e, clampToGrid) {
    const r = canvas.getBoundingClientRect();
    let x = Math.floor((e.clientX - r.left) / r.width * gridW);
    let y = Math.floor((e.clientY - r.top) / r.height * gridH);
    if (clampToGrid) {
      x = Math.min(Math.max(x, 0), gridW - 1);
      y = Math.min(Math.max(y, 0), gridH - 1);
    }
    return x >= 0 && y >= 0 && x < gridW && y < gridH ? { x, y } : null;
  }

  viewport.addEventListener("wheel", (e) => {
    e.preventDefault();
    const p = local(e);
    zoomAt(e.deltaY < 0 ? 1.2 : 1 / 1.2, p.x, p.y);
  }, { passive: false });

  const pointers = new Map();
  let drag = null, pinch = null, spaceHeld = false;

  function mid() {
    const [a, b] = [...pointers.values()];
    return { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2, dist: Math.hypot(a.x - b.x, a.y - b.y) };
  }

  viewport.addEventListener("pointerdown", (e) => {
    viewport.setPointerCapture(e.pointerId);
    pointers.set(e.pointerId, local(e));
    if (pointers.size === 1) {
      const pan = tool === "move" || spaceHeld || e.button === 1;
      if (pan) {
        drag = { start: local(e), tx, ty };
      } else if (e.button === 0) {
        startStroke(e);
      }
    } else if (pointers.size === 2) {
      cancelStroke();
      drag = null;
      const m = mid();
      pinch = { ...m, scale, tx, ty };
    }
  });

  viewport.addEventListener("pointermove", (e) => {
    const p = local(e);
    if (pointers.has(e.pointerId)) pointers.set(e.pointerId, p);
    if (pinch && pointers.size === 2) {
      const m = mid();
      scale = pinch.scale; tx = pinch.tx; ty = pinch.ty;
      zoomAt(m.dist / pinch.dist, pinch.x, pinch.y);
      tx += m.x - pinch.x; ty += m.y - pinch.y;
      apply();
      return;
    }
    if (drag) {
      tx = drag.tx + p.x - drag.start.x; ty = drag.ty + p.y - drag.start.y;
      apply();
    }
    if (stroke) moveStroke(e);
    const c = cellAt(e);
    if (c?.x !== hover?.x || c?.y !== hover?.y) {
      hover = c;
      coordsEl.textContent = c ? `x ${c.x} y ${c.y}` : "x – y –";
      redraw();
    }
  });

  function endPointer(e) {
    if (!pointers.has(e.pointerId)) return;
    pointers.delete(e.pointerId);
    if (pinch) { if (pointers.size < 2) pinch = null; return; }
    if (stroke) {
      if (e.type === "pointerup") commitStroke(); else cancelStroke();
    }
    drag = null;
  }
  viewport.addEventListener("pointerup", endPointer);
  viewport.addEventListener("pointercancel", endPointer);
  viewport.addEventListener("pointerleave", () => { if (!pointers.size) { hover = null; redraw(); } });
  viewport.addEventListener("contextmenu", (e) => e.preventDefault());

  document.getElementById("zoom-in").onclick = () => zoomAt(1.5, viewport.clientWidth / 2, viewport.clientHeight / 2);
  document.getElementById("zoom-out").onclick = () => zoomAt(1 / 1.5, viewport.clientWidth / 2, viewport.clientHeight / 2);
  document.getElementById("zoom-reset").onclick = () => { scale = 1; tx = 0; ty = 0; apply(); };
  window.addEventListener("resize", apply);

  // ---- strokes -------------------------------------------------------------
  function startStroke(e) {
    if (!paintable) return;
    if (Date.now() < readyAt) { tick(); return; }
    const c = cellAt(e);
    if (!c) return;
    stroke = { start: c, last: c, cells: new Set() };
    if (tool === "brush" || tool === "eraser") stamp(stroke.cells, c.x, c.y);
    else stroke.cells = shapeCells(c, c);
    redraw();
  }

  function moveStroke(e) {
    const c = cellAt(e, true);
    if (c.x === stroke.last.x && c.y === stroke.last.y) return;
    if (tool === "brush" || tool === "eraser") line(stroke.cells, stroke.last, c);
    else stroke.cells = shapeCells(stroke.start, c);
    stroke.last = c;
    redraw();
  }

  function cancelStroke() {
    if (!stroke) return;
    stroke = null;
    redraw();
  }

  async function commitStroke() {
    const rgb = paintRGB();
    const idx = [...stroke.cells].filter((i) => cells[i] !== inkAt(rgb, i));
    stroke = null;
    if (!idx.length) { redraw(); return; }
    if (idx.length > MAX_CELLS) {
      showError(`Too big: up to ${MAX_CELLS} squares at a time.`);
      redraw();
      return;
    }
    const prev = idx.map((i) => cells[i]);
    const next = idx.map((i) => inkAt(rgb, i));
    idx.forEach((i, k) => { cells[i] = next[k]; putCell(i, next[k]); });
    if (tool === "text") textInput.value = "";
    redraw();
    message = null;
    try {
      const res = await fetch("/api/pixel", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ p: idx.map((i) => [i % gridW, Math.floor(i / gridW)]), rgb }),
      });
      const body = await res.json().catch(() => ({}));
      if (body.retry_after != null) readyAt = Date.now() + body.retry_after * 1000;
      if (!res.ok) throw new Error(body.error || res.statusText);
    } catch (err) {
      idx.forEach((i, k) => { if (cells[i] === next[k]) { cells[i] = prev[k]; putCell(i, prev[k]); } });
      redraw();
      if (Date.now() >= readyAt) showError(String(err.message || err));
    }
    tick();
  }

  // ---- toolbar -------------------------------------------------------------
  function pressGroup(selector, match) {
    document.querySelectorAll(selector).forEach((b) => b.setAttribute("aria-pressed", String(match(b))));
  }
  const swatches = [...document.querySelectorAll(".swatch")];
  const custom = document.getElementById("custom");
  const customSwatch = custom.closest(".swatch");
  const hexRGB = (hex) => parseInt(hex.slice(1), 16);

  // Shows the dither pattern the colour will paint with.
  function showCustom() {
    const rgb = hexRGB(custom.value);
    const c = document.createElement("canvas");
    c.width = c.height = 4;
    const x = c.getContext("2d");
    for (let y = 0; y < 4; y++) {
      for (let xx = 0; xx < 4; xx++) {
        const [r, g, b] = COLORS[ink(rgb, xx, y)];
        x.fillStyle = `rgb(${r},${g},${b})`;
        x.fillRect(xx, y, 1, 1);
      }
    }
    customSwatch.style.setProperty("--sw", `url(${c.toDataURL()}) 0 0 / 16px 16px`);
  }

  function pickColor(b) {
    color = b === customSwatch ? hexRGB(custom.value) : parseInt(b.dataset.rgb, 16);
    if (tool === "eraser" || tool === "move") pickTool("brush");
    pressGroup(".swatch", (s) => s === b);
    redraw();
  }
  function pickTool(t) {
    tool = t;
    cancelStroke();
    viewport.style.cursor = t === "move" ? "grab" : "crosshair";
    pressGroup("[data-tool]", (b) => b.dataset.tool === t);
    textInput.hidden = t !== "text";
    sizeGroup.hidden = t === "move";
    if (t === "text") textInput.focus();
    if (toolSize[t]) pickSize(toolSize[t]);
    redraw();
  }
  function pickSize(s) {
    size = s;
    if (toolSize[tool]) toolSize[tool] = s;
    pressGroup("[data-size]", (b) => Number(b.dataset.size) === s);
    redraw();
  }
  const toolSize = { brush: 1, eraser: 4, circle: 1, text: 1 };
  const sizeGroup = document.getElementById("sizes");
  const SIZES = [...document.querySelectorAll("[data-size]")].map((b) => Number(b.dataset.size));
  swatches.forEach((b) => b.addEventListener("click", () => pickColor(b)));
  custom.addEventListener("input", () => { showCustom(); pickColor(customSwatch); });
  showCustom();
  document.querySelectorAll("[data-tool]").forEach((b) => b.addEventListener("click", () => pickTool(b.dataset.tool)));
  document.querySelectorAll("[data-size]").forEach((b) => b.addEventListener("click", () => pickSize(Number(b.dataset.size))));
  textInput.addEventListener("input", redraw);

  const KEYS = { b: "brush", e: "eraser", o: "circle", t: "text", m: "move" };
  window.addEventListener("keydown", (e) => {
    if (!paintable || e.target.closest("input, textarea") || e.ctrlKey || e.metaKey || e.altKey) return;
    if (e.key >= "1" && e.key <= "4") pickColor(swatches[Number(e.key) - 1]);
    else if (KEYS[e.key]) { e.preventDefault(); pickTool(KEYS[e.key]); }
    else if (e.key === "[" || e.key === "]") {
      const i = SIZES.indexOf(size) + (e.key === "]" ? 1 : -1);
      if (i >= 0 && i < SIZES.length) pickSize(SIZES[i]);
    } else if (e.key === " ") { spaceHeld = true; viewport.style.cursor = "grab"; e.preventDefault(); }
    else if (e.key === "Escape") cancelStroke();
  });
  window.addEventListener("keyup", (e) => {
    if (e.key === " ") { spaceHeld = false; viewport.style.cursor = tool === "move" ? "grab" : "crosshair"; }
  });
  pickTool("brush");
  pressGroup(".swatch", (b) => b.dataset.rgb && parseInt(b.dataset.rgb, 16) === color);

  const paintGroups = [...document.querySelectorAll(".tools .group:not(.zoom)")];
  function setPaintable(on) {
    if (on === paintable) return;
    paintable = on;
    paintGroups.forEach((g) => { g.hidden = !on; });
    pickTool(on ? "brush" : "move");
    document.getElementById("paint-note").hidden = !on;
    document.getElementById("view-note").hidden = on;
  }
  setPaintable(false);

  // ---- cooldown ------------------------------------------------------------
  function showError(text) {
    message = text;
    tick();
  }
  function tick() {
    if (!paintable) {
      statusEl.textContent = "view only";
      statusEl.className = "status";
      return;
    }
    const left = Math.ceil((readyAt - Date.now()) / 1000);
    if (left > 0) {
      statusEl.textContent = `wait ${left}s`;
      statusEl.className = "status wait";
    } else if (message) {
      statusEl.textContent = message;
      statusEl.className = "status wait";
    } else {
      statusEl.textContent = "ready to paint";
      statusEl.className = "status ready";
    }
  }
  setInterval(tick, 250);

  // ---- live updates --------------------------------------------------------
  let backoff = 1000;
  function connect() {
    const ws = new WebSocket((location.protocol === "https:" ? "wss://" : "ws://") + location.host + "/ws");
    ws.onopen = () => { backoff = 1000; loadCanvas(); refreshMe(); };
    ws.onmessage = (m) => {
      const e = JSON.parse(m.data);
      if (e.t === "px") {
        for (const [x, y] of e.p) { cells[y * gridW + x] = e.c; putCell(y * gridW + x, e.c); }
        redraw();
      } else if (e.t === "widgets") loadLayer(e.v);
      else if (e.t === "clear") loadCanvas();
    };
    ws.onclose = () => { setTimeout(connect, backoff); backoff = Math.min(backoff * 2, 30000); };
  }

  async function refreshMe() {
    const me = await (await fetch("/api/me", { cache: "no-store" })).json();
    readyAt = Date.now() + me.retry_after * 1000;
    document.getElementById("cooldown").textContent = Math.round(me.cooldown);
    setPaintable(me.paint);
    document.getElementById("cooldown-note").hidden = !me.paint || me.cooldown <= 0;
    onlineEl.textContent = me.online > 0 ? `${me.online} here` : "";
    if (!layer || !layer.src.endsWith(encodeURIComponent(me.widgets_v))) loadLayer(me.widgets_v);
    tick();
    return me;
  }

  (async () => {
    const me = await refreshMe();
    [gridW, gridH] = me.grid;
    canvas.width = me.panel[0]; canvas.height = me.panel[1];
    cells = new Uint8Array(gridW * gridH);
    setupOffscreen();
    await loadCanvas();
    apply();
    connect();
    setInterval(() => refreshMe().catch(() => {}), 30000);
  })();
})();
