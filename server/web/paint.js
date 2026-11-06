(() => {
  const COLORS = [[255, 255, 255], [0, 0, 0], [255, 0, 0]];
  const MAX_CELLS = 2500;
  const viewport = document.getElementById("viewport");
  const stage = document.getElementById("stage");
  const canvas = document.getElementById("canvas");
  const ctx = canvas.getContext("2d");
  const statusEl = document.getElementById("status");
  const coordsEl = document.getElementById("coords");
  const onlineEl = document.getElementById("online");

  let gridW = 120, gridH = 84;
  let cells = new Uint8Array(gridW * gridH);
  let off = document.createElement("canvas");
  let offCtx, offImg;
  let layer = null;
  let color = 1;
  let tool = "brush";
  let size = 1;
  let fill = false;
  let readyAt = 0;
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

  function paintColor() { return tool === "eraser" ? 0 : color; }

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
      if (stroke) {
        const [r, g, b] = COLORS[paintColor()];
        ctx.fillStyle = `rgb(${r},${g},${b})`;
        for (const i of stroke.cells) ctx.fillRect((i % gridW) * cw, Math.floor(i / gridW) * cw, cw, cw);
      }
      if (layer && layer.complete) ctx.drawImage(layer, 0, 0, canvas.width, canvas.height);
      if (hover && tool !== "move") {
        const [x0, y0] = brushOrigin(hover.x, hover.y);
        ctx.lineWidth = 2;
        ctx.strokeStyle = paintColor() === 2 ? "#000" : "#f00";
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

  function box(a, b) {
    const set = new Set();
    const x0 = Math.min(a.x, b.x), x1 = Math.max(a.x, b.x);
    const y0 = Math.min(a.y, b.y), y1 = Math.max(a.y, b.y);
    if (fill) {
      for (let y = y0; y <= y1; y++) for (let x = x0; x <= x1; x++) set.add(y * gridW + x);
    } else {
      line(set, { x: x0, y: y0 }, { x: x1, y: y0 });
      line(set, { x: x1, y: y0 }, { x: x1, y: y1 });
      line(set, { x: x1, y: y1 }, { x: x0, y: y1 });
      line(set, { x: x0, y: y1 }, { x: x0, y: y0 });
    }
    return set;
  }

  // Ellipse inside the box from a to b.
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
        const edge = !inside(x - 1, y) || !inside(x + 1, y) || !inside(x, y - 1) || !inside(x, y + 1);
        if (fill) set.add(y * gridW + x);
        else if (edge) stamp(set, x, y);
      }
    }
    return set;
  }

  function shapeCells(start, end) {
    if (tool === "line") { const s = new Set(); line(s, start, end); return s; }
    if (tool === "box") return box(start, end);
    if (tool === "circle") return circle(start, end);
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
    const c = paintColor();
    const idx = [...stroke.cells].filter((i) => cells[i] !== c);
    stroke = null;
    if (!idx.length) { redraw(); return; }
    if (idx.length > MAX_CELLS) {
      showError(`Too big: up to ${MAX_CELLS} squares at a time.`);
      redraw();
      return;
    }
    const prev = idx.map((i) => cells[i]);
    idx.forEach((i) => { cells[i] = c; putCell(i, c); });
    redraw();
    message = null;
    try {
      const res = await fetch("/api/pixel", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ p: idx.map((i) => [i % gridW, Math.floor(i / gridW)]), c }),
      });
      const body = await res.json().catch(() => ({}));
      if (body.retry_after != null) readyAt = Date.now() + body.retry_after * 1000;
      if (!res.ok) throw new Error(body.error || res.statusText);
    } catch (err) {
      idx.forEach((i, k) => { if (cells[i] === c) { cells[i] = prev[k]; putCell(i, prev[k]); } });
      redraw();
      if (Date.now() >= readyAt) showError(String(err.message || err));
    }
    tick();
  }

  // ---- toolbar -------------------------------------------------------------
  function pressGroup(selector, match) {
    document.querySelectorAll(selector).forEach((b) => b.setAttribute("aria-pressed", String(match(b))));
  }
  function pickColor(c) {
    color = c;
    if (tool === "eraser" || tool === "move") pickTool("brush");
    pressGroup(".swatch", (b) => Number(b.dataset.c) === c);
    redraw();
  }
  function pickTool(t) {
    tool = t;
    cancelStroke();
    viewport.style.cursor = t === "move" ? "grab" : "crosshair";
    pressGroup("[data-tool]", (b) => b.dataset.tool === t);
    redraw();
  }
  function pickSize(s) {
    size = s;
    pressGroup("[data-size]", (b) => Number(b.dataset.size) === s);
    redraw();
  }
  const SIZES = [...document.querySelectorAll("[data-size]")].map((b) => Number(b.dataset.size));
  const fillButton = document.getElementById("fill");
  function toggleFill() {
    fill = !fill;
    fillButton.setAttribute("aria-pressed", String(fill));
  }

  document.querySelectorAll(".swatch").forEach((b) => b.addEventListener("click", () => pickColor(Number(b.dataset.c))));
  document.querySelectorAll("[data-tool]").forEach((b) => b.addEventListener("click", () => pickTool(b.dataset.tool)));
  document.querySelectorAll("[data-size]").forEach((b) => b.addEventListener("click", () => pickSize(Number(b.dataset.size))));
  fillButton.addEventListener("click", toggleFill);

  const KEYS = { b: "brush", e: "eraser", l: "line", r: "box", o: "circle", m: "move" };
  window.addEventListener("keydown", (e) => {
    if (e.target.closest("input, textarea") || e.ctrlKey || e.metaKey || e.altKey) return;
    if (e.key >= "1" && e.key <= "3") pickColor(Number(e.key) - 1);
    else if (KEYS[e.key]) pickTool(KEYS[e.key]);
    else if (e.key === "f") toggleFill();
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
  pickSize(1);

  // ---- cooldown ------------------------------------------------------------
  function showError(text) {
    message = text;
    tick();
  }
  function tick() {
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
    document.getElementById("cooldown-note").hidden = me.cooldown <= 0;
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
