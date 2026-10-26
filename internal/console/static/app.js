// The plainbucket console. Every action is an S3 request signed in the page with the key
// typed at login; the key stays in memory only, so a reload signs out.
import { canonicalKey, displayKey, encodeKey, escapeXML, keyBytes, keyText, xmlKey } from "./keys.js";
import { encodePath, presign, queryString, signHeaders } from "./sigv4.js";
import { ZipBuilder, crc32, entryName } from "./zip.js";

const host = location.host;
const region = "us-east-1";
const minPartSize = 16 * 1024 * 1024;
const maxParts = 10000;
const partConcurrency = 3;
const partAttempts = 3;
const shareUnits = [["minutes", 60], ["hours", 3600], ["days", 86400]];
// SigV4 caps a presigned URL at seven days.
const maxShareSeconds = 7 * 86400;

let cred = null;
// What the signed-in key may do, as the usage API reports it.
let keyInfo = { readOnly: false };
const app = document.getElementById("app");

// ---- DOM

function el(tag, props = {}, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "class") e.className = v;
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else if (k === "value") e.value = v;
    else e.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    e.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return e;
}

function button(label, onclick, cls = "") {
  return el("button", { type: "button", class: cls, onclick }, label);
}

// action is a button that stays disabled while its request runs.
function action(label, run, cls = "") {
  const b = button(label, async () => {
    if (b.disabled) return;
    b.disabled = true;
    try {
      await run();
    } finally {
      b.disabled = false;
    }
  }, cls);
  return b;
}

// ---- Theme

const themes = { auto: "Auto", light: "Light", dark: "Dark" };
const themeKey = "plainbucket-theme";

// The theme is remembered in localStorage: unlike the key, it is no secret.
function loadTheme() {
  try {
    const t = localStorage.getItem(themeKey);
    return Object.hasOwn(themes, t ?? "") ? t : "auto";
  } catch {
    return "auto";
  }
}

let theme = loadTheme();

function applyTheme() {
  if (theme === "auto") delete document.documentElement.dataset.theme;
  else document.documentElement.dataset.theme = theme;
}
applyTheme();

function themeButton() {
  const names = Object.keys(themes);
  const b = button("Theme: " + themes[theme], () => {
    theme = names[(names.indexOf(theme) + 1) % names.length];
    try {
      localStorage.setItem(themeKey, theme);
    } catch {
      // Storage may be disabled; the choice then lasts until the page is reloaded.
    }
    applyTheme();
    b.textContent = "Theme: " + themes[theme];
  }, "link");
  b.title = "Follow the system, or force the light or dark theme";
  return b;
}

function formatBytes(n) {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return (i === 0 ? n : n.toFixed(1)) + " " + units[i];
}

function plural(n, word) {
  return `${n.toLocaleString()} ${word}${n === 1 ? "" : "s"}`;
}

function formatDuration(seconds) {
  if (seconds < 60) return `${Math.ceil(seconds)} s`;
  if (seconds < 3600) return `${Math.ceil(seconds / 60)} min`;
  return `${Math.floor(seconds / 3600)} h ${Math.round((seconds % 3600) / 60)} min`;
}

// formatGoDuration shortens the server's Go durations: 336h0m0s becomes 336h.
function formatGoDuration(d) {
  return d.replace(/m0s$/, "m").replace(/h0m$/, "h");
}

function formatDate(s) {
  return s ? new Date(s).toLocaleString() : "";
}

let generation = 0;
let banner = null;

function notify(message, kind = "error") {
  banner?.remove();
  banner = el("div", { class: "banner " + kind, role: kind === "error" ? "alert" : "status" },
    el("span", {}, message), button("×", () => banner.remove(), "link"));
  app.prepend(banner);
}

// reporter returns an error reporter for the current view, which stays silent once
// another view has replaced it.
function reporter() {
  const gen = generation;
  return (err) => {
    if (gen !== generation) return;
    if (err.code === "AccessDenied") {
      notify(`${err.code}: ${err.message} This key may be limited to other buckets or read-only.`);
    } else if (err.code === "RequestTimeTooSkewed") {
      notify("The server refused the signature because this computer's clock differs from the server's by more than 15 minutes.");
    } else {
      notify(describe(err));
    }
  };
}

function describe(err) {
  return err.code ? `${err.code}: ${err.message}` : String(err.message || err);
}

// ---- S3

class S3Error extends Error {
  constructor(status, code, message) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

function xml(text) {
  return new DOMParser().parseFromString(text, "application/xml");
}

function child(node, name) {
  return [...node.children].find((c) => c.localName === name)?.textContent ?? "";
}

function children(node, name) {
  return [...node.children].filter((c) => c.localName === name);
}

async function s3Error(status, text) {
  const doc = xml(text || "<Error/>").documentElement;
  return new S3Error(status, child(doc, "Code") || String(status), child(doc, "Message") || "Request failed.");
}

// key is in encoded form (see keys.js).
function target(bucket, key, params) {
  const path = bucket === "" ? "/" : encodePath(bucket, keyBytes(key));
  return { path, query: queryString(params) };
}

// s3 sends a signed request and returns the response, throwing S3Error unless it is 2xx.
async function s3(method, bucket, key = "", { params = [], headers = {}, body, as = cred, signal } = {}) {
  const { path, query } = target(bucket, key, params);
  const res = await fetch(path + (query ? "?" + query : ""), {
    method, body, signal, headers: signHeaders(as, { method, host, path, query, headers }),
  });
  if (!res.ok) throw await s3Error(res.status, await res.text());
  return res;
}

async function s3xml(method, bucket, key, opts) {
  return xml(await (await s3(method, bucket, key, opts)).text()).documentElement;
}

function presignedURL(bucket, key, expires, params = []) {
  const path = encodePath(bucket, keyBytes(key));
  return location.origin + path + "?" + presign(cred, { method: "GET", host, path, params, expires });
}

// consoleAPI sends a signed request to one of the console's own API paths.
async function consoleAPI(method, path, params = []) {
  const query = queryString(params);
  const res = await fetch(path + (query ? "?" + query : ""), { method, headers: signHeaders(cred, { method, host, path, query }) });
  if (!res.ok) throw await s3Error(res.status, await res.text());
  return res;
}

async function consoleUsage() {
  return (await consoleAPI("GET", "/_console/api/usage")).json();
}

const trashPath = "/_console/api/trash";

// deleteKeys deletes encoded keys, in batches of DeleteObjects, and one by one those
// that XML cannot carry.
async function deleteKeys(bucket, keys) {
  const inXML = [];
  for (const k of keys) {
    const text = xmlKey(k);
    if (text === null) await s3("DELETE", bucket, k);
    else inXML.push(text);
  }
  for (let i = 0; i < inXML.length; i += 1000) {
    const body = "<Delete><Quiet>true</Quiet>" +
      inXML.slice(i, i + 1000).map((k) => `<Object><Key>${k}</Key></Object>`).join("") + "</Delete>";
    const res = await s3xml("POST", bucket, "", { params: [["delete", ""]], body });
    const errors = children(res, "Error");
    if (errors.length) throw new S3Error(200, child(errors[0], "Code"), `${errors.length} keys were not deleted: ${child(errors[0], "Message")}`);
  }
}

// listObjects returns every object under prefix: its encoded key, size and modification time.
async function listObjects(bucket, prefix) {
  const out = [];
  let token = "";
  for (;;) {
    const params = [["list-type", "2"], ["prefix", keyBytes(prefix)], ["encoding-type", "url"]];
    if (token) params.push(["continuation-token", token]);
    const res = await s3xml("GET", bucket, "", { params });
    for (const c of children(res, "Contents")) {
      out.push({ key: child(c, "Key"), size: Number(child(c, "Size")), modified: child(c, "LastModified") });
    }
    if (child(res, "IsTruncated") !== "true") return out;
    token = child(res, "NextContinuationToken");
  }
}

async function listAll(bucket, prefix) {
  return (await listObjects(bucket, prefix)).map((o) => o.key);
}

// ---- Uploads

// put sends body with XMLHttpRequest, the only browser API that reports upload progress.
function put(as, bucket, key, params, headers, body, onProgress, pending) {
  const { path, query } = target(bucket, key, params);
  const signed = signHeaders(as, { method: "PUT", host, path, query, headers });
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    pending.add(xhr);
    xhr.open("PUT", path + (query ? "?" + query : ""));
    for (const [k, v] of Object.entries(signed)) xhr.setRequestHeader(k, v);
    xhr.upload.onprogress = (e) => onProgress(e.loaded);
    xhr.onload = async () => {
      pending.delete(xhr);
      if (xhr.status >= 200 && xhr.status < 300) resolve(xhr.getResponseHeader("ETag"));
      else reject(await s3Error(xhr.status, xhr.responseText));
    };
    xhr.onerror = () => {
      pending.delete(xhr);
      reject(new Error("The upload failed: network error."));
    };
    xhr.onabort = () => {
      pending.delete(xhr);
      reject(new Error("Canceled."));
    };
    xhr.send(body);
  });
}

