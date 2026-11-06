(() => {
  const PW = 960, PH = 672;
  const stage = document.getElementById("stage");
  const preview = document.getElementById("preview");
  const form = document.getElementById("form");
  const empty = document.getElementById("empty");
  const errorEl = document.getElementById("error");
  let widgets = [];
  let selected = null;

  function showError(msg) { errorEl.textContent = msg || ""; }

  async function api(method, path, body) {
    const opts = { method, headers: {} };
    if (body instanceof FormData) opts.body = body;
    else if (body !== undefined) { opts.headers["Content-Type"] = "application/json"; opts.body = JSON.stringify(body); }
    const res = await fetch(path, opts);
    if (res.status === 204) return null;
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || res.statusText);
    return data;
  }

  // ---- preview -------------------------------------------------------------
  let previewTimer = null;
  function refreshPreview() {
    clearTimeout(previewTimer);
    previewTimer = setTimeout(() => { preview.src = "/badge/frame.png?t=" + Date.now(); }, 300);
  }

  // ---- widget boxes --------------------------------------------------------
  const pct = (v, of) => (v / of * 100) + "%";
  function place(el, w) {
    el.style.left = pct(w.x, PW); el.style.top = pct(w.y, PH);
    el.style.width = pct(w.w, PW); el.style.height = pct(w.h, PH);
  }

  function render() {
    stage.querySelectorAll(".wbox").forEach((el) => el.remove());
    for (const w of [...widgets].sort((a, b) => a.z - b.z || a.id - b.id)) {
      const el = document.createElement("div");
      el.className = "wbox" + (selected === w.id ? " selected" : "");
      el.dataset.id = w.id;
      place(el, w);
      const tag = document.createElement("span");
      tag.className = "tag";
      tag.textContent = `${w.kind} #${w.id}`;
      const handle = document.createElement("span");
      handle.className = "handle";
      el.append(tag, handle);
      stage.append(el);
      bindDrag(el, w);
    }
    fillForm();
  }

  function bindDrag(el, w) {
    el.addEventListener("pointerdown", (e) => {
      e.preventDefault();
      select(w.id);
      const resizing = e.target.classList.contains("handle");
      const rect = stage.getBoundingClientRect();
      const k = PW / rect.width;
      const start = { x: e.clientX, y: e.clientY, w: { ...w } };
      el.setPointerCapture(e.pointerId);
      const move = (ev) => {
        const dx = Math.round((ev.clientX - start.x) * k), dy = Math.round((ev.clientY - start.y) * k);
        if (resizing) {
          w.w = Math.min(PW, Math.max(1, start.w.w + dx));
          w.h = Math.min(PH, Math.max(1, start.w.h + dy));
        } else {
          w.x = start.w.x + dx; w.y = start.w.y + dy;
        }
        place(el, w);
        fillForm();
      };
      const up = async () => {
        el.removeEventListener("pointermove", move);
        el.removeEventListener("pointerup", up);
        el.removeEventListener("pointercancel", up);
        if (w.x === start.w.x && w.y === start.w.y && w.w === start.w.w && w.h === start.w.h) return;
        await save(w.id, { x: w.x, y: w.y, w: w.w, h: w.h }, start.w);
      };
      el.addEventListener("pointermove", move);
      el.addEventListener("pointerup", up);
      el.addEventListener("pointercancel", up);
    });
  }

  function select(id) {
    selected = id;
    stage.querySelectorAll(".wbox").forEach((el) => el.classList.toggle("selected", Number(el.dataset.id) === id));
    fillForm();
  }

  // ---- form ----------------------------------------------------------------
  const current = () => widgets.find((w) => w.id === selected);

  function fillForm() {
    const w = current();
    form.hidden = !w; empty.hidden = !!w;
    if (!w) return;
    document.getElementById("form-title").textContent = `${w.kind} #${w.id}`;
    for (const el of form.elements) {
      if (!el.name || el.type === "file" || el === document.activeElement) continue;
      if (!(el.name in w)) continue;
      if (el.type === "checkbox") el.checked = w[el.name];
      else el.value = w[el.name];
    }
    form.querySelectorAll("[data-for]").forEach((el) => {
      const kinds = el.dataset.for.split(" ");
      el.hidden = !kinds.includes(w.kind);
    });
  }

  form.addEventListener("change", async (e) => {
    const w = current();
    if (!w) return;
    const el = e.target;
    if (el.type === "file") {
      if (!el.files.length) return;
      const fd = new FormData();
      fd.append("file", el.files[0]);
      try {
        const updated = await api("POST", `/api/admin/widgets/${w.id}/image`, fd);
        Object.assign(w, updated); showError(); refreshPreview();
      } catch (err) { showError(err.message); }
      el.value = "";
      return;
    }
    const value = el.type === "checkbox" ? el.checked : el.type === "number" || el.tagName === "SELECT" && el.name !== "align" && el.name !== "font" ? Number(el.value) : el.value;
    await save(w.id, { [el.name]: value }, { ...w });
  });

  async function save(id, patch, before) {
    const w = widgets.find((x) => x.id === id);
    try {
      const updated = await api("PATCH", `/api/admin/widgets/${id}`, patch);
      Object.assign(w, updated);
      showError();
      refreshPreview();
    } catch (err) {
      Object.assign(w, before);
      showError(err.message);
    }
    render();
  }

  document.getElementById("delete").addEventListener("click", async () => {
    const w = current();
    if (!w || !confirm(`Are you sure you want to delete ${w.kind} #${w.id}?`)) return;
    try {
      await api("DELETE", `/api/admin/widgets/${w.id}`);
      widgets = widgets.filter((x) => x.id !== w.id);
      selected = null;
      render(); refreshPreview();
    } catch (err) { showError(err.message); }
  });

  document.querySelectorAll("[data-add]").forEach((btn) => btn.addEventListener("click", async () => {
    try {
      const w = await api("POST", "/api/admin/widgets", { kind: btn.dataset.add });
      widgets.push(w);
      selected = w.id;
      render(); refreshPreview();
    } catch (err) { showError(err.message); }
  }));

  document.getElementById("clear-canvas").addEventListener("click", async () => {
    if (!confirm("Are you sure you want to clear every painted pixel? This can't be undone.")) return;
    try { await api("POST", "/api/admin/canvas/clear"); refreshPreview(); } catch (err) { alert(err.message); }
  });

  stage.addEventListener("pointerdown", (e) => { if (e.target === preview) select(null); });

  // ---- live ----------------------------------------------------------------
  async function load() {
    const data = await api("GET", "/api/widgets");
    widgets = data.widgets;
    render();
  }

  function connect() {
    const ws = new WebSocket((location.protocol === "https:" ? "wss://" : "ws://") + location.host + "/ws");
    ws.onmessage = (m) => {
      const e = JSON.parse(m.data);
      if (e.t === "widgets") load();
      refreshPreview();
    };
    ws.onclose = () => setTimeout(connect, 3000);
  }

  load().catch((err) => showError(err.message));
  connect();
})();
