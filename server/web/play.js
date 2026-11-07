(() => {
  const $ = (id) => document.getElementById(id);
  const OFFSET_KEY = "quadrant-offset";
  const DIFF_NAMES = { easy: "Easy", normal: "Normal", hard: "Hard" };

  let ctx = null;
  let clockOffset = 0; // server ms minus local ms
  let songs = {};
  const buffers = new Map();
  let source = null;
  let es = null;
  let room = null;
  let state = { t: "idle", seq: -1 };
  let resync = 0;
  let frame = 0;

  const localMs = () => performance.timeOrigin + performance.now();
  const serverNow = () => localMs() + clockOffset;
  const mmss = (ms) => {
    const s = Math.max(0, Math.floor(ms / 1000));
    return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
  };

  function offsetMs() {
    try { return Number(localStorage.getItem(OFFSET_KEY)) || 0; } catch (e) { return Number($("offset").value) || 0; }
  }
  function setOffset(v) {
    $("offset").value = v;
    try { localStorage.setItem(OFFSET_KEY, String(v)); } catch (e) {}
  }

  function audio() {
    if (!ctx) ctx = new (window.AudioContext || window.webkitAudioContext)({ latencyHint: "interactive" });
    if (ctx.state === "suspended") ctx.resume();
    return ctx;
  }

  // Context time at which a sound must be scheduled to be heard at local performance time perfMs.
  function ctxTimeFor(perfMs) {
    const ts = ctx.getOutputTimestamp ? ctx.getOutputTimestamp() : null;
    if (ts && ts.performanceTime > 0) return ts.contextTime + (perfMs - ts.performanceTime) / 1000;
    const latency = ctx.outputLatency || ctx.baseLatency || 0;
    return ctx.currentTime + (perfMs - performance.now()) / 1000 - latency;
  }
  function perfFor(ctxTime) {
    const ts = ctx.getOutputTimestamp ? ctx.getOutputTimestamp() : null;
    if (ts && ts.performanceTime > 0) return ts.performanceTime + (ctxTime - ts.contextTime) * 1000;
    const latency = ctx.outputLatency || ctx.baseLatency || 0;
    return performance.now() + (ctxTime - ctx.currentTime + latency) * 1000;
  }

  // NTP-style: keep the offset from the round trip with the least delay.
  async function syncClock() {
    const samples = [];
    for (let i = 0; i < 8; i++) {
      const t0 = localMs();
      const res = await fetch("/api/rhythm/time", { cache: "no-store" });
      const { t } = await res.json();
      const t1 = localMs();
      samples.push({ rtt: t1 - t0, off: t - (t0 + t1) / 2 });
    }
    samples.sort((a, b) => a.rtt - b.rtt);
    clockOffset = samples[0].off;
  }

  async function songInfo(id) {
    if (!songs[id]) {
      const res = await fetch("/api/rhythm/songs");
      for (const s of (await res.json()).songs) songs[s.id] = s;
    }
    return songs[id];
  }

  // Kick on each beat, accent on the bar, hat on the off-beat.
  async function synthTrack(song) {
    const rate = 44100;
    const len = song.duration_ms / 1000 + 1;
    const off = new OfflineAudioContext(1, Math.ceil(rate * len), rate);
    const noise = off.createBuffer(1, rate / 20, rate);
    const data = noise.getChannelData(0);
    for (let i = 0; i < data.length; i++) data[i] = Math.random() * 2 - 1;
    const beat = 60 / song.bpm;
    let i = 0;
    for (let t = song.offset_ms / 1000; t < len; t += beat, i++) {
      if (t < 0) continue;
      const osc = off.createOscillator();
      const g = off.createGain();
      osc.frequency.setValueAtTime(i % 4 === 0 ? 180 : 140, t);
      osc.frequency.exponentialRampToValueAtTime(45, t + 0.12);
      g.gain.setValueAtTime(i % 4 === 0 ? 0.9 : 0.6, t);
      g.gain.exponentialRampToValueAtTime(0.001, t + 0.22);
      osc.connect(g).connect(off.destination);
      osc.start(t);
      osc.stop(t + 0.25);
      const h = off.createBufferSource();
      const hp = off.createBiquadFilter();
      const hg = off.createGain();
      h.buffer = noise;
      hp.type = "highpass";
      hp.frequency.value = 7000;
      hg.gain.setValueAtTime(0.25, t + beat / 2);
      hg.gain.exponentialRampToValueAtTime(0.001, t + beat / 2 + 0.05);
      h.connect(hp).connect(hg).connect(off.destination);
      h.start(t + beat / 2);
    }
    return off.startRendering();
  }

  function loadBuffer(song) {
    if (!buffers.has(song.id)) {
      const p = song.has_audio
        ? fetch(`/api/rhythm/songs/${encodeURIComponent(song.id)}/audio`).then((r) => {
          if (!r.ok) throw new Error(`audio ${r.status}`);
          return r.arrayBuffer();
        }).then((b) => ctx.decodeAudioData(b))
        : synthTrack(song);
      p.catch(() => buffers.delete(song.id));
      buffers.set(song.id, p);
    }
    return buffers.get(song.id);
  }

  function stopAudio() {
    if (source) {
      try { source.stop(); } catch (e) {}
      source.disconnect();
      source = null;
    }
  }

  // Plays the song so that audio position 0 is heard at server time startAt.
  async function playFrom(st) {
    stopAudio();
    const song = await songInfo(st.song);
    if (!song) throw new Error("unknown song");
    const buf = await loadBuffer(song);
    if (st.seq !== state.seq) return;
    const heard = st.start_at + offsetMs() - clockOffset - performance.timeOrigin;
    let at = ctxTimeFor(heard);
    let pos = 0;
    const soonest = ctx.currentTime + 0.03;
    if (at < soonest) {
      pos = soonest - at;
      at = soonest;
    }
    if (pos >= buf.duration) return;
    const src = ctx.createBufferSource();
    src.buffer = buf;
    src.connect(ctx.destination);
    src.start(at, pos);
    source = src;
  }

  function show(id, on) { $(id).hidden = !on; }

  function tick() {
    cancelAnimationFrame(frame);
    const song = songs[state.song];
    const playing = (state.t === "start" || state.t === "resume" || state.t === "result") && song && state.start_at;
    if (!playing) return;
    const t = serverNow() - state.start_at;
    if (state.t !== "result") {
      show("big", t < 0);
      if (t < 0) $("big").textContent = Math.ceil(-t / 1000);
    }
    $("bar").style.width = `${Math.min(100, Math.max(0, (t / song.duration_ms) * 100))}%`;
    $("elapsed").textContent = mmss(t);
    if (t < song.duration_ms) frame = requestAnimationFrame(tick);
  }

  async function apply(st) {
    if (st.seq === state.seq) return;
    const prev = state;
    state = st;
    $("room-error").textContent = "";
    if (st.t === "result" && prev.start_at) state.start_at = prev.start_at;
    const song = st.song ? await songInfo(st.song).catch(() => null) : null;
    if (st !== state) return;
    const active = !!song && st.t !== "stop" && st.t !== "idle";
    $("song-title").textContent = active ? song.title : "Waiting for the badge";
    $("song-meta").textContent = active ? `${song.artist}, ${DIFF_NAMES[st.diff] || st.diff}` : "";
    show("song-meta", active);
    show("progress", active && (st.t !== "result" || !!state.start_at));
    show("result", active && st.t === "result");
    show("big", false);
    if (!active) {
      stopAudio();
      return;
    }
    $("total").textContent = mmss(song.duration_ms);

    switch (st.t) {
      case "start":
      case "resume":
        playFrom(st).catch(() => { $("room-error").textContent = "Failed to load the audio."; });
        tick();
        break;
      case "pause":
        stopAudio();
        $("big").textContent = "Paused";
        show("big", true);
        $("bar").style.width = `${Math.min(100, (st.pos_ms / song.duration_ms) * 100)}%`;
        $("elapsed").textContent = mmss(st.pos_ms);
        break;
      case "result":
        $("res-grade").textContent = st.grade;
        $("res-grade").className = `grade big g-${st.grade}`;
        $("res-score").textContent = st.score.toLocaleString("en-US");
        $("res-acc").textContent = `${(st.acc * 100).toFixed(2)}%`;
        $("res-claim").href = new URL(st.claim).pathname;
        tick();
        break;
    }
  }

  function leave(msg) {
    if (es) es.close();
    es = null;
    stopAudio();
    clearInterval(resync);
    cancelAnimationFrame(frame);
    room = null;
    state = { t: "idle", seq: -1 };
    show("room", false);
    show("join", true);
    $("join-error").textContent = msg || "";
    history.replaceState(null, "", "/play");
  }

  function listen() {
    es = new EventSource(`/api/rhythm/rooms/${room}/events`);
    es.onmessage = (e) => apply(JSON.parse(e.data));
    es.addEventListener("gone", () => leave("That room expired. Make a new one on the badge."));
    es.onerror = async () => {
      if (es.readyState !== EventSource.CLOSED) return;
      const res = await fetch(`/api/rhythm/rooms/${room}`).catch(() => null);
      if (res && res.status === 404) leave("That room closed. Make a new one on the badge.");
      else setTimeout(() => room && listen(), 2000);
    };
  }

  $("join").addEventListener("submit", async (e) => {
    e.preventDefault();
    audio();
    const code = $("code").value.trim().toUpperCase();
    $("join-error").textContent = "";
    try {
      const res = await fetch(`/api/rhythm/rooms/${encodeURIComponent(code)}`);
      if (res.status === 404) {
        $("join-error").textContent = "No room with that code.";
        return;
      }
      if (!res.ok) throw new Error(res.status);
      await syncClock();
      await songInfo("");
    } catch (err) {
      $("join-error").textContent = "Failed to connect. Try again in a moment.";
      return;
    }
    room = code;
    $("room-code").textContent = code;
    show("join", false);
    show("room", true);
    history.replaceState(null, "", `/play?room=${code}`);
    resync = setInterval(() => syncClock().catch(() => {}), 120000);
    listen();
  });
  $("code").addEventListener("input", (e) => { e.target.value = e.target.value.toUpperCase(); });
  $("leave").addEventListener("click", () => leave());

  // Calibration: clicks every 500 ms; the offset is the median of how late the taps were.
  const TAP_CLICKS = 16;
  const TAP_WARMUP = 4;
  let tapTest = null;

  $("offset").value = offsetMs();
  $("offset").addEventListener("change", (e) => setOffset(Math.round(Number(e.target.value) || 0)));

  $("tap-start").addEventListener("click", () => {
    audio();
    const t0 = ctx.currentTime + 1;
    const clicks = [];
    for (let i = 0; i < TAP_CLICKS; i++) {
      const t = t0 + i * 0.5;
      const osc = ctx.createOscillator();
      const g = ctx.createGain();
      osc.frequency.value = i % 4 === 0 ? 1320 : 880;
      g.gain.setValueAtTime(0.5, t);
      g.gain.exponentialRampToValueAtTime(0.001, t + 0.06);
      osc.connect(g).connect(ctx.destination);
      osc.start(t);
      osc.stop(t + 0.08);
      clicks.push(t);
    }
    tapTest = { clicks, taps: [] };
    show("tap", true);
    $("tap-start").disabled = true;
    $("tap-status").textContent = "Tap on each click.";
    $("tap").focus();
    setTimeout(finishTapTest, (t0 - ctx.currentTime + TAP_CLICKS * 0.5 + 0.5) * 1000);
  });

  function tap(e) {
    if (!tapTest) return;
    e.preventDefault();
    tapTest.taps.push(e.timeStamp);
    $("tap-status").textContent = `${tapTest.taps.length} / ${TAP_CLICKS}`;
  }
  $("tap").addEventListener("pointerdown", tap);
  document.addEventListener("keydown", (e) => {
    if (tapTest && !e.repeat && (e.key === " " || e.key === "Enter")) tap(e);
  });

  function finishTapTest() {
    const heard = tapTest.clicks.map(perfFor);
    const diffs = [];
    for (const t of tapTest.taps) {
      let best = 0;
      for (let i = 1; i < heard.length; i++) if (Math.abs(t - heard[i]) < Math.abs(t - heard[best])) best = i;
      if (best >= TAP_WARMUP && Math.abs(t - heard[best]) < 250) diffs.push(t - heard[best]);
    }
    tapTest = null;
    show("tap", false);
    $("tap-start").disabled = false;
    if (diffs.length < 6) {
      $("tap-status").textContent = "Not enough taps on the beat. Try again.";
      return;
    }
    diffs.sort((a, b) => a - b);
    const median = diffs[Math.floor(diffs.length / 2)];
    setOffset(-Math.round(median));
    $("tap-status").textContent = `Set to ${-Math.round(median)} ms.`;
  }

  const q = new URLSearchParams(location.search).get("room");
  if (q) $("code").value = q.toUpperCase().slice(0, 4);
  $("code").focus();
})();
