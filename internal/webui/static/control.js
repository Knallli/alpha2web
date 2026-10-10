// Settings UI for /control: renders the property list from /control/props,
// applies changes through /actions/setprop and keeps rows current by polling.
(() => {
  const root = document.getElementById("control");
  const csrf = root.dataset.csrf;
  const $ = (id) => document.getElementById(id);
  const host = $("props");
  const rows = new Map(); // code -> {el, p, ctl, status, busy}
  let version = null, connected = true, dirty = false;

  const el = (tag, cls, text) => {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  };
  const show = (p, v) => {
    if (p.scale) return String(v / p.scale) + (p.unit ? " " + p.unit : "");
    return String(v) + (p.unit ? " " + p.unit : "");
  };
  const optLabel = (p, v) => (p.options.find((o) => o.value === v) || { label: p.display }).label;

  // Shutter and movie: post in the background and report in the status line.
  const actStatus = $("act-status");
  document.addEventListener("submit", async (e) => {
    const f = e.target;
    if (e.defaultPrevented || !f.hasAttribute("data-act")) return;
    e.preventDefault();
    actStatus.textContent = "Working…";
    try {
      const r = await fetch(f.action, { method: "POST", body: new URLSearchParams(new FormData(f)), headers: { Accept: "application/json" }, credentials: "same-origin" });
      const j = await r.json();
      actStatus.textContent = j.ok ? "Done: " + j.msg : j.error || "HTTP " + r.status;
    } catch (err) {
      actStatus.textContent = "Cannot reach alpha2web: " + err.message;
    }
  });

  const gate = () => ({
    undoc: $("f-unlock").checked,
    danger: $("f-danger").checked,
  });

  // lock decides whether the row's widget can be used right now.
  const lock = (r) => {
    const g = gate();
    let why = r.p.disabled || "";
    if (r.p.widget !== "readonly") {
      if (!connected) why = "Camera not connected";
      else if (!r.p.documented && !g.undoc) why = "Enable editing of undocumented settings to change this";
      else if (r.p.danger !== "none" && !g.danger) why = "Enable dangerous actions to change this";
    }
    r.ctl.forEach((c) => { c.disabled = why !== ""; });
    r.reason.textContent = why;
  };

  const setWidget = (r) => {
    const p = r.p, [a, b] = r.ctl;
    if (p.widget === "switch") {
      a.checked = p.current === Math.max(...p.options.map((o) => o.value));
      r.out.textContent = optLabel(p, p.current);
    } else if (p.widget === "select") {
      if (![...a.options].some((o) => o.value === String(p.current))) {
        a.add(new Option(p.display, p.current), 0);
      }
      a.value = String(p.current);
    } else if (p.widget === "slider") {
      a.value = b.value = p.current;
      r.out.textContent = show(p, p.current);
    } else if (p.widget === "number") {
      a.value = p.current;
    }
  };

  const setStatus = (r, text, bad) => {
    r.status.textContent = text;
    r.status.classList.toggle("bad", !!bad);
  };

  const apply = async (r, raw) => {
    const p = r.p;
    if (p.danger !== "none" && !confirm(p.name + ": " + p.note + "\nChange it anyway?")) {
      setWidget(r);
      return;
    }
    const body = new URLSearchParams({ csrf, code: p.code, value: raw });
    if (!p.documented) body.set("unlock", "1");
    if (p.danger !== "none") body.set("confirm", "1");
    r.busy = true;
    setStatus(r, "Saving…");
    try {
      const resp = await fetch("/actions/setprop", { method: "POST", body, headers: { Accept: "application/json" }, credentials: "same-origin" });
      const j = await resp.json();
      if (j.ok) {
        r.p = j.prop;
        setStatus(r, "Saved");
      } else {
        setStatus(r, j.error || "HTTP " + resp.status, true);
      }
    } catch (err) {
      setStatus(r, "Cannot reach alpha2web: " + err.message, true);
    }
    r.busy = false;
    setWidget(r); // on error this puts the last known value back
    lock(r);
  };

  const build = (p) => {
    const row = el("div", "prop");
    row.dataset.code = p.code;
    const head = el("div", "p-head");
    const id = "w" + p.code;
    const title = el(p.widget === "readonly" ? "strong" : "label", null, p.name);
    if (p.widget !== "readonly") title.htmlFor = id;
    head.append(title, " ", el("code", null, p.codeHex));
    if (p.danger !== "none") {
      head.append(" ", el("span", "badge d-" + p.danger, p.danger === "danger" ? "Danger" : "Caution"));
    }
    row.append(head, el("p", "muted p-desc", p.desc));
    if (p.danger !== "none") row.append(el("p", "p-note d-" + p.danger, p.note));
    const r = { el: row, p, ctl: [], status: el("span", "muted p-status"), reason: el("span", "muted p-reason"), out: el("span", "p-out"), busy: false };
    r.status.setAttribute("role", "status");
    r.status.setAttribute("aria-live", "polite");
    const line = el("div", "p-ctl");
    if (p.widget === "switch") {
      const c = el("input");
      c.type = "checkbox"; c.id = id;
      c.setAttribute("role", "switch");
      const on = Math.max(...p.options.map((o) => o.value));
      const off = Math.min(...p.options.map((o) => o.value));
      c.addEventListener("change", () => apply(r, c.checked ? on : off));
      r.ctl = [c];
      line.append(c, " ", r.out);
    } else if (p.widget === "select") {
      const s = el("select");
      s.id = id;
      p.options.forEach((o) => s.add(new Option(o.label, o.value)));
      s.addEventListener("change", () => apply(r, s.value));
      r.ctl = [s];
      line.append(s);
    } else if (p.widget === "slider") {
      const s = el("input", "p-range"), n = el("input", "p-num");
      s.type = "range"; n.type = "number";
      s.id = id;
      for (const x of [s, n]) { x.min = p.min; x.max = p.max; x.step = p.step; }
      s.addEventListener("input", () => { n.value = s.value; r.out.textContent = show(p, Number(s.value)); });
      s.addEventListener("change", () => apply(r, s.value));
      n.addEventListener("change", () => { s.value = n.value; apply(r, n.value); });
      n.setAttribute("aria-label", p.name + " (number)");
      r.ctl = [s, n];
      line.append(s, n, " ", r.out);
    } else if (p.widget === "number") {
      const n = el("input", "p-num");
      n.type = "number"; n.id = id;
      n.addEventListener("change", () => apply(r, n.value));
      r.ctl = [n];
      line.append(n);
    } else {
      line.append(el("span", "p-value", p.display));
    }
    line.append(" ", r.status);
    row.append(line, r.reason);
    setWidget(r);
    lock(r);
    return r;
  };

  // sig is what forces a widget rebuild; a plain value change only moves it.
  const sig = (p) => JSON.stringify([p.widget, p.options, p.min, p.max, p.step, p.disabled, p.danger, p.documented]);

  const busyNow = (r) => r.busy || r.el.contains(document.activeElement);

  const buildAll = (ps) => {
    rows.clear();
    const groups = new Map();
    for (const p of ps) {
      if (!groups.has(p.group)) {
        const card = el("section", "card");
        card.append(el("h2", null, p.group));
        groups.set(p.group, card);
      }
      const r = build(p);
      rows.set(p.code, r);
      groups.get(p.group).append(r.el);
    }
    host.replaceChildren(...groups.values());
    filter();
  };

  const update = (ps) => {
    const same = ps.length === rows.size && ps.every((p) => rows.has(p.code));
    if (!same) {
      if ([...rows.values()].some(busyNow)) { dirty = true; return; }
      buildAll(ps);
      return;
    }
    for (const p of ps) {
      const r = rows.get(p.code);
      if (JSON.stringify(r.p) === JSON.stringify(p)) continue;
      if (busyNow(r)) { dirty = true; continue; }
      if (sig(r.p) !== sig(p)) {
        const n = build(p);
        n.el.hidden = r.el.hidden;
        r.el.replaceWith(n.el);
        rows.set(p.code, n);
      } else {
        r.p = p;
        setWidget(r);
        lock(r);
      }
    }
  };

  function filter() {
    const q = $("q").value.trim().toLowerCase();
    const onlyChange = $("f-change").checked, undoc = $("f-undoc").checked || $("f-unlock").checked;
    for (const r of rows.values()) {
      const p = r.p;
      const hay = (p.name + " " + p.desc + " " + p.codeHex).toLowerCase();
      r.el.hidden = (q && !hay.includes(q)) || (onlyChange && p.widget === "readonly") || (!p.documented && !undoc);
    }
    for (const card of host.children) {
      card.hidden = ![...card.querySelectorAll(".prop")].some((e) => !e.hidden);
    }
  }

  const refresh = () => rows.forEach(lock);
  for (const id of ["q", "f-change", "f-undoc"]) $(id).addEventListener("input", filter);
  $("f-unlock").addEventListener("change", () => {
    $("w-unlock").hidden = !$("f-unlock").checked;
    filter();
    refresh();
  });
  $("f-danger").addEventListener("change", refresh);

  const poll = async () => {
    if (document.hidden) return;
    try {
      const url = "/control/props" + (version == null || dirty ? "" : "?since=" + version);
      const r = await fetch(url, { cache: "no-store", credentials: "same-origin" });
      if (r.status === 204) return;
      if (!r.ok) throw new Error("HTTP " + r.status);
      const j = await r.json();
      connected = j.connected;
      $("conn").textContent = connected ? "" : j.reason || "Camera reconnecting…";
      dirty = false;
      if (version == null) buildAll(j.props); else update(j.props);
      if (!dirty) version = j.version;
      refresh();
    } catch (err) {
      $("conn").textContent = "Cannot reach alpha2web: " + err.message;
    }
  };

  $("toolbar").hidden = false;
  poll();
  setInterval(poll, 2000);
})();
