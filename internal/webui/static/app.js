// Shared page behaviour: fragment refresh, theme, filters and confirm dialogs.
// All wiring is data-attribute based (no inline handlers: CSP).

// Elements marked data-refresh="<url>" are re-rendered from server HTML fragments.
const filters = [];
document.querySelectorAll("[data-refresh]").forEach((el) => {
  const every = Number(el.dataset.every) || 3000;
  const follow = () => { const f = document.getElementById("lf-follow"); return !f || f.checked; };
  const tick = async () => {
    if (document.hidden) return;
    try {
      const r = await fetch(el.dataset.refresh, { cache: "no-store", credentials: "same-origin" });
      if (!r.ok) return;
      const atBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 20;
      el.innerHTML = await r.text();
      filters.forEach((f) => f());
      if (el.dataset.scroll && atBottom && follow()) el.scrollTop = el.scrollHeight;
    } catch (_) { /* offline; try again next tick */ }
  };
  if (el.dataset.scroll) el.scrollTop = el.scrollHeight;
  setInterval(tick, every);
});

// Live text filter: <input data-filter="css selector of the rows">.
document.querySelectorAll("input[data-filter]").forEach((inp) => {
  const run = () => {
    const q = inp.value.trim().toLowerCase();
    document.querySelectorAll(inp.dataset.filter).forEach((row) => { row.hidden = !!q && !row.textContent.toLowerCase().includes(q); });
  };
  inp.addEventListener("input", run);
  filters.push(run);
});

// Selects that filter a server-rendered list submit on change.
document.querySelectorAll("select[data-autosubmit]").forEach((s) => s.addEventListener("change", () => s.form.submit()));

// Theme: follows the system until the button is used; the choice is remembered (storage is optional).
const root = document.documentElement;
try { const t = localStorage.getItem("theme"); if (t) root.dataset.theme = t; } catch (_) { /* no storage */ }
const themeBtn = document.getElementById("theme");
if (themeBtn) themeBtn.addEventListener("click", () => {
  const dark = root.dataset.theme ? root.dataset.theme === "dark" : matchMedia("(prefers-color-scheme: dark)").matches;
  root.dataset.theme = dark ? "light" : "dark";
  try { localStorage.setItem("theme", root.dataset.theme); } catch (_) { /* no storage */ }
});

// Forms with data-confirm ask first.
document.addEventListener("submit", (e) => {
  const msg = e.target.dataset && e.target.dataset.confirm;
  if (msg && !confirm(msg)) e.preventDefault();
});

// Keyboard: "/" focuses the first search box on the page.
document.addEventListener("keydown", (e) => {
  if (e.key !== "/" || e.target.closest("input,select,textarea")) return;
  const q = document.querySelector("input[type=search]");
  if (q) { e.preventDefault(); q.focus(); }
});