// Cancel functions of the uploads in progress.
const uploads = new Set();
// Size and bytes sent of each upload in progress, for the summary line.
const transfers = new Set();
const summaryText = el("span", { class: "muted" });
const uploadSummary = el("div", { class: "row" }, summaryText,
  button("Cancel all", () => { for (const stop of [...uploads]) stop(); }, "link danger"));
uploadSummary.hidden = true;
// Decisions pending for uploads refused because the key exists: true overwrites.
const conflicts = new Set();
const conflictText = el("span", { class: "muted" });
const conflictBar = el("div", { class: "row" }, conflictText,
  button("Overwrite all", () => { for (const decide of [...conflicts]) decide(true); }, "link danger"),
  button("Skip all", () => { for (const decide of [...conflicts]) decide(false); }, "link"));
conflictBar.hidden = true;
const uploadList = el("div", { class: "uploads" }, uploadSummary, conflictBar);

function showConflicts() {
  conflictBar.hidden = conflicts.size < 2;
  conflictText.textContent = `${conflicts.size} files already exist.`;
}
let rate = 0;
let lastSent = 0;
let ticker = null;

function sentTotals() {
  let size = 0;
  let sent = 0;
  for (const t of transfers) {
    size += t.size;
    sent += t.sent;
  }
  return { size, sent };
}

// rate is a moving average of bytes per second.
function tick() {
  if (!transfers.size) {
    clearInterval(ticker);
    ticker = null;
    rate = 0;
    uploadSummary.hidden = true;
    return;
  }
  const { size, sent } = sentTotals();
  rate = 0.7 * rate + 0.3 * Math.max(0, sent - lastSent);
  lastSent = sent;
  summaryText.textContent = `${plural(transfers.size, "upload")} · ${formatBytes(sent)} of ${formatBytes(size)}` +
    (rate >= 1 ? ` · ${formatBytes(rate)}/s · about ${formatDuration((size - sent) / rate)} left` : "");
  uploadSummary.hidden = false;
}

function track(t) {
  transfers.add(t);
  if (!ticker) {
    lastSent = sentTotals().sent;
    ticker = setInterval(tick, 1000);
  }
}

function untrack(t) {
  if (transfers.delete(t)) lastSent -= t.sent;
}
// Multipart upload IDs this page is still sending, which uploadsDialog must not abort.
const liveUploads = new Set();

window.addEventListener("beforeunload", (e) => {
  if (!uploads.size) return;
  e.preventDefault();
  e.returnValue = "";
});

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// upload sends file to an encoded key. It signs with the key that was signed in when it
// started, so signing out cancels it instead of leaving it to another key. Unless
// overwrite is set, the write is conditional on the key not existing.
function upload(bucket, key, file, onDone, overwrite = false) {
  const as = cred;
  const bar = el("progress", { max: file.size || 1, value: 0 });
  const status = el("span", { class: "muted" }, "starting");
  const pending = new Set();
  let canceled = false;
  const stop = () => {
    canceled = true;
    for (const x of pending) x.abort();
  };
  const cancel = button("Cancel", stop, "link");
  const row = el("div", { class: "upload" }, el("span", { class: "name" }, displayKey(key)), bar, status, cancel);
  uploadList.append(row);
  uploads.add(stop);
  const transfer = { size: file.size, sent: 0 };
  track(transfer);
  const progress = (n) => {
    bar.value = n;
    transfer.sent = n;
  };

  const finish = (text, kind) => {
    uploads.delete(stop);
    untrack(transfer);
    status.textContent = text;
    status.className = kind;
    cancel.remove();
    if (kind !== "error") setTimeout(() => row.remove(), 4000);
  };
  const failed = (err) => {
    if (err.status !== 412) {
      finish(describe(err), "error");
      row.append(button("Retry", () => {
        row.remove();
        upload(bucket, key, file, onDone, overwrite);
      }, "link"), button("Dismiss", () => row.remove(), "link"));
      return;
    }
    finish("already exists", "error");
    const decide = (yes) => {
      conflicts.delete(decide);
      showConflicts();
      row.remove();
      if (yes) upload(bucket, key, file, onDone, true);
    };
    conflicts.add(decide);
    showConflicts();
    row.append(button("Overwrite", () => decide(true), "link danger"), button("Skip", () => decide(false), "link"));
  };
  const headers = { "content-type": file.type || "application/octet-stream" };
  const guard = overwrite ? {} : { "if-none-match": "*" };

  const run = async () => {
    if (file.size <= minPartSize) {
      await put(as, bucket, key, [], { ...headers, ...guard }, file, progress, pending);
      return;
    }
    // The final write checks again; this only saves sending the parts for nothing.
    if (!overwrite && await s3("HEAD", bucket, key, { as }).then(() => true, (err) => {
      if (err.status === 404) return false;
      throw err;
    })) throw new S3Error(412, "PreconditionFailed", "An object with this name already exists.");
    const partSize = Math.max(minPartSize, Math.ceil(file.size / maxParts));
    const created = await s3xml("POST", bucket, key, { params: [["uploads", ""]], headers, as });
    const uploadId = child(created, "UploadId");
    liveUploads.add(uploadId);
    const count = Math.ceil(file.size / partSize);
    const sent = new Array(count).fill(0);
    const etags = new Array(count);
    let next = 0;
    const sendPart = async (i) => {
      const part = file.slice(i * partSize, Math.min(file.size, (i + 1) * partSize));
      const params = [["partNumber", String(i + 1)], ["uploadId", uploadId]];
      for (let attempt = 1; ; attempt++) {
        if (canceled) throw new Error("Canceled.");
        try {
          const etag = await put(as, bucket, key, params, {}, part, (n) => {
            sent[i] = n;
            progress(sent.reduce((a, b) => a + b, 0));
          }, pending);
          if (!etag) throw new Error(`The server returned no ETag for part ${i + 1}.`);
          return etag;
        } catch (err) {
          // A client error or a cancel will not go away on a retry.
          if (canceled || attempt === partAttempts || (err.status >= 400 && err.status < 500)) throw err;
          sent[i] = 0;
          await sleep(1000 * attempt);
        }
      }
    };
    const worker = async () => {
      while (next < count && !canceled) {
        const i = next++;
        etags[i] = await sendPart(i);
      }
    };
    try {
      await Promise.all(Array.from({ length: Math.min(partConcurrency, count) }, worker));
      if (canceled) throw new Error("Canceled.");
      cancel.hidden = true;
      status.textContent = "completing";
      const body = "<CompleteMultipartUpload>" +
        etags.map((e, i) => `<Part><PartNumber>${i + 1}</PartNumber><ETag>${escapeXML(e)}</ETag></Part>`).join("") +
        "</CompleteMultipartUpload>";
      await s3("POST", bucket, key, { params: [["uploadId", uploadId]], headers: guard, body, as });
    } catch (err) {
      stop();
      await s3("DELETE", bucket, key, { params: [["uploadId", uploadId]], as }).catch(() => {});
      throw err;
    } finally {
      liveUploads.delete(uploadId);
    }
  };

  status.textContent = "uploading";
  run().then(() => {
    finish("done", "ok");
    onDone();
  }, failed);
}

