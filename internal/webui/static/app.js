/* Tg-WebDAV Web UI — vanilla JS, no build step. */
"use strict";

const $ = (id) => document.getElementById(id);

const state = {
  path: "/",
  entries: [],
  uploading: false,
};

const VIDEO_EXT = new Set(["mp4", "m4v", "webm", "mkv", "mov", "avi", "ts"]);
const AUDIO_EXT = new Set(["mp3", "m4a", "aac", "ogg", "oga", "opus", "flac", "wav"]);
const IMAGE_EXT = new Set(["jpg", "jpeg", "png", "webp", "gif", "bmp", "svg", "avif"]);

/* ── helpers ─────────────────────────────────────────────────────────── */

function joinPath(dir, name) {
  return (dir === "/" ? "" : dir) + "/" + name;
}

function davURL(p) {
  return "/dav" + p.split("/").map(encodeURIComponent).join("/");
}

function ext(name) {
  const i = name.lastIndexOf(".");
  return i > 0 ? name.slice(i + 1).toLowerCase() : "";
}

function kind(name, isDir) {
  if (isDir) return "dir";
  const e = ext(name);
  if (VIDEO_EXT.has(e)) return "video";
  if (AUDIO_EXT.has(e)) return "audio";
  if (IMAGE_EXT.has(e)) return "image";
  return "file";
}

const ICONS = { dir: "📁", video: "🎬", audio: "🎵", image: "🖼️", file: "📄" };

function fmtSize(n) {
  if (n === 0) return "0 B";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  const i = Math.min(units.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
  return (n / Math.pow(1024, i)).toFixed(i === 0 ? 0 : 1) + " " + units[i];
}

function fmtTime(unix) {
  if (!unix) return "—";
  const d = new Date(unix * 1000);
  const now = new Date();
  const sameYear = d.getFullYear() === now.getFullYear();
  const date = d.toLocaleDateString(undefined, sameYear
    ? { month: "short", day: "numeric" }
    : { year: "numeric", month: "short", day: "numeric" });
  return date + " " + d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
}

async function api(path, opts = {}) {
  const res = await fetch("/web/api/" + path, Object.assign({
    headers: { "Content-Type": "application/json" },
  }, opts));
  if (res.status === 401) {
    showLogin();
    throw new Error("session expired");
  }
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || ("HTTP " + res.status));
  return body;
}

/* ── auth ────────────────────────────────────────────────────────────── */

function showLogin() {
  $("login").classList.remove("hidden");
  $("app").classList.add("hidden");
  $("player").classList.add("hidden");
}

function showApp() {
  $("login").classList.add("hidden");
  $("app").classList.remove("hidden");
  refresh();
}

$("login-form").addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const errEl = $("login-error");
  errEl.classList.add("hidden");
  try {
    await fetch("/web/api/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username: $("login-user").value, password: $("login-pass").value }),
    }).then(async (res) => {
      if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || "Login failed");
    });
    $("login-pass").value = "";
    showApp();
  } catch (err) {
    errEl.textContent = err.message;
    errEl.classList.remove("hidden");
  }
});

$("btn-logout").addEventListener("click", async () => {
  try { await api("logout", { method: "POST" }); } catch (_) {}
  showLogin();
});

/* ── browser ─────────────────────────────────────────────────────────── */

async function refresh() {
  try {
    const data = await api("ls?path=" + encodeURIComponent(state.path));
    state.entries = data.entries || [];
    render();
  } catch (err) {
    alert(err.message);
  }
}

