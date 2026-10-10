// Shows the stream state and reloads the image when the stream ends.
const img = document.getElementById("live");
const line = document.getElementById("live-status");
let failed = 0;

img.addEventListener("load", () => { failed = 0; });
img.addEventListener("error", () => {
  failed++;
  const wait = Math.min(1000 + 500 * failed, 3000);
  setTimeout(() => { img.src = "/liveview.mjpeg?t=" + Date.now(); }, wait);
});

let last = -1, still = 0;
const poll = async () => {
  if (document.hidden) return;
  let s;
  try {
    const r = await fetch("/liveview/status", { cache: "no-store", credentials: "same-origin" });
    if (!r.ok) throw new Error("HTTP " + r.status);
    s = await r.json();
  } catch (e) {
    line.textContent = "Cannot reach alpha2web: " + e.message;
    return;
  }
  still = s.frames === last ? still + 1 : 0;
  last = s.frames;
  if (s.error) line.textContent = s.error;
  else if (!s.connected && s.reconnecting) line.textContent = (s.lastDropReason || "Connection to camera was closed.") + " Reconnecting…";
  else if (!s.connected) line.textContent = "Connecting…";
  else if (failed >= 3) line.textContent = "Cannot open the stream (is live view open in another tab?). Retrying…";
  else if (s.frames > 0 && still < 3) line.textContent = Math.round(s.fps) + " fps";
  else line.textContent = "Waiting for frames…";
};
setInterval(poll, 1000);
poll();
