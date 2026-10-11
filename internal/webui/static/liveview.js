// Starts and stops the stream on button presses (the page itself never
// touches the camera), shows its state and reloads the image when it ends.
const img = document.getElementById("live");
const line = document.getElementById("live-status");
const startBtn = document.getElementById("live-start");
const stopBtn = document.getElementById("live-stop");
const snapBtn = document.getElementById("live-snap");
const fullBtn = document.getElementById("live-full");
const idle = "Press Start to connect to the camera.";
let failed = 0, running = false, retry = 0;

const show = (on) => {
  running = on;
  startBtn.hidden = on;
  stopBtn.hidden = !on;
  snapBtn.hidden = !on;
  fullBtn.hidden = !on;
  img.hidden = !on;
};
startBtn.addEventListener("click", () => {
  failed = 0;
  show(true);
  line.textContent = "Connecting…";
  img.src = "/liveview.mjpeg?t=" + Date.now();
});
stopBtn.addEventListener("click", () => {
  clearTimeout(retry);
  show(false);
  img.removeAttribute("src"); // ends the stream request
  line.textContent = idle;
});

img.addEventListener("load", () => { failed = 0; });
img.addEventListener("error", () => {
  if (!running) return;
  failed++;
  const wait = Math.min(1000 + 500 * failed, 3000);
  retry = setTimeout(() => { img.src = "/liveview.mjpeg?t=" + Date.now(); }, wait);
});

let last = -1, still = 0;
const poll = async () => {
  if (document.hidden || !running) return;
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

// Save the frame currently shown as a JPEG (the stream is same-origin, so the canvas is not tainted).
snapBtn.addEventListener("click", () => {
  if (!img.naturalWidth) return;
  const c = document.createElement("canvas");
  c.width = img.naturalWidth;
  c.height = img.naturalHeight;
  c.getContext("2d").drawImage(img, 0, 0);
  c.toBlob((b) => {
    const a = document.createElement("a");
    a.href = URL.createObjectURL(b);
    a.download = "liveview-" + new Date().toISOString().replace(/[:.]/g, "-") + ".jpg";
    a.click();
    URL.revokeObjectURL(a.href);
  }, "image/jpeg", 0.92);
});
fullBtn.addEventListener("click", () => document.getElementById("live-wrap").requestFullscreen());