// droppedFiles returns [relative path, File] for everything dropped, walking folders.
async function droppedFiles(dt) {
  // Entries must be taken before the first await, while the drop's data is readable.
  const entries = [...(dt.items ?? [])].filter((i) => i.kind === "file").map((i) => i.webkitGetAsEntry?.()).filter(Boolean);
  if (!entries.length) return [...dt.files].map((f) => [f.name, f]);
  const out = [];
  const walk = async (entry, dir) => {
    if (entry.isFile) {
      out.push([dir + entry.name, await new Promise((resolve, reject) => entry.file(resolve, reject))]);
      return;
    }
    const reader = entry.createReader();
    for (;;) {
      const batch = await new Promise((resolve, reject) => reader.readEntries(resolve, reject));
      if (!batch.length) return;
      for (const e of batch) await walk(e, dir + entry.name + "/");
    }
  };
  for (const e of entries) await walk(e, "");
  return out;
}

// ---- Dialogs

function dialog(title, ...body) {
  const d = el("dialog", {}, el("h2", {}, title), ...body,
    el("div", { class: "actions" }, button("Close", () => d.close())));
  d.addEventListener("close", () => d.remove());
  document.body.append(d);
  d.showModal();
  return d;
}

function copyField(value) {
  const input = el("input", { readonly: true, value, class: "wide" });
  const copy = button("Copy", async () => {
    try {
      await navigator.clipboard.writeText(input.value);
      copy.textContent = "Copied";
    } catch {
      // The clipboard API needs a secure context; the older command works without one.
      input.select();
      copy.textContent = document.execCommand("copy") ? "Copied" : "Press Ctrl+C / ⌘C";
    }
  });
  return el("div", { class: "row" }, input, copy);
}

const previewBytes = 64 * 1024;
const textTypes = /^text\/|[/+](json|xml|yaml|x-yaml|javascript|x-sh|toml|csv)$/;

// preview shows media through a short-lived presigned URL and text as its first bytes.
// Stored pages are never rendered here.
function preview(bucket, key, type, size) {
  type = (type ?? "").split(";")[0].trim().toLowerCase();
  const src = () => presignedURL(bucket, key, 300);
  if (type.startsWith("image/")) return el("img", { src: src(), alt: displayKey(key), class: "preview" });
  if (type.startsWith("video/")) return el("video", { src: src(), controls: true, preload: "metadata", class: "preview" });
  if (type.startsWith("audio/")) return el("audio", { src: src(), controls: true, preload: "metadata", class: "preview" });
  if (!textTypes.test(type)) return null;
  const pre = el("pre", { class: "preview" }, size ? "Loading…" : "(empty)");
  if (size) {
    s3("GET", bucket, key, { headers: { range: `bytes=0-${previewBytes - 1}` } })
      .then((res) => res.arrayBuffer())
      .then((buf) => {
        pre.textContent = new TextDecoder().decode(buf) +
          (size > previewBytes ? `\n\n… the first ${formatBytes(previewBytes)} of ${formatBytes(size)}` : "");
      }, (err) => { pre.textContent = "No preview: " + describe(err); });
  }
  return pre;
}

