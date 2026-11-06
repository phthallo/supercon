(() => {
  const statusEl = document.getElementById("status");
  const list = document.getElementById("networks");
  const busyButtons = [document.getElementById("add-ink"), document.getElementById("save-wifi")];

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
      row.remove();
      if (!list.children.length) addRow();
    });
    list.append(row);
  }
  addRow();
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

  function configFiles() {
    const nets = networks();
    if (!nets.length) throw new Error("Add at least one wifi network first.");
    const files = [["/data/wifi.json", enc(JSON.stringify({ networks: nets }))]];
    // On a local test server, the badge keeps its default server.
    if (!/^(localhost|127\.|\[::1\])/.test(location.hostname)) {
      files.push(["/data/ink.json", enc(JSON.stringify({ server: location.origin, poll_s: 60 }))]);
    }
    return files;
  }

  async function install(withApp) {
    let port, repl;
    busyButtons.forEach((b) => (b.disabled = true));
    try {
      const files = configFiles();
      if (withApp) {
        const res = await fetch("/install/files/ink.py");
        if (!res.ok) throw new Error("Couldn't download the app. Try again in a moment.");
        files.unshift(["/apps/ink.py", new Uint8Array(await res.arrayBuffer())]);
      }
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
      for (const [i, [path, bytes]] of files.entries()) {
        status(`Writing ${path} (${i + 1} of ${files.length})…`);
        await repl.writeFile(path, bytes);
      }
      await repl.reset();
      status("Done! Your badge is restarting.", "ready");
    } catch (e) {
      if (e.name === "NotFoundError") status("No badge picked.", "wait");
      else status(e.message || String(e), "wait");
    } finally {
      if (repl) await repl.close();
      if (port) await port.close().catch(() => {});
      busyButtons.forEach((b) => (b.disabled = !("serial" in navigator)));
    }
  }

  document.getElementById("add-ink").addEventListener("click", () => install(true));
  document.getElementById("save-wifi").addEventListener("click", () => install(false));
})();
