(() => {
  const btn = document.getElementById("theme");
  if (!btn) return;
  const root = document.documentElement;
  const label = () => { btn.textContent = root.dataset.theme === "light" ? "[ dark ]" : "[ light ]"; };
  label();
  btn.addEventListener("click", () => {
    const next = root.dataset.theme === "light" ? "dark" : "light";
    if (next === "light") root.dataset.theme = "light"; else delete root.dataset.theme;
    try { localStorage.setItem("theme", next); } catch (e) {}
    label();
  });
})();