function shellQuote(s) {
  return "'" + s.replace(/'/g, "'\\''") + "'";
}

function shareDialog(bucket, key) {
  const amount = el("input", { type: "number", min: 1, value: 1, class: "short" });
  const unit = el("select", {}, shareUnits.map(([label, s]) => el("option", { value: s, selected: s === 86400 }, label)));
  const out = el("div");
  const draw = () => {
    const seconds = Math.round(Number(amount.value) * Number(unit.value));
    if (!(seconds >= 1 && seconds <= maxShareSeconds)) {
      out.replaceChildren(el("p", { class: "error" }, "A presigned URL lasts at most seven days."));
      return;
    }
    const url = presignedURL(bucket, key, seconds);
    const name = displayKey(key.slice(key.lastIndexOf("/") + 1)) || "download";
    out.replaceChildren(
      el("p", { class: "muted" }, "Expires " + new Date(Date.now() + seconds * 1000).toLocaleString() + "."),
      el("div", { class: "field" }, "Link", copyField(url)),
      el("div", { class: "field" }, "curl", copyField(`curl -fLo ${shellQuote(name)} ${shellQuote(url)}`)));
  };
  amount.addEventListener("input", draw);
  unit.addEventListener("change", draw);
  draw();
  dialog("Share " + displayKey(key),
    el("p", { class: "muted" }, "Anyone with this link can download the object until it expires, as long as this key stays valid."),
    el("div", { class: "row" }, "Expires in", amount, unit), out);
}

// moveKeys copies each [from, to] pair without overwriting anything, then deletes the
// sources, so a failed copy leaves every source in place.
async function moveKeys(srcBucket, dstBucket, pairs, onCopied) {
  for (const [i, [from, to]] of pairs.entries()) {
    await s3("PUT", dstBucket, to, { headers: { "x-amz-copy-source": encodePath(srcBucket, keyBytes(from)), "if-none-match": "*" } });
    onCopied(i + 1);
  }
  await deleteKeys(srcBucket, pairs.map(([from]) => from));
}

// moveDialog renames or moves an object, or a folder with everything in it.
function moveDialog(bucket, key, onDone) {
  const folder = key.endsWith("/");
  const text = keyText(key);
  if (text === null || text !== displayKey(key)) {
    dialog("Rename or move", el("p", {}, "This key holds bytes or control characters that cannot be edited as text."));
    return;
  }
  const path = el("input", { value: folder ? text.slice(0, -1) : text, class: "wide" });
  const target = el("select", {}, el("option", { value: bucket }, bucket));
  const status = el("p", { class: "muted" }, folder
    ? "Every object in the folder is copied, then the originals are deleted. Nothing at the new name is overwritten."
    : "The object is copied, then the original is deleted. Nothing at the new name is overwritten.");
  let copying = true;
  const save = action(folder ? "Move folder" : "Move", async () => {
    let name = path.value.trim().replace(/^\/+/, "");
    if (folder) name = name.replace(/\/+$/, "") + "/";
    if (!name || name === "/") {
      status.textContent = "Enter a name.";
      return;
    }
    const to = encodeKey(name);
    const dstBucket = target.value;
    if (dstBucket === bucket && to === key) {
      d.close();
      return;
    }
    if (folder && dstBucket === bucket && to.startsWith(key)) {
      status.textContent = "A folder cannot move into itself.";
      return;
    }
    try {
      const pairs = folder ? (await listAll(bucket, key)).map((k) => [k, to + k.slice(key.length)]) : [[key, to]];
      if (folder && !confirm(`Move ${plural(pairs.length, "object")} to ${dstBucket}/${name}?`)) return;
      copying = true;
      await moveKeys(bucket, dstBucket, pairs, (n) => {
        status.textContent = `Copied ${n} of ${pairs.length}.`;
        copying = n < pairs.length;
      });
      d.close();
      onDone();
    } catch (err) {
      const why = err.code === "PreconditionFailed" ? "Something already exists at the new name. " : "";
      status.textContent = why + describe(err) +
        (copying ? " The originals were not deleted." : " Everything was copied, but some originals were not deleted.");
      status.className = "error";
    }
  });
  const d = dialog(folder ? "Rename or move folder" : "Rename or move",
    el("div", { class: "field" }, "Bucket", target), el("div", { class: "field" }, "Key", path), status, save);
  s3xml("GET", "").then((res) => {
    for (const b of children(res.getElementsByTagNameNS("*", "Buckets")[0] ?? res, "Bucket")) {
      const n = child(b, "Name");
      if (n !== bucket) target.append(el("option", { value: n }, n));
    }
  }, () => {});
}

const editableHeaders = ["Content-Type", "Cache-Control", "Content-Disposition", "Content-Encoding", "Content-Language", "Expires"];

// metadataDialog rewrites an object's metadata by copying it onto itself; the copy is
// refused if the object changed since current was read.
function metadataDialog(bucket, key, current, onDone) {
  const fields = editableHeaders.map((h) => [h, el("input", { value: current.get(h) ?? "", class: "wide" })]);
  const userRows = el("div");
  const addUser = (name = "", value = "") => {
    const n = el("input", { value: name, placeholder: "name" });
    const v = el("input", { value, placeholder: "value", class: "wide" });
    const row = el("div", { class: "row" }, "x-amz-meta-", n, v, button("Remove", () => row.remove(), "link danger"));
    row.fields = [n, v];
    userRows.append(row);
  };
  for (const [name, value] of current) {
    if (name.startsWith("x-amz-meta-")) addUser(name.slice("x-amz-meta-".length), value);
  }
  const status = el("p", { class: "muted" }, "Saving replaces all of the object's metadata; its data is unchanged.");
  const save = action("Save", async () => {
    const headers = { "x-amz-copy-source": encodePath(bucket, keyBytes(key)), "x-amz-metadata-directive": "REPLACE" };
    if (current.get("etag")) headers["x-amz-copy-source-if-match"] = current.get("etag");
    for (const [h, input] of fields) {
      if (input.value.trim()) headers[h.toLowerCase()] = input.value.trim();
    }
    for (const row of userRows.children) {
      const [n, v] = row.fields.map((f) => f.value.trim());
      if (n) headers["x-amz-meta-" + n.toLowerCase()] = v;
    }
    // fetch refuses header values outside Latin-1, and S3 keeps metadata ASCII.
    const bad = Object.entries(headers).find(([k, v]) => !/^[\x20-\x7e]*$/.test(v) || !/^[a-z0-9-]+$/.test(k));
    if (bad) {
      status.textContent = `${bad[0]}: names may hold letters, digits and "-", and values printable ASCII.`;
      status.className = "error";
      return;
    }
    try {
      await s3("PUT", bucket, key, { headers });
      d.close();
      onDone();
    } catch (err) {
      status.textContent = (err.code === "PreconditionFailed" ? "The object changed since it was opened. " : "") + describe(err);
      status.className = "error";
    }
  });
  const d = dialog("Edit metadata",
    ...fields.map(([h, input]) => el("div", { class: "field" }, h, input)),
    el("h3", {}, "User metadata"), userRows, button("Add", () => addUser()), status, save);
}

async function detailsDialog(bucket, key, report, onChange) {
  try {
    const res = await s3("HEAD", bucket, key);
    const size = Number(res.headers.get("content-length"));
    const type = res.headers.get("content-type");
    const rows = [
      ["Key", displayKey(key)], ["Size", formatBytes(size)],
      ["Last modified", formatDate(res.headers.get("last-modified"))], ["ETag", res.headers.get("etag")],
      ["Content-Type", type],
    ];
    for (const [name, value] of res.headers) {
      if (name.startsWith("x-amz-meta-") || editableHeaders.slice(1).some((h) => h.toLowerCase() === name)) rows.push([name, value]);
    }
    const attrs = await s3xml("GET", bucket, key, {
      params: [["attributes", ""]], headers: { "x-amz-object-attributes": "Checksum,ObjectParts" },
    }).catch(() => null);
    const checksum = attrs && children(attrs, "Checksum")[0];
    for (const c of checksum ? checksum.children : []) {
      rows.push([c.localName === "ChecksumType" ? "Checksum type" : "Checksum " + c.localName.replace(/^Checksum/, ""), c.textContent]);
    }
    const parts = attrs && children(attrs, "ObjectParts")[0];
    if (parts) rows.push(["Parts", child(parts, "PartsCount") || child(parts, "TotalPartsCount")]);
    rows.push(["S3 URI", copyField(`s3://${bucket}/${keyText(key) ?? displayKey(key)}`)]);
    const then = (open) => () => {
      d.close();
      open();
    };
    const d = dialog("Object",
      el("div", { class: "row" },
        button("Download", () => download(bucket, key)),
        button("Share", then(() => shareDialog(bucket, key))),
        keyInfo.readOnly ? null : [
          button("Rename or move", then(() => moveDialog(bucket, key, onChange))),
          button("Edit metadata", then(() => metadataDialog(bucket, key, res.headers, () => {
            onChange();
            detailsDialog(bucket, key, report, onChange);
          }))),
        ]),
      el("table", { class: "details" }, rows.map(([k, v]) => el("tr", {}, el("th", {}, k), el("td", {}, v)))),
      preview(bucket, key, type, size));
  } catch (err) {
    report(err);
  }
}

const maxSearchResults = 500;

// searchDialog finds keys below prefix whose path contains the query, listing the bucket
// page by page; the server has no search, so this reads every key under prefix.
function searchDialog(bucket, prefix, report) {
  const query = el("input", { type: "search", placeholder: "Part of a name or path", class: "wide" });
  const status = el("p", { class: "muted" }, "Searches every folder below " + (prefix ? displayKey(prefix) : "the bucket root") + ".");
  const body = el("tbody");
  let run = 0;
  const stop = button("Stop", () => {
    run++;
    stop.hidden = true;
    status.textContent += " Stopped.";
  }, "link");
  stop.hidden = true;
  const matches = (n) => (n === 1 ? "1 match" : `${n.toLocaleString()} matches`);
  const search = async () => {
    const mine = ++run;
    const q = query.value.trim().toLowerCase();
    body.replaceChildren();
    if (!q) return;
    let token = "";
    let scanned = 0;
    let found = 0;
    stop.hidden = false;
    try {
      for (;;) {
        const params = [["list-type", "2"], ["prefix", keyBytes(prefix)], ["encoding-type", "url"]];
        if (token) params.push(["continuation-token", token]);
        const res = await s3xml("GET", bucket, "", { params });
        if (mine !== run) return;
        for (const c of children(res, "Contents")) {
          scanned++;
          const key = child(c, "Key");
          const path = displayKey(key.slice(prefix.length));
          if (key.endsWith("/") || !path.toLowerCase().includes(q) || found === maxSearchResults) continue;
          found++;
          const folder = key.slice(0, key.lastIndexOf("/") + 1);
          body.append(el("tr", {},
            el("td", {}, button(path, () => detailsDialog(bucket, key, report, render), "link")),
            el("td", { class: "num" }, formatBytes(Number(child(c, "Size")))),
            el("td", { class: "actions" }, el("a", { href: bucketHash(bucket, folder), onclick: () => d.close() }, "Open folder"))));
        }
        status.textContent = `${matches(found)} in ${plural(scanned, "key")} scanned.` +
          (found === maxSearchResults ? ` Stopped at ${maxSearchResults}; narrow the search to see more.` : "");
        if (found === maxSearchResults || child(res, "IsTruncated") !== "true") break;
        token = child(res, "NextContinuationToken");
      }
    } catch (err) {
      if (mine === run) status.textContent = describe(err);
    }
    if (mine === run) stop.hidden = true;
  };
  query.addEventListener("keydown", (e) => {
    if (e.key === "Enter") search();
  });
  const d = dialog("Search " + bucket, el("div", { class: "row" }, query, button("Search", search), stop), status,
    el("table", {}, body));
  d.addEventListener("close", () => run++);
  query.focus();
}

// uploadsDialog lists the multipart uploads left under prefix, such as those of a closed
// browser tab, and aborts them.
async function uploadsDialog(bucket, prefix) {
  const found = [];
  const status = el("p", { class: "muted" }, "Loading…");
  const body = el("tbody");
  const abort = async (u) => {
    await s3("DELETE", bucket, u.key, { params: [["uploadId", u.id]] });
    found.splice(found.indexOf(u), 1);
  };
  const abortAll = action("Abort all", async () => {
    const stale = found.filter((u) => !liveUploads.has(u.id));
    if (!confirm(`Abort ${plural(stale.length, "upload")}? Their uploaded parts are deleted.`)) return;
    try {
      for (const u of stale) await abort(u);
    } catch (err) {
      status.textContent = describe(err);
    }
    draw();
  }, "danger");
  const draw = () => {
    status.textContent = found.length
      ? `${plural(found.length, "upload")} in this folder ${found.length === 1 ? "was" : "were"} started and never completed. The server also removes incomplete uploads once they expire.`
      : "No incomplete uploads in this folder.";
    abortAll.hidden = keyInfo.readOnly || !found.some((u) => !liveUploads.has(u.id));
    body.replaceChildren(...found.map((u) => el("tr", {},
      el("td", {}, displayKey(u.key)),
      el("td", {}, formatDate(u.started)),
      el("td", { class: "actions" }, liveUploads.has(u.id)
        ? el("span", { class: "muted" }, "uploading in this page")
        : keyInfo.readOnly ? null : action("Abort", async () => {
          try {
            await abort(u);
          } catch (err) {
            status.textContent = describe(err);
          }
          draw();
        }, "link danger")))));
  };
  dialog("Incomplete uploads", status,
    el("table", {}, el("thead", {}, el("tr", {}, el("th", {}, "Key"), el("th", {}, "Started"), el("th"))), body), abortAll);
  abortAll.hidden = true;
  try {
    let keyMarker = "";
    let idMarker = "";
    for (;;) {
      const params = [["uploads", ""], ["prefix", keyBytes(prefix)], ["encoding-type", "url"]];
      if (keyMarker) params.push(["key-marker", keyBytes(keyMarker)], ["upload-id-marker", idMarker]);
      const res = await s3xml("GET", bucket, "", { params });
      for (const u of children(res, "Upload")) found.push({ key: child(u, "Key"), id: child(u, "UploadId"), started: child(u, "Initiated") });
      if (child(res, "IsTruncated") !== "true") break;
      keyMarker = child(res, "NextKeyMarker");
      idMarker = child(res, "NextUploadIdMarker");
    }
  } catch (err) {
    status.textContent = describe(err);
    return;
  }
  draw();
}

// ---- Views

function header(active) {
  const link = (label, hash) => el("a", { href: hash, class: active === hash ? "active" : "" }, label);
  return el("header", {},
    el("strong", {}, "plainbucket"),
    el("nav", {}, link("Buckets", "#/"), link("Usage", "#/usage")),
    el("span", { class: "spacer" }),
    el("span", { class: "muted" }, cred.accessKey),
    keyInfo.maintenance ? el("span", { class: "badge", title: "The server refuses writes until its read-only mode is turned off." }, "read-only: maintenance")
      : keyInfo.readOnly ? el("span", { class: "badge" }, "read-only") : null,
    themeButton(),
    button("Sign out", () => {
      const running = [uploads.size ? plural(uploads.size, "upload") : "", downloads.size ? plural(downloads.size, "ZIP download") : ""].filter(Boolean);
      if (running.length && !confirm(`Cancel ${running.join(" and ")} in progress and sign out?`)) return;
      signOut();
    }, "link"));
}

// signOut forgets the key and everything opened with it.
function signOut() {
  for (const stop of uploads) stop();
  for (const d of document.querySelectorAll("dialog")) d.close();
  cred = null;
  keyInfo = { readOnly: false };
  // replaceState fires no hashchange, whose render would drop the banner set after this one.
  history.replaceState(null, "", location.pathname + location.search);
  render();
}

const idleLimit = 30 * 60 * 1000;
let lastActive = Date.now();
for (const type of ["pointerdown", "keydown", "wheel"]) {
  window.addEventListener(type, () => { lastActive = Date.now(); }, { passive: true });
}
// The secret key should not outlive the person at the keyboard; a running upload defers it.
setInterval(() => {
  if (!cred || uploads.size || downloads.size || Date.now() - lastActive < idleLimit) return;
  signOut();
  notify("Signed out after 30 minutes without activity.", "status");
}, 30 * 1000);

async function bucketsView() {
  const report = reporter();
  const name = el("input", { placeholder: "new-bucket-name", pattern: "[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]", required: true });
  const create = el("button", { type: "submit" }, "Create bucket");
  const form = el("form", { class: "row", onsubmit: async (e) => {
    e.preventDefault();
    create.disabled = true;
    try {
      await s3("PUT", name.value);
      location.hash = bucketHash(name.value, "");
    } catch (err) {
      report(err);
    } finally {
      create.disabled = false;
    }
  } }, name, create);

  const body = el("tbody");
  const view = el("section", {}, el("h1", {}, "Buckets"), keyInfo.readOnly ? null : form,
    el("table", {}, el("thead", {}, el("tr", {}, el("th", {}, "Name"), el("th", {}, "Created"), el("th"))), body));
  try {
    const res = await s3xml("GET", "");
    const buckets = children(res.getElementsByTagNameNS("*", "Buckets")[0] ?? res, "Bucket");
    if (!buckets.length) body.append(el("tr", {}, el("td", { colspan: 3, class: "muted" }, "No buckets yet.")));
    for (const b of buckets) {
      const n = child(b, "Name");
      body.append(el("tr", {},
        el("td", {}, el("a", { href: bucketHash(n, "") }, n)),
        el("td", {}, formatDate(child(b, "CreationDate"))),
        el("td", { class: "actions" }, keyInfo.readOnly ? null : action("Delete", async () => {
          if (!confirm(`Delete the bucket ${n}? It must be empty.`)) return;
          try {
            await s3("DELETE", n);
            render();
          } catch (err) {
            report(err);
          }
        }, "link danger"))));
    }
  } catch (err) {
    report(err);
  }
  return view;
}

// bucketHash links to a folder; prefix is encoded, so it is already safe in a URL.
function bucketHash(bucket, prefix) {
  return "#/b/" + encodeURIComponent(bucket) + "/" + prefix;
}

function download(bucket, key) {
  const name = displayKey(key.slice(key.lastIndexOf("/") + 1)) || "download";
  const disposition = `attachment; filename*=UTF-8''${encodeURIComponent(name)}`;
  // download keeps an error response from replacing the page, which would sign out.
  const a = el("a", { href: presignedURL(bucket, key, 300, [["response-content-disposition", disposition]]), download: name });
  document.body.append(a);
  a.click();
  a.remove();
}

// zipName names the archive of a folder (encoded, ending in "/") or of a bucket's root.
function zipName(bucket, folder) {
  const name = folder ? displayKey(folder.slice(folder.slice(0, -1).lastIndexOf("/") + 1, -1)) : bucket;
  return (name.replace(/[\\/:*?"<>|\u0000-\u001f]/g, "_") || "download") + ".zip";
}

async function blobCRC(blob, signal) {
  const reader = blob.stream().getReader();
  let crc = 0;
  try {
    for (;;) {
      signal.throwIfAborted();
      const { done, value } = await reader.read();
      if (done) return crc;
      crc = crc32(value, crc);
    }
  } finally {
    reader.cancel().catch(() => {});
  }
}

const zipMemoryWarning = 2 * 1024 * 1024 * 1024;
// The abort functions of the ZIP downloads running; like uploads they defer the idle sign-out.
const downloads = new Set();

// zipDownload saves the objects load returns (as listObjects does) as one stored ZIP, entries
// named by key minus base. Where showSaveFilePicker exists (Chromium, secure origin) each object
// goes to the picked file once fetched; elsewhere the archive is built as a Blob, which browsers
// spill to disk once large, then downloaded. The picker opens before load runs, while the click
// that asked for it still counts as a user gesture.
async function zipDownload(bucket, base, load, name) {
  let file = null;
  if (window.showSaveFilePicker) {
    try {
      const handle = await window.showSaveFilePicker({ suggestedName: name, types: [{ description: "ZIP archive", accept: { "application/zip": [".zip"] } }] });
      file = await handle.createWritable();
    } catch (err) {
      if (err.name === "AbortError") return;
    }
  }
  const abort = new AbortController();
  const status = el("p", { class: "muted" }, "Listing the objects…");
  const bar = el("progress", { value: 0 });
  let finished = false;
  const stop = () => abort.abort();
  downloads.add(stop);
  const d = dialog(`Download ${name}`, status, bar);
  d.addEventListener("close", () => { if (!finished) stop(); });
  const parts = [];
  const write = (chunk) => (file ? file.write(chunk) : parts.push(chunk));
  try {
    const objects = await load();
    abort.signal.throwIfAborted();
    if (!objects.length) {
      if (file) await file.abort();
      finished = true;
      d.close();
      notify("There is nothing to download.", "status");
      return;
    }
    const total = objects.reduce((n, o) => n + o.size, 0);
    if (!file && total > zipMemoryWarning &&
        !confirm(`This browser builds the ${formatBytes(total)} archive before saving it, which needs that much free disk space or memory. Continue?`)) {
      finished = true;
      d.close();
      return;
    }
    bar.max = total || 1;
    const zip = new ZipBuilder();
    const baseLength = keyBytes(base).length;
    let done = 0;
    for (const [i, o] of objects.entries()) {
      status.textContent = `Object ${i + 1} of ${objects.length} · ${formatBytes(done)} of ${formatBytes(total)}`;
      const entry = { name: entryName(keyBytes(o.key).subarray(baseLength)), utf8: keyText(o.key) !== null, modified: new Date(o.modified) };
      if (o.key.endsWith("/")) {
        await write(zip.header(entry));
        continue;
      }
      const blob = await (await s3("GET", bucket, o.key, { signal: abort.signal })).blob();
      await write(zip.header({ ...entry, size: blob.size, crc: await blobCRC(blob, abort.signal) }));
      await write(blob);
      done += o.size;
      bar.value = done;
    }
    for (const p of zip.end()) await write(p);
    if (file) {
      await file.close();
    } else {
      const url = URL.createObjectURL(new Blob(parts, { type: "application/zip" }));
      const a = el("a", { href: url, download: name });
      document.body.append(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 60 * 1000);
    }
    finished = true;
    d.close();
  } catch (err) {
    finished = true;
    if (file) await file.abort().catch(() => {});
    if (!abort.signal.aborted) {
      status.textContent = describe(err);
      bar.remove();
    }
  } finally {
    downloads.delete(stop);
  }
}

// bucketInfo summarizes a bucket's settings and usage from the usage API, once it answers.
function bucketInfo(bucket) {
  const info = el("div", { class: "row bucket-info" });
  consoleUsage().then((u) => {
    const b = u.buckets.find((x) => x.name === bucket);
    if (!b) return;
    const badge = (text) => el("span", { class: "badge" }, text);
    // Unlike el, replaceChildren would show a null as the text "null".
    info.replaceChildren(...[
      b.public ? badge("public") : null,
      b.website ? badge("website") : null,
      b.cors ? badge("CORS") : null,
      b.max_size ? badge(`limit ${formatBytes(b.max_size)}` + (b.evict ? ", evicts least recently used" : "")) : null,
      b.expire_after ? badge(`expires after ${formatGoDuration(b.expire_after)}`) : null,
      b.trash_after ? el("a", { href: trashHash(bucket), class: "badge" }, `trash, kept ${formatGoDuration(b.trash_after)}`) : null,
      el("span", { class: "muted" }, `${plural(b.objects, "object")} · ${formatBytes(b.bytes)}`)].filter(Boolean));
  }, () => {});
  return info;
}

const sortLabels = { name: "Name", size: "Size", modified: "Last modified" };
const sortOrders = {
  name: (a, b) => a.name.localeCompare(b.name),
  size: (a, b) => a.size - b.size,
  modified: (a, b) => (a.modified < b.modified ? -1 : a.modified > b.modified ? 1 : 0),
};

async function browserView(bucket, prefix) {
  const report = reporter();
  const writable = !keyInfo.readOnly;
  // The loaded folders and objects; filtering and sorting never modify this.
  const entries = [];
  const selected = new Set();
  let anchor = null;
  let sort = { by: "name", dir: 1 };
  const body = el("tbody");
  const more = el("div");
  const count = el("span", { class: "muted" });
  const filter = el("input", { type: "search", placeholder: "Filter this folder", class: "filter" });
  const all = el("input", { type: "checkbox", title: "Select every object shown" });

  const deleteSelected = button("Delete selected", async () => {
    if (!selected.size || !confirm(`Delete ${plural(selected.size, "selected object")}?`)) return;
    deleteSelected.disabled = true;
    try {
      await deleteKeys(bucket, [...selected]);
      render();
    } catch (err) {
      report(err);
      deleteSelected.disabled = selected.size === 0;
    }
  }, "danger");
  deleteSelected.disabled = true;

  const shown = () => {
    const q = filter.value.trim().toLowerCase();
    const order = sortOrders[sort.by];
    return entries.filter((e) => !q || e.name.toLowerCase().includes(q))
      .sort((a, b) => (a.folder !== b.folder ? (a.folder ? -1 : 1) : sort.dir * order(a, b)));
  };
  const select = (e, on) => {
    if (on) selected.add(e.key);
    else selected.delete(e.key);
    if (e.box) e.box.checked = on;
  };
  const syncSelection = (list) => {
    const objects = list.filter((e) => !e.folder);
    const n = objects.filter((e) => selected.has(e.key)).length;
    all.checked = n > 0 && n === objects.length;
    all.indeterminate = n > 0 && n < objects.length;
    all.disabled = !objects.length;
    deleteSelected.disabled = selected.size === 0;
  };

  const heads = {};
  const sortHead = (by, cls) => {
    heads[by] = button(sortLabels[by], () => {
      sort = { by, dir: sort.by === by ? -sort.dir : 1 };
      draw();
    }, "link sort");
    return el("th", { class: cls }, heads[by]);
  };
  const upRow = prefix
    ? el("tr", {}, el("td"), el("td", {}, el("a", { href: bucketHash(bucket, prefix.slice(0, prefix.slice(0, -1).lastIndexOf("/") + 1)) }, "..")), el("td"), el("td"), el("td"))
    : null;

  const draw = () => {
    const list = shown();
    // A selected object the filter hides would otherwise be deleted unseen.
    for (const e of entries) if (selected.has(e.key) && !list.includes(e)) select(e, false);
    body.replaceChildren(...(upRow ? [upRow] : []), ...list.map((e) => e.row));
    if (!list.length) {
      body.append(el("tr", {}, el("td", { colspan: 5, class: "muted" }, entries.length ? "Nothing matches the filter." : "This folder is empty.")));
    }
    const loaded = more.childElementCount ? " loaded so far" : "";
    count.textContent = (list.length === entries.length ? "" : `${list.length} of `) + plural(entries.length, "item") + loaded;
    for (const [by, b] of Object.entries(heads)) b.textContent = sortLabels[by] + (sort.by === by ? (sort.dir > 0 ? " ▲" : " ▼") : "");
    syncSelection(list);
  };
  filter.addEventListener("input", draw);
  all.addEventListener("change", () => {
    for (const e of shown()) if (!e.folder) select(e, all.checked);
    syncSelection(shown());
  });

  const crumbs = [el("a", { href: "#/" }, "Buckets"), " / ", el("a", { href: bucketHash(bucket, "") }, bucket)];
  let acc = "";
  for (const part of prefix.split("/").filter(Boolean)) {
    acc += part + "/";
    crumbs.push(" / ", el("a", { href: bucketHash(bucket, acc) }, displayKey(part)));
  }

  const zipShown = async () => {
    try {
      if (selected.size) {
        const chosen = entries.filter((e) => !e.folder && selected.has(e.key));
        await zipDownload(bucket, prefix, async () => chosen, zipName(bucket, prefix));
      } else {
        const parent = prefix.slice(0, prefix.slice(0, -1).lastIndexOf("/") + 1);
        await zipDownload(bucket, parent, () => listObjects(bucket, prefix), zipName(bucket, prefix));
      }
    } catch (err) {
      report(err);
    }
  };
  const zipButton = action("Download ZIP", zipShown);
  zipButton.title = "The selected objects, or with none selected the whole folder, as one ZIP file";

  const startUploads = (list) => {
    for (const [path, f] of list) upload(bucket, prefix + encodeKey(path), f, render);
  };
  const files = el("input", { type: "file", multiple: true, class: "hidden" });
  files.addEventListener("change", () => startUploads([...files.files].map((f) => [f.webkitRelativePath || f.name, f])));
  const folder = el("input", { placeholder: "folder name" });
  const toolbar = el("div", { class: "row" },
    writable ? [button("Upload files", () => files.click()), files, folder, action("New folder", async () => {
      const name = folder.value.replace(/^\/+|\/+$/g, "");
      if (!name) return;
      const marker = prefix + encodeKey(name) + "/";
      try {
        await s3("PUT", bucket, marker, { body: "" });
        location.hash = bucketHash(bucket, marker);
      } catch (err) {
        report(err);
      }
    })] : null,
    button("Refresh", () => render()),
    button("Search", () => searchDialog(bucket, prefix, report)),
    button("Incomplete uploads", () => uploadsDialog(bucket, prefix)),
    zipButton,
    el("span", { class: "spacer" }), writable ? deleteSelected : null);

  const table = el("table", {},
    el("thead", {}, el("tr", {}, el("th", {}, writable ? all : null), sortHead("name"), sortHead("size", "num"), sortHead("modified"), el("th"))),
    body);
  const view = el("section", { class: "drop" },
    el("h1", { class: "crumbs" }, crumbs), bucketInfo(bucket), toolbar, el("div", { class: "row" }, filter, count), uploadList, table, more,
    el("p", { class: "muted" }, writable ? "Drop files or folders here to upload them to this folder. " : "", "Press ? for keyboard shortcuts."));
  if (writable) {
    view.addEventListener("dragover", (e) => {
      e.preventDefault();
      view.classList.add("over");
    });
    view.addEventListener("dragleave", () => view.classList.remove("over"));
    view.addEventListener("drop", (e) => {
      e.preventDefault();
      view.classList.remove("over");
      droppedFiles(e.dataTransfer).then(startUploads, report);
    });
  }

  const folderEntry = (full) => {
    const name = displayKey(full.slice(prefix.length));
    const e = { folder: true, key: full, name, size: 0, modified: "" };
    const sizeCell = el("td", { class: "num" });
    // Sizing a folder lists everything under it, so it waits to be asked for.
    sizeCell.append(action("Calculate", async () => {
      try {
        const objects = await listObjects(bucket, full);
        e.size = objects.reduce((n, o) => n + o.size, 0);
        sizeCell.replaceChildren(formatBytes(e.size), el("span", { class: "muted" }, ` · ${plural(objects.length, "object")}`));
      } catch (err) {
        report(err);
      }
    }, "link"));
    e.row = el("tr", { class: "folder" },
        el("td"),
        el("td", {}, el("a", { href: bucketHash(bucket, full) }, name)),
        sizeCell, el("td"),
        el("td", { class: "actions" },
          action("Download ZIP", async () => {
            try {
              await zipDownload(bucket, prefix, () => listObjects(bucket, full), zipName(bucket, full));
            } catch (err) {
              report(err);
            }
          }, "link"),
          writable ? button("Rename", () => moveDialog(bucket, full, render), "link") : null, writable ? action("Delete folder", async () => {
          try {
            const keys = await listAll(bucket, full);
            if (!confirm(`Delete the folder ${name} and the ${plural(keys.length, "object")} in it?`)) return;
            await deleteKeys(bucket, keys);
            render();
          } catch (err) {
            report(err);
          }
        }, "link danger") : null));
    return e;
  };
  const objectEntry = (c) => {
    const key = child(c, "Key");
    const e = { folder: false, key, name: displayKey(key.slice(prefix.length)), size: Number(child(c, "Size")), modified: child(c, "LastModified") };
    if (writable) {
      e.box = el("input", { type: "checkbox" });
      e.box.addEventListener("click", (ev) => {
        const objects = shown().filter((x) => !x.folder);
        const from = objects.findIndex((x) => x.key === anchor);
        const to = objects.indexOf(e);
        if (ev.shiftKey && from >= 0 && to >= 0) {
          for (const x of objects.slice(Math.min(from, to), Math.max(from, to) + 1)) select(x, e.box.checked);
        } else {
          select(e, e.box.checked);
        }
        anchor = key;
        syncSelection(shown());
      });
    }
    e.row = el("tr", {},
      el("td", {}, e.box ?? null),
      el("td", {}, button(e.name, () => detailsDialog(bucket, key, report, render), "link")),
      el("td", { class: "num" }, formatBytes(e.size)),
      el("td", {}, formatDate(e.modified)),
      el("td", { class: "actions" },
        button("Download", () => download(bucket, key), "link"),
        button("Share", () => shareDialog(bucket, key), "link"),
        writable ? button("Rename", () => moveDialog(bucket, key, render), "link") : null,
        writable ? action("Delete", async () => {
          if (!confirm(`Delete ${displayKey(key)}?`)) return;
          try {
            await s3("DELETE", bucket, key);
            render();
          } catch (err) {
            report(err);
          }
        }, "link danger") : null));
    return e;
  };

  const loadPage = async (token) => {
    const params = [["list-type", "2"], ["prefix", keyBytes(prefix)], ["delimiter", "/"], ["encoding-type", "url"]];
    if (token) params.push(["continuation-token", token]);
    const res = await s3xml("GET", bucket, "", { params });
    for (const p of children(res, "CommonPrefixes")) entries.push(folderEntry(child(p, "Prefix")));
    for (const c of children(res, "Contents")) {
      if (child(c, "Key") !== prefix) entries.push(objectEntry(c)); // skips the folder's own marker
    }
    more.replaceChildren();
    if (child(res, "IsTruncated") === "true") {
      const next = child(res, "NextContinuationToken");
      more.append(action("Load more", () => loadPage(next).catch(report)));
    }
    draw();
  };
  try {
    await loadPage("");
  } catch (err) {
    report(err);
  }
  const up = prefix ? bucketHash(bucket, prefix.slice(0, prefix.slice(0, -1).lastIndexOf("/") + 1)) : "#/";
  view.shortcuts = {
    "/": () => filter.focus(),
    r: () => render(),
    s: () => searchDialog(bucket, prefix, report),
    z: () => zipButton.click(),
    // On a Mac the delete key is Backspace; going up would drop a selection meant for Delete.
    Backspace: () => { if (!selected.size) location.hash = up; },
    Escape: () => {
      for (const e of entries) select(e, false);
      filter.value = "";
      draw();
    },
  };
  if (writable) {
    Object.assign(view.shortcuts, {
      u: () => files.click(),
      n: () => folder.focus(),
      a: () => all.disabled || all.click(),
      Delete: () => deleteSelected.click(),
    });
  }
  return view;
}

async function usageView() {
  const report = reporter();
  const view = el("section", {}, el("h1", {}, "Usage"));
  try {
    const usage = await consoleUsage();
    const row = (b, name) => el("tr", {},
      el("td", {}, name),
      el("td", { class: "num" }, b.objects.toLocaleString()),
      el("td", { class: "num" }, formatBytes(b.bytes)),
      el("td", { class: "num" }, b.max_size ? formatBytes(b.max_size) + (b.evict ? " (evicts)" : "") : ""),
      el("td", {}, b.expire_after ? formatGoDuration(b.expire_after) : ""),
      el("td", {}, [b.public && "public", b.website && "website", b.cors && "CORS"].filter(Boolean).join(", ")),
      el("td", { class: "muted" }, formatDate(b.measured_at)));
    view.append(
      el("p", { class: "muted" }, "Writes and deletes through the server count at once; files changed outside the server count from the next hourly measurement."),
      el("table", {},
        el("thead", {}, el("tr", {}, ["Bucket", "Objects", "Size", "Limit", "Expires after", "Access", "Measured"].map((h) => el("th", {}, h)))),
        el("tbody", {}, usage.buckets.map((b) => row(b, b.name)), usage.total ? row(usage.total, "All buckets") : null)));
    const d = usage.disk;
    if (d) {
      const low = d.min_free > 0 && d.free < d.min_free;
      view.append(el("p", { class: low ? "error" : "muted" },
        `Disk: ${formatBytes(d.free)} free of ${formatBytes(d.size)}` +
        (d.min_free ? ` · writes are refused below ${formatBytes(d.min_free)} free` : "")));
    }
  } catch (err) {
    report(err);
  }
  return view;
}

function loginView() {
  const report = reporter();
  // autocomplete off: the secret key is meant to live in this page's memory only.
  const accessKey = el("input", { autocomplete: "off", required: true, autofocus: true });
  const secretKey = el("input", { type: "password", autocomplete: "off", required: true });
  const submit = el("button", { type: "submit" }, "Sign in");
  const form = el("form", { class: "login", onsubmit: async (e) => {
    e.preventDefault();
    submit.disabled = true;
    cred = { accessKey: accessKey.value.trim(), secretKey: secretKey.value, region };
    try {
      await s3("GET", "");
      const usage = await consoleUsage().catch(() => ({}));
      keyInfo = { readOnly: Boolean(usage.read_only), maintenance: Boolean(usage.maintenance) };
      render();
    } catch (err) {
      cred = null;
      report(err);
    } finally {
      submit.disabled = false;
    }
  } },
  el("h1", {}, "plainbucket"),
  el("label", {}, "Access key", accessKey),
  el("label", {}, "Secret key", secretKey),
  submit,
  el("p", { class: "muted" }, "Requests are signed in this page; the secret key is never sent and is forgotten on reload."));
  return form;
}

function trashHash(bucket) {
  return "#/t/" + encodeURIComponent(bucket);
}

async function trashView(bucket) {
  const report = reporter();
  const writable = !keyInfo.readOnly;
  const body = el("tbody");
  const note = el("p", { class: "muted" });
  const params = (id, act) => [["bucket", bucket], ["id", id], ["action", act]];
  const load = async () => {
    const t = await (await consoleAPI("GET", trashPath, [["bucket", bucket]])).json();
    note.textContent = (t.trash_after
      ? `Deleted objects stay here for ${formatGoDuration(t.trash_after)}, then they are removed for good.`
      : "This bucket no longer keeps deleted objects; these are removed when their time is up.") +
      (t.entries.length ? ` ${plural(t.entries.length, "object")}, ${formatBytes(t.bytes)} of disk space.` : " The trash is empty.");
    body.replaceChildren(...t.entries.map((e) => {
      // The API returns keys as text; the console's key identity is the encoded form.
      const name = displayKey(encodeKey(e.key));
      return el("tr", {},
        el("td", {}, name),
        el("td", { class: "num" }, formatBytes(e.size)),
        el("td", {}, formatDate(e.deleted_at)),
        el("td", { class: "muted" }, formatDate(e.expires_at)),
        el("td", {}, writable ? [
          action("Restore", async () => {
            try {
              await consoleAPI("POST", trashPath, params(e.id, "restore"));
              notify(`Restored ${name}.`, "status");
              await load();
            } catch (err) {
              if (err.code === "PreconditionFailed") notify(`${name} exists again; delete or rename it before restoring this one.`);
              else report(err);
            }
          }),
          action("Delete for good", async () => {
            if (!confirm(`Delete ${name} for good? It cannot be restored afterwards.`)) return;
            try {
              await consoleAPI("POST", trashPath, params(e.id, "purge"));
              await load();
            } catch (err) {
              report(err);
            }
          }, "danger"),
        ] : null));
    }));
  };
  const view = el("section", {},
    el("h1", { class: "crumbs" }, el("a", { href: "#/" }, "Buckets"), " / ", el("a", { href: bucketHash(bucket, "") }, bucket), " / Trash"),
    el("div", { class: "row" }, button("Refresh", () => load().catch(report))),
    note,
    el("table", {}, el("thead", {}, el("tr", {}, ["Key", "Size", "Deleted", "Removed for good", ""].map((h, i) => el("th", { class: i === 1 ? "num" : "" }, h)))), body));
  try {
    await load();
  } catch (err) {
    report(err);
  }
  return view;
}

// parseBucketHash reads "#/b/<bucket>/<encoded prefix>"; it throws on a malformed one.
function parseBucketHash(hash) {
  const rest = hash.slice(4);
  const i = rest.indexOf("/");
  const bucket = decodeURIComponent(i < 0 ? rest : rest.slice(0, i));
  if (!bucket) throw new URIError("No bucket in " + hash);
  return [bucket, i < 0 ? "" : canonicalKey(rest.slice(i + 1))];
}

const shortcutHelp = [
  ["/", "Filter this folder"], ["u", "Upload files"], ["n", "Type a new folder's name"], ["r", "Refresh"],
  ["s", "Search the bucket"], ["a", "Select or clear every object shown"],
  ["z", "Download the selected objects, or the folder, as a ZIP file"], ["Delete", "Delete the selected objects (fn+delete on a Mac)"],
  ["Backspace", "Go up one folder, when nothing is selected"], ["Escape", "Clear the filter and the selection; in a field, leave it"],
  ["?", "Show these shortcuts"],
];

function shortcutsDialog() {
  dialog("Keyboard shortcuts", el("table", {}, el("tbody", {},
    shortcutHelp.map(([k, what]) => el("tr", {}, el("td", {}, el("kbd", {}, k)), el("td", {}, what))))),
  el("p", { class: "muted" }, "They work in a folder, outside text fields; a read-only key has no upload, folder or delete shortcuts."));
}

// The shortcuts of the view on screen, by KeyboardEvent.key.
let shortcuts = {};
document.addEventListener("keydown", (e) => {
  if (!cred || e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey || document.querySelector("dialog[open]")) return;
  const t = e.target;
  // A focused checkbox or button is no text field: select a row, then press z or Delete.
  const typing = t instanceof HTMLElement && (t.isContentEditable ||
    t.matches("textarea, select, input:not([type=checkbox], [type=radio], [type=button], [type=submit], [type=reset], [type=file], [type=range], [type=color])"));
  if (typing) {
    if (e.key === "Escape") t.blur();
    return;
  }
  const run = e.key === "?" ? shortcutsDialog : shortcuts[e.key];
  if (!run) return;
  e.preventDefault();
  run();
});

async function render() {
  const mine = ++generation;
  banner = null;
  if (!cred) {
    shortcuts = {};
    app.replaceChildren(el("div", { class: "topbar" }, themeButton()), loginView());
    return;
  }
  let hash = location.hash || "#/";
  let view;
  if (hash === "#/usage") {
    view = await usageView();
  } else if (hash.startsWith("#/t/")) {
    let bucket = "";
    try {
      bucket = decodeURIComponent(hash.slice(4));
    } catch {
      hash = "#/";
    }
    view = await (bucket ? trashView(bucket) : bucketsView());
  } else if (hash.startsWith("#/b/")) {
    let where = null;
    try {
      where = parseBucketHash(hash);
    } catch {
      hash = "#/";
    }
    view = await (where ? browserView(...where) : bucketsView());
  } else {
    view = await bucketsView();
  }
  if (mine !== generation) return;
  shortcuts = view.shortcuts ?? {};
  const keep = banner;
  app.replaceChildren(header(hash.startsWith("#/b/") || hash.startsWith("#/t/") ? "#/" : hash), view);
  if (keep) app.prepend(keep);
}

window.addEventListener("hashchange", render);
render();
