// Gallery: selection, background jobs and the lightbox. Item data comes from the JSON blocks in the page.
(() => {
  const root = document.getElementById("gallery");
  const { csrf, slot } = root.dataset;
  const json = (id) => JSON.parse(document.getElementById(id).textContent || "[]") || [];
  const items = json("gallery-data");
  const allKeys = json("gallery-keys");
  const $ = (id) => document.getElementById(id);
  const box = $("lightbox");

  // selection, kept per slot across pages (sessionStorage is optional)
  const skey = "gal-sel-" + slot;
  let sel = new Set();
  try { sel = new Set(JSON.parse(sessionStorage.getItem(skey) || "[]")); } catch (_) { /* no storage */ }
  const boxes = [...document.querySelectorAll(".g-sel")];
  let running = false;
  const sync = () => {
    boxes.forEach((b) => { b.checked = sel.has(b.value); b.closest(".g-card").classList.toggle("sel", b.checked); });
    try { sessionStorage.setItem(skey, JSON.stringify([...sel])); } catch (_) { /* no storage */ }
    if ($("g-pull")) {
      $("g-pull").textContent = `Pull selected (${sel.size})`;
      $("g-pull").disabled = running || sel.size === 0;
    }
    $("g-refresh").disabled = running;
  };
  boxes.forEach((b) => b.addEventListener("change", () => { b.checked ? sel.add(b.value) : sel.delete(b.value); sync(); }));
  const on = (id, fn) => { if ($(id)) $(id).addEventListener("click", fn); };
  on("g-page", () => { boxes.forEach((b) => sel.add(b.value)); sync(); });
  on("g-all", () => { allKeys.forEach((k) => sel.add(k)); sync(); });
  on("g-none", () => { sel.clear(); sync(); });

  // jobs
  const post = (url, body = new URLSearchParams()) => {
    body.set("csrf", csrf);
    return fetch(url, { method: "POST", credentials: "same-origin", body });
  };
  let wasRunning = false, timer = 0;
  const poll = async () => {
    clearTimeout(timer);
    let j;
    try { j = await (await fetch("/gallery/job", { cache: "no-store", credentials: "same-origin" })).json(); } catch (_) { timer = setTimeout(poll, 3000); return; }
    running = !!j.kind && !j.finished;
    sync();
    $("job").hidden = !running;
    if (running) {
      wasRunning = true;
      $("job-title").textContent = (j.kind === "pull" ? "Pulling" : "Refreshing") + (j.total ? ` ${j.done} / ${j.total}` : "");
      const bar = $("job-bar");
      if (j.total) { bar.max = j.total; bar.value = j.done; } else bar.removeAttribute("value");
      $("job-cur").textContent = j.current;
      timer = setTimeout(poll, 1000);
    } else if (wasRunning) location.reload(); // the page then shows the result
  };
  const start = async (url, body) => {
    const r = await post(url, body);
    if (!r.ok) { $("job-err").textContent = (await r.text()).trim(); $("job-err").hidden = false; return; }
    $("job-err").hidden = true;
    wasRunning = true;
    poll();
  };
  on("g-refresh", () => start("/gallery/refresh"));
  on("g-pull", () => {
    const body = new URLSearchParams({ slot });
    sel.forEach((k) => body.append("k", k));
    start("/gallery/pull", body);
  });
  on("job-stop", () => post("/gallery/job/stop"));
  sync();
  poll();

  // thumbnails the cache does not have: neutral placeholder, no retry
  const miss = (img) => img.classList.add("missing");
  document.querySelectorAll(".g-card img").forEach((img) => {
    img.addEventListener("error", () => miss(img));
    if (img.complete && img.naturalWidth === 0 && img.currentSrc) miss(img);
  });

  // lightbox
  let cur = -1;
  const img = $("lb-img");
  const show = (i) => {
    if (i < 0 || i >= items.length) return;
    cur = i;
    const it = items[i];
    $("lb-missing").hidden = true;
    $("lb-loading").hidden = false;
    img.hidden = true;
    img.src = `/gallery/preview?slot=${slot}&k=${it.key}`;
    const d = new Date(it.created);
    $("lb-utc").textContent = d.toISOString().replace("T", " ").replace(/\..*/, "") + " UTC";
    $("lb-local").textContent = d.toLocaleString();
    $("lb-del-key").value = it.key;
    $("lb-files").replaceChildren(...it.files.map((f, n) => {
      const li = document.createElement("li");
      const a = document.createElement("a");
      a.href = `/gallery/file?slot=${slot}&k=${it.key}&f=${n}`;
      a.textContent = f.path;
      const s = document.createElement("span");
      s.className = "muted";
      s.textContent = " " + f.size + " B" + (f.pulled ? " · pulled" : "");
      li.append(a, s);
      return li;
    }));
    $("lb-prev").disabled = i === 0;
    $("lb-next").disabled = i === items.length - 1;
  };
  const close = () => { box.hidden = true; img.removeAttribute("src"); cur = -1; };

  img.addEventListener("load", () => { img.hidden = false; $("lb-loading").hidden = true; });
  img.addEventListener("error", async () => {
    const u = img.getAttribute("src");
    if (!u) return;
    $("lb-loading").hidden = true;
    img.hidden = true;
    const m = $("lb-missing");
    m.textContent = "No preview available.";
    m.hidden = false;
    try { // the server explains a camera refusal in the response body
      const r = await fetch(u, { credentials: "same-origin" });
      const t = (await r.text()).trim();
      if (r.status >= 500 && t.length < 300 && img.getAttribute("src") === u) m.textContent = t;
    } catch (_) { /* keep the generic text */ }
  });
  document.querySelectorAll(".g-open").forEach((b) => b.addEventListener("click", () => {
    box.hidden = false;
    show(Number(b.dataset.i));
  }));
  $("lb-prev").addEventListener("click", () => show(cur - 1));
  $("lb-next").addEventListener("click", () => show(cur + 1));
  $("lb-close").addEventListener("click", close);
  box.addEventListener("click", (e) => { if (e.target === box) close(); });
  document.addEventListener("keydown", (e) => {
    if (box.hidden) return;
    if (e.key === "Escape") close();
    else if (e.key === "ArrowLeft") show(cur - 1);
    else if (e.key === "ArrowRight") show(cur + 1);
  });
})();
