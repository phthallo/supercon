(() => {
  const $ = (id) => document.getElementById(id);
  const cell = (v) => Object.assign(document.createElement("td"), { textContent: v });
  const button = (label, onClick) => {
    const td = document.createElement("td");
    const b = Object.assign(document.createElement("button"), { type: "button", className: "link", textContent: label });
    b.addEventListener("click", onClick);
    td.append(b);
    return td;
  };

  async function loadSongs() {
    const { songs } = await (await fetch("/api/rhythm/songs")).json();
    $("songs").replaceChildren(...songs.map((s) => {
      const tr = document.createElement("tr");
      tr.append(cell(s.id), cell(s.title), cell(s.has_audio ? "yes" : "synth"),
        cell(["easy", "normal", "hard"].map((d) => s.notes[d] || 0).join(" / ")),
        button("[ delete ]", async () => {
          if (!confirm(`Delete ${s.title}? Its scores stay and come back if you upload it again with id ${s.id}.`)) return;
          await fetch(`/api/admin/rhythm/songs/${encodeURIComponent(s.id)}`, { method: "DELETE" });
          loadSongs();
        }));
      return tr;
    }));
  }

  async function loadRuns() {
    const { runs } = await (await fetch("/api/admin/rhythm/runs")).json();
    $("runs").replaceChildren(...runs.map((r) => {
      const tr = document.createElement("tr");
      tr.append(cell(r.name), cell(`${r.title || r.song} ${r.diff}`), cell(r.score.toLocaleString("en-US")), cell(r.ip),
        button("[ remove ]", async () => {
          if (!confirm(`Remove ${r.name}'s run from the leaderboard?`)) return;
          await fetch(`/api/admin/rhythm/runs/${r.id}`, { method: "DELETE" });
          loadRuns();
        }));
      return tr;
    }));
  }

  $("upload").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("upload-msg").textContent = "";
    const submit = e.target.querySelector("button[type=submit]");
    submit.disabled = true;
    submit.textContent = "[ uploading ]";
    const res = await fetch("/api/admin/rhythm/songs", { method: "POST", body: new FormData(e.target) }).catch(() => null);
    const body = res ? await res.json().catch(() => ({})) : {};
    submit.disabled = false;
    submit.textContent = "[ upload ]";
    if (!res || !res.ok) {
      $("upload-msg").textContent = body.error || "Upload failed.";
      return;
    }
    e.target.reset();
    loadSongs();
  });

  loadSongs();
  loadRuns();
})();
