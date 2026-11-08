(() => {
  const $ = (id) => document.getElementById(id);
  const NAME_KEY = "quadrant-name";
  const code = location.pathname.split("/").pop();
  const DIFF_NAMES = { easy: "Easy", normal: "Normal", hard: "Hard" };
  let run = null;

  function boardLink(name) {
    const q = new URLSearchParams({ song: run.song, diff: run.diff, name });
    const a = Object.assign(document.createElement("a"), { href: `/quadrant?${q}`, textContent: "[ leaderboard ]" });
    return a;
  }

  function showClaimed(name, fresh) {
    $("claim").hidden = true;
    $("claimed").replaceChildren(fresh ? `Saved as ${name}! ` : `Claimed by ${name}. `, boardLink(name));
    $("claimed").hidden = false;
  }

  async function load() {
    const res = await fetch(`/api/rhythm/claims/${encodeURIComponent(code)}`).catch(() => null);
    const body = res ? await res.json().catch(() => ({})) : {};
    if (!res || !res.ok) {
      $("load-error").textContent = body.error || "Failed to load. Try again in a moment.";
      $("load-error").hidden = false;
      return;
    }
    run = body;
    $("title").textContent = run.title || run.song;
    $("diff").textContent = DIFF_NAMES[run.diff] || run.diff;
    $("grade").textContent = run.grade;
    $("grade").className = `grade big g-${run.grade}`;
    $("score").textContent = run.score.toLocaleString("en-US");
    $("acc").textContent = `${(run.acc * 100).toFixed(2)}%`;
    $("counts").replaceChildren(...[...run.counts, `${run.max_combo}/${run.notes}`].map((v) => {
      const td = document.createElement("td");
      td.textContent = v;
      return td;
    }));
    $("run").hidden = false;
    if (run.name) {
      showClaimed(run.name, false);
      return;
    }
    $("claim").hidden = false;
    try { $("name").value = localStorage.getItem(NAME_KEY) || ""; } catch (e) {}
    $("name").focus();
  }

  $("claim").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("claim-error").textContent = "";
    const res = await fetch(`/api/rhythm/claims/${encodeURIComponent(code)}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name: $("name").value }),
    }).catch(() => null);
    const body = res ? await res.json().catch(() => ({})) : {};
    if (!res || !res.ok) {
      $("claim-error").textContent = body.error || "Failed to save. Try again in a moment.";
      return;
    }
    try { localStorage.setItem(NAME_KEY, body.name); } catch (err) {}
    showClaimed(body.name, true);
  });

  load();
})();