function render() {
  renderCrumbs();
  const tbody = $("entries");
  tbody.textContent = "";
  $("empty").classList.toggle("hidden", state.entries.length > 0);

  for (const e of state.entries) {
    const tr = document.createElement("tr");
    const k = kind(e.name, e.is_dir);
    const full = joinPath(state.path, e.name);

    const tdName = document.createElement("td");
    tdName.className = "c-name";
    tdName.innerHTML =
      '<div class="name-cell"><span class="icon">' + ICONS[k] + '</span>' +
      '<span class="label"></span></div>';
    tdName.querySelector(".label").textContent = e.name;

    const tdSize = document.createElement("td");
    tdSize.className = "c-size";
    tdSize.textContent = e.is_dir ? "—" : fmtSize(e.size);

    const tdTime = document.createElement("td");
    tdTime.className = "c-mtime";
    tdTime.textContent = fmtTime(e.mtime);

    const tdAct = document.createElement("td");
    tdAct.className = "c-act";
    if (!e.is_dir) {
      tdAct.appendChild(rowBtn("⬇", "Download", (ev) => { ev.stopPropagation(); download(full); }));
    }
    tdAct.appendChild(rowBtn("✏️", "Rename", (ev) => { ev.stopPropagation(); renameEntry(e); }));
    tdAct.appendChild(rowBtn("🗑", "Delete", (ev) => { ev.stopPropagation(); deleteEntry(e); }, true));

    tr.append(tdName, tdSize, tdTime, tdAct);
    tr.addEventListener("click", () => openEntry(e, full));
    tbody.appendChild(tr);
  }
}

function rowBtn(glyph, title, onClick, danger) {
  const b = document.createElement("button");
  b.className = "row-btn" + (danger ? " danger" : "");
  b.title = title;
  b.textContent = glyph;
  b.addEventListener("click", onClick);
  return b;
}

function renderCrumbs() {
  const nav = $("crumbs");
  nav.textContent = "";
  const parts = state.path.split("/").filter(Boolean);
  nav.appendChild(crumb("🏠 Root", "/", parts.length === 0));
  let acc = "";
  parts.forEach((p, i) => {
    acc += "/" + p;
    const sep = document.createElement("span");
    sep.className = "crumb-sep";
    sep.textContent = "›";
    nav.appendChild(sep);
    nav.appendChild(crumb(p, acc, i === parts.length - 1));
  });
}

function crumb(label, path, current) {
  const a = document.createElement("a");
  a.className = "crumb" + (current ? " current" : "");
  a.textContent = label;
  a.href = "#";
  a.title = label;
  a.addEventListener("click", (ev) => { ev.preventDefault(); navigate(path); });
  return a;
}

function navigate(path) {
  state.path = path === "" ? "/" : path;
  refresh();
}

function openEntry(e, full) {
  if (e.is_dir) return navigate(full);
  const k = kind(e.name, false);
  if (k === "video" || k === "audio" || k === "image") return openPlayer(e, full, k);
  download(full);
}

function download(full) {
  const a = document.createElement("a");
  a.href = davURL(full);
  a.download = full.split("/").pop();
  document.body.appendChild(a);
  a.click();
  a.remove();
}

async function renameEntry(e) {
  const to = prompt("New name:", e.name);
  if (!to || to === e.name) return;
  try {
    await api("rename", { method: "POST", body: JSON.stringify({ from: joinPath(state.path, e.name), to: joinPath(state.path, to) }) });
    refresh();
  } catch (err) { alert(err.message); }
}

async function deleteEntry(e) {
  const full = joinPath(state.path, e.name);
  if (!confirm(`Delete ${e.is_dir ? "folder" : "file"} "${e.name}"? This also removes it from Telegram.`)) return;
  try {
    await api("delete", { method: "POST", body: JSON.stringify({ path: full }) });
    refresh();
  } catch (err) { alert(err.message); }
}

$("btn-mkdir").addEventListener("click", async () => {
  const name = prompt("Folder name:");
  if (!name) return;
  try {
    await api("mkdir", { method: "POST", body: JSON.stringify({ path: joinPath(state.path, name.trim()) }) });
    refresh();
  } catch (err) { alert(err.message); }
});

/* ── uploads ─────────────────────────────────────────────────────────── */

const uploadQueue = [];

$("btn-upload").addEventListener("click", () => $("file-picker").click());
$("file-picker").addEventListener("change", (ev) => {
  queueFiles([...ev.target.files]);
  ev.target.value = "";
});

const dropzone = $("dropzone");
["dragenter", "dragover"].forEach((t) =>
  dropzone.addEventListener(t, (ev) => {
    ev.preventDefault();
    dropzone.classList.add("drag");
    $("drop-hint").classList.remove("hidden");
  }));
