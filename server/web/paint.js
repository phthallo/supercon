(() => {
  const COLORS = [[255, 255, 255], [0, 0, 0], [255, 0, 0]];
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
  let readyAt = 0;
  let hover = null;

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

  let queued = false;
  function redraw() {
    if (queued || !offCtx) return;
    queued = true;
    requestAnimationFrame(() => {
      queued = false;
      offCtx.putImageData(offImg, 0, 0);
      ctx.imageSmoothingEnabled = false;
      ctx.drawImage(off, 0, 0, canvas.width, canvas.height);
      if (layer && layer.complete) ctx.drawImage(layer, 0, 0, canvas.width, canvas.height);
      if (hover) {
        const cw = canvas.width / gridW;
        ctx.lineWidth = 2;
        ctx.strokeStyle = color === 2 ? "#000" : "#f00";
        ctx.strokeRect(hover.x * cw + 1, hover.y * cw + 1, cw - 2, cw - 2);
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
  function cellAt(e) {
    const r = canvas.getBoundingClientRect();
    const x = Math.floor((e.clientX - r.left) / r.width * gridW);
    const y = Math.floor((e.clientY - r.top) / r.height * gridH);
    return x >= 0 && y >= 0 && x < gridW && y < gridH ? { x, y } : null;
  }

  viewport.addEventListener("wheel", (e) => {
    e.preventDefault();
    const p = local(e);
    zoomAt(e.deltaY < 0 ? 1.2 : 1 / 1.2, p.x, p.y);
  }, { passive: false });

  const pointers = new Map();
  let drag = null, pinch = null;

  viewport.addEventListener("pointerdown", (e) => {
    viewport.setPointerCapture(e.pointerId);
    pointers.set(e.pointerId, local(e));
    if (pointers.size === 1) {
      drag = { start: local(e), tx, ty, moved: false };
    } else if (pointers.size === 2) {
      const [a, b] = [...pointers.values()];
      pinch = { dist: Math.hypot(a.x - b.x, a.y - b.y), scale };
      drag = null;
    }
  });

  viewport.addEventListener("pointermove", (e) => {
    const p = local(e);
    if (pointers.has(e.pointerId)) pointers.set(e.pointerId, p);
    if (pinch && pointers.size === 2) {
      const [a, b] = [...pointers.values()];
      const dist = Math.hypot(a.x - b.x, a.y - b.y);
      zoomAt((pinch.scale * dist / pinch.dist) / scale, (a.x + b.x) / 2, (a.y + b.y) / 2);
      return;
    }
    if (drag) {
      const dx = p.x - drag.start.x, dy = p.y - drag.start.y;
      if (Math.hypot(dx, dy) > 5) drag.moved = true;
      if (drag.moved) { tx = drag.tx + dx; ty = drag.ty + dy; apply(); }
    }
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
    if (pinch) { if (pointers.size < 2) pinch = null; drag = null; return; }
    if (drag && !drag.moved && e.type === "pointerup") {
      const c = cellAt(e);
      if (c) paint(c.x, c.y);
    }
    drag = null;
  }
  viewport.addEventListener("pointerup", endPointer);
  viewport.addEventListener("pointercancel", endPointer);
  viewport.addEventListener("pointerleave", () => { if (!pointers.size) { hover = null; redraw(); } });

  document.getElementById("zoom-in").onclick = () => zoomAt(1.5, viewport.clientWidth / 2, viewport.clientHeight / 2);
  document.getElementById("zoom-out").onclick = () => zoomAt(1 / 1.5, viewport.clientWidth / 2, viewport.clientHeight / 2);
  document.getElementById("zoom-reset").onclick = () => { scale = 1; tx = 0; ty = 0; apply(); };
  window.addEventListener("resize", apply);

  // ---- colour --------------------------------------------------------------
  const swatches = [...document.querySelectorAll(".swatch")];
  function pick(c) {
    color = c;
    swatches.forEach((s) => s.setAttribute("aria-pressed", String(Number(s.dataset.c) === c)));
    redraw();
  }
  swatches.forEach((s) => s.addEventListener("click", () => pick(Number(s.dataset.c))));
  window.addEventListener("keydown", (e) => {
    if (e.target.closest("input, textarea")) return;
    if (e.key >= "1" && e.key <= "3") pick(Number(e.key) - 1);
  });

  // ---- painting and cooldown -----------------------------------------------
  function tick() {
    const left = Math.ceil((readyAt - Date.now()) / 1000);
    if (left > 0) {
      statusEl.textContent = `wait ${left}s`;
      statusEl.className = "status wait";
    } else {
      statusEl.textContent = "ready to paint";
      statusEl.className = "status ready";
    }
  }
  setInterval(tick, 250);

  async function paint(x, y) {
    if (Date.now() < readyAt) { tick(); return; }
    const i = y * gridW + x;
    const prev = cells[i];
    if (prev === color) return;
    cells[i] = color; putCell(i, color); redraw();
    try {
      const res = await fetch("/api/pixel", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ x, y, c: color }),
      });
      const body = await res.json().catch(() => ({}));
      if (body.retry_after != null) readyAt = Date.now() + body.retry_after * 1000;
      if (!res.ok) throw new Error(body.error || res.statusText);
    } catch (err) {
      cells[i] = prev; putCell(i, prev); redraw();
      if (Date.now() >= readyAt) { statusEl.textContent = String(err.message || err); statusEl.className = "status wait"; return; }
    }
    tick();
  }

  // ---- live updates --------------------------------------------------------
  let backoff = 1000;
  function connect() {
    const ws = new WebSocket((location.protocol === "https:" ? "wss://" : "ws://") + location.host + "/ws");
    ws.onopen = () => { backoff = 1000; loadCanvas(); refreshMe(); };
    ws.onmessage = (m) => {
      const e = JSON.parse(m.data);
      if (e.t === "px") { cells[e.y * gridW + e.x] = e.c; putCell(e.y * gridW + e.x, e.c); redraw(); }
      else if (e.t === "widgets") loadLayer(e.v);
      else if (e.t === "clear") loadCanvas();
    };
    ws.onclose = () => { setTimeout(connect, backoff); backoff = Math.min(backoff * 2, 30000); };
  }

  async function refreshMe() {
    const me = await (await fetch("/api/me", { cache: "no-store" })).json();
    readyAt = Date.now() + me.retry_after * 1000;
    document.getElementById("cooldown").textContent = Math.round(me.cooldown);
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
