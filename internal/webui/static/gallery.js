// Lightbox for the gallery. Item data comes from the JSON block in the page.
(() => {
  const items = JSON.parse(document.getElementById("gallery-data").textContent || "[]") || [];
  const slot = new URLSearchParams(location.search).get("slot") || "1";
  const $ = (id) => document.getElementById(id);
  const box = $("lightbox");
  let cur = -1;

  const show = (i) => {
    if (i < 0 || i >= items.length) return;
    cur = i;
    const it = items[i];
    const img = $("lb-img");
    $("lb-missing").hidden = true;
    img.hidden = false;
    img.src = `/gallery/preview?slot=${slot}&id=${it.id}&file=${it.cover}`;
    const d = new Date(it.created);
    $("lb-utc").textContent = d.toISOString().replace("T", " ").replace(/\..*/, "") + " UTC";
    $("lb-local").textContent = d.toLocaleString();
    $("lb-del-id").value = it.id;
    $("lb-files").replaceChildren(...it.files.map((f) => {
      const li = document.createElement("li");
      const a = document.createElement("a");
      a.href = "/files/get?" + new URLSearchParams({ slot, id: it.id, file: f.id, size: f.size, path: f.path });
      a.textContent = f.path;
      const s = document.createElement("span");
      s.className = "muted";
      s.textContent = " " + f.size + " B";
      li.append(a, s);
      return li;
    }));
    $("lb-prev").disabled = i === 0;
    $("lb-next").disabled = i === items.length - 1;
  };
  const close = () => { box.hidden = true; $("lb-img").removeAttribute("src"); cur = -1; };

  $("lb-img").addEventListener("error", () => {
    $("lb-img").hidden = true;
    $("lb-missing").hidden = false;
  });
  document.querySelectorAll(".g-card").forEach((b) => b.addEventListener("click", () => {
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
