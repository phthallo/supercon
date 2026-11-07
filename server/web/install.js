(() => {
  const statusEl = document.getElementById("status");
  const list = document.getElementById("networks");
  const busyButtons = ["add-ink", "save-wifi", "load-wifi"].map((id) => document.getElementById(id));
  // Networks removed from the list here, so a save drops them from the badge too.
  const removed = new Set();

  function status(text, kind = "") {
    statusEl.textContent = text;
    statusEl.className = "status " + kind;
  }

  if (!("serial" in navigator)) {
    document.getElementById("unsupported").hidden = false;
    busyButtons.forEach((b) => (b.disabled = true));
  }

  // ---- wifi rows ------------------------------------------------------------
  function addRow(ssid = "", password = "") {
    const row = document.createElement("div");
    row.className = "network";
    row.innerHTML = `
      <label>name <input name="ssid" autocomplete="off" spellcheck="false"></label>
      <label>password <input name="password" type="password" autocomplete="off"></label>
      <button class="link" type="button">[ remove ]</button>`;
    row.querySelector("[name=ssid]").value = ssid;
    row.querySelector("[name=password]").value = password;
    row.querySelector("button").addEventListener("click", () => {
      const ssid = row.querySelector("[name=ssid]").value.trim();
      if (ssid) removed.add(ssid);
      row.remove();
      if (!list.children.length) addRow();
    });
    list.append(row);
  }
  fetch("/api/install")
    .then((r) => r.json())
    .then((cfg) => cfg.networks.forEach((n) => addRow(n.ssid, n.password)))
    .catch(() => {})
    .finally(() => { if (!list.children.length) addRow(); });
  document.getElementById("add-network").addEventListener("click", () => addRow());

  function networks() {
    return [...list.children]
      .map((row) => ({
        ssid: row.querySelector("[name=ssid]").value.trim(),
        password: row.querySelector("[name=password]").value,
      }))
      .filter((n) => n.ssid);
  }

  // ---- badge ----------------------------------------------------------------
  const enc = (s) => new TextEncoder().encode(s);

  // The networks saved on the badge. Older badges store a single {ssid, password}.
  async function badgeNetworks(repl) {
    const text = await repl.readText("/data/wifi.json");
    if (!text) return [];
    try {
      const cfg = JSON.parse(text);
      return (cfg.networks || [cfg]).filter((n) => n && n.ssid);
    } catch (e) {
      return [];
    }
  }

  // Opens the badge's REPL, runs fn with it, and closes the port. Returns fn's result.
  async function withBadge(fn) {
    let port, repl;
    busyButtons.forEach((b) => (b.disabled = true));
    try {
      status("Pick your badge in the browser's list…");
      port = await navigator.serial.requestPort({ filters: [{ usbVendorId: 0x303a }] });
      try {
        await port.open({ baudRate: 115200 });
      } catch (e) {
        throw new Error("Couldn't open the badge. Close the flasher window or any other serial tool, then try again.");
      }
      repl = new Repl(port.readable, port.writable);
      status("Connecting…");
      await repl.enter();
      return await fn(repl);
    } catch (e) {
      if (e.name === "NotFoundError") status("No badge picked.", "wait");
      else status(e.message || String(e), "wait");
    } finally {
      if (repl) await repl.close();
      if (port) await port.close().catch(() => {});
      busyButtons.forEach((b) => (b.disabled = !("serial" in navigator)));
    }
  }

  async function install(withApp) {
    const nets = networks();
    if (!nets.length) {
      status("Add at least one wifi network first.", "wait");
      return;
    }
    let app;
    if (withApp) {
      const res = await fetch("/install/files/ink.py").catch(() => null);
      if (!res || !res.ok) {
        status("Couldn't download the app. Try again in a moment.", "wait");
        return;
      }
      app = new Uint8Array(await res.arrayBuffer());
    }
    await withBadge(async (repl) => {
      const listed = new Set(nets.map((n) => n.ssid));
      const kept = (await badgeNetworks(repl)).filter((n) => !listed.has(n.ssid) && !removed.has(n.ssid));
      const files = [["/data/wifi.json", enc(JSON.stringify({ networks: [...nets, ...kept] }))]];
      // On a local test server, the badge keeps its default server.
      if (!/^(localhost|127\.|\[::1\])/.test(location.hostname)) {
        files.push(["/data/ink.json", enc(JSON.stringify({ server: location.origin, poll_s: 60 }))]);
      }
      if (app) files.unshift(["/apps/ink.py", app]);
      for (const [i, [path, bytes]] of files.entries()) {
        status(`Writing ${path} (${i + 1} of ${files.length})…`);
        await repl.writeFile(path, bytes);
      }
      await repl.reset();
      const also = kept.length ? ` Also kept ${kept.map((n) => n.ssid).join(", ")}, already on your badge.` : "";
      status(`Done! Your badge is restarting.${also}`, "ready");
    });
  }

  async function load() {
    await withBadge(async (repl) => {
      const saved = await badgeNetworks(repl);
      const listed = new Set(networks().map((n) => n.ssid));
      // Drop the empty starter row so loaded networks don't sit under a blank one.
      [...list.children].forEach((row) => { if (!row.querySelector("[name=ssid]").value.trim()) row.remove(); });
      const fresh = saved.filter((n) => !listed.has(n.ssid));
      fresh.forEach((n) => { removed.delete(n.ssid); addRow(n.ssid, n.password); });
      if (!list.children.length) addRow();
      status(saved.length ? `Loaded ${saved.length} network${saved.length === 1 ? "" : "s"} from your badge.` : "No wifi saved on your badge yet.", "ready");
    });
  }

  document.getElementById("add-ink").addEventListener("click", () => install(true));
  document.getElementById("save-wifi").addEventListener("click", () => install(false));
  document.getElementById("load-wifi").addEventListener("click", load);
})();
