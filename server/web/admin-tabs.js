(() => {
  const tabs = document.querySelectorAll("[data-tab]");
  const panels = document.querySelectorAll("[data-panel]");

  function show() {
    const wanted = location.hash.slice(1);
    const active = [...panels].some((p) => p.dataset.panel === wanted) ? wanted : "ink";
    panels.forEach((p) => { p.hidden = p.dataset.panel !== active; });
    tabs.forEach((t) => {
      if (t.dataset.tab === active) t.setAttribute("aria-current", "page");
      else t.removeAttribute("aria-current");
    });
  }

  window.addEventListener("hashchange", show);
  show();
})();