["dragleave", "drop"].forEach((t) =>
  dropzone.addEventListener(t, (ev) => {
    ev.preventDefault();
    if (t === "drop" || ev.target === dropzone) {
      dropzone.classList.remove("drag");
      $("drop-hint").classList.add("hidden");
    }
  }));
dropzone.addEventListener("drop", (ev) => {
  ev.preventDefault();
  dropzone.classList.remove("drag");
  $("drop-hint").classList.add("hidden");
  queueFiles([...ev.dataTransfer.files]);
});

function queueFiles(files) {
  for (const f of files) uploadQueue.push(f);
  pumpUploads();
}

async function pumpUploads() {
  if (state.uploading) return;
  const file = uploadQueue.shift();
  if (!file) return;
  state.uploading = true;
  $("upload-progress").classList.remove("hidden");
  try {
    await uploadOne(file);
    refresh();
  } catch (err) {
    alert(`Upload of "${file.name}" failed: ${err.message}`);
  }
  state.uploading = false;
  if (uploadQueue.length) return pumpUploads();
  $("upload-progress").classList.add("hidden");
}

function uploadOne(file) {
  return new Promise((resolve, reject) => {
    const url = "/web/api/upload?path=" + encodeURIComponent(state.path) + "&name=" + encodeURIComponent(file.name);
    const xhr = new XMLHttpRequest();
    xhr.open("PUT", url);
    xhr.setRequestHeader("Content-Type", "application/octet-stream");
    xhr.upload.addEventListener("progress", (ev) => {
      if (!ev.lengthComputable) return;
      const pct = Math.round((ev.loaded / ev.total) * 100);
      $("upload-bar").style.width = pct + "%";
      $("upload-label").textContent = `${file.name} — ${pct}% of ${fmtSize(file.size)}`;
    });
    xhr.addEventListener("load", () => {
      if (xhr.status >= 200 && xhr.status < 300) return resolve();
      let msg = "HTTP " + xhr.status;
      try { msg = JSON.parse(xhr.responseText).error || msg; } catch (_) {}
      reject(new Error(msg));
    });
    xhr.addEventListener("error", () => reject(new Error("network error")));
    xhr.send(file);
  });
}

/* ── player ──────────────────────────────────────────────────────────── */

function openPlayer(e, full, k) {
  const stage = $("player-stage");
  stage.textContent = "";
  $("player-error").classList.add("hidden");
  $("player-title").textContent = e.name;
  $("player-download").href = davURL(full);

  const url = davURL(full);
  let el;
  if (k === "video") {
    el = document.createElement("video");
    el.controls = true;
    el.autoplay = true;
    el.playsInline = true;
    el.preload = "auto";
    el.src = url; // cookie auth + HTTP 206 → smooth seeking
  } else if (k === "audio") {
    el = document.createElement("audio");
    el.controls = true;
    el.autoplay = true;
    el.preload = "auto";
    el.src = url;
  } else {
    el = document.createElement("img");
    el.src = url;
    el.alt = e.name;
  }
  el.addEventListener("error", () => {
    const d = document.createElement("div");
    d.className = "player-error";
    d.textContent = "Playback failed — the format may be unsupported by this browser. Try downloading instead.";
    stage.replaceChildren(d);
  });
  stage.appendChild(el);
  $("player").classList.remove("hidden");
}

function closePlayer() {
  const stage = $("player-stage");
  stage.querySelectorAll("video,audio").forEach((m) => { m.pause(); m.removeAttribute("src"); m.load(); });
  stage.textContent = "";
  $("player").classList.add("hidden");
}

$("player").addEventListener("click", (ev) => {
  if (ev.target.dataset.close !== undefined || ev.target.classList.contains("modal-backdrop")) closePlayer();
});
document.addEventListener("keydown", (ev) => {
  if (ev.key === "Escape" && !$("player").classList.contains("hidden")) closePlayer();
});

/* ── boot ────────────────────────────────────────────────────────────── */

(async function boot() {
  try {
    const s = await fetch("/web/api/session").then((r) => r.json());
    s.authenticated ? showApp() : showLogin();
  } catch (_) {
    showLogin();
  }
})();
