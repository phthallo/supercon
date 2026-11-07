(() => {
  const DIFFS = ["easy", "normal", "hard"];
  const $ = (id) => document.getElementById(id);
  const params = new URLSearchParams(location.search);
  let songs = [];
  let song = params.get("song");
  let diff = DIFFS.includes(params.get("diff")) ? params.get("diff") : "normal";
  const mark = params.get("name");

  const el = (tag, props = {}, ...kids) => {
    const n = Object.assign(document.createElement(tag), props);
    n.append(...kids);
    return n;
  };
  const fmt = (n) => n.toLocaleString("en-US");

  function renderSongs() {
    $("songs").replaceChildren(...songs.map((s) => {
      const b = el("button", { type: "button", className: "song" },
        el("span", { textContent: s.title }), el("span", { className: "muted", textContent: s.artist }));
      b.setAttribute("aria-pressed", s.id === song);
      b.addEventListener("click", () => { song = s.id; update(); });
      return el("li", {}, b);
    }));
  }

  function renderDiffs(s) {
    $("diffs").replaceChildren(...DIFFS.filter((d) => s.notes[d]).map((d) => {
      const b = el("button", { type: "button", className: "link", textContent: `[ ${d} ]` });
      b.setAttribute("aria-pressed", d === diff);
      b.addEventListener("click", () => { diff = d; update(); });
      return b;
    }));
  }

  async function update() {
    const s = songs.find((x) => x.id === song) || songs[0];
    if (!s) return;
    song = s.id;
    if (!s.notes[diff]) diff = DIFFS.find((d) => s.notes[d]);
    const q = new URLSearchParams({ song, diff });
    history.replaceState(null, "", `?${q}`);
    renderSongs();
    renderDiffs(s);
    $("board-title").textContent = s.title;
    $("board-error").hidden = true;
    const res = await fetch(`/api/rhythm/leaderboard?${q}`).catch(() => null);
    if (!res || !res.ok) {
      $("board").replaceChildren();
      $("board-empty").hidden = true;
      $("board-error").textContent = "Failed to load scores. Try again in a moment.";
      $("board-error").hidden = false;
      return;
    }
    const { runs } = await res.json();
    if (song !== s.id) return;
    $("board").replaceChildren(...runs.map((r, i) => {
      const tr = el("tr", {},
        el("td", { textContent: i + 1 }),
        el("td", { textContent: r.name }),
        el("td", { textContent: fmt(r.score) }),
        el("td", { textContent: `${(r.acc * 100).toFixed(2)}%` }),
        el("td", { textContent: r.grade, className: `grade g-${r.grade}` }),
        el("td", { textContent: `${r.max_combo}/${r.notes}` }));
      if (mark && r.name.toLowerCase() === mark.toLowerCase()) tr.className = "mine";
      return tr;
    }));
    $("board-empty").hidden = runs.length > 0;
  }

  fetch("/api/rhythm/songs").then((r) => r.json()).then((d) => { songs = d.songs; update(); })
    .catch(() => { $("board-error").textContent = "Failed to load songs. Try again in a moment."; $("board-error").hidden = false; });
})();
