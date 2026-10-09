// Refreshes elements marked with data-refresh="<url>" (server-rendered HTML fragments).
document.querySelectorAll("[data-refresh]").forEach((el) => {
  const every = Number(el.dataset.every) || 3000;
  const tick = async () => {
    if (document.hidden) return;
    try {
      const r = await fetch(el.dataset.refresh, { cache: "no-store", credentials: "same-origin" });
      if (!r.ok) return;
      const atBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 20;
      el.innerHTML = await r.text();
      if (el.dataset.scroll && atBottom) el.scrollTop = el.scrollHeight;
    } catch (_) { /* offline; try again next tick */ }
  };
  if (el.dataset.scroll) el.scrollTop = el.scrollHeight;
  setInterval(tick, every);
});
