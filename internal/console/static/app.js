// The plainbucket console. Every action is an S3 request signed in the page with the key
// typed at login; the key stays in memory only, so a reload signs out.
import { canonicalKey, displayKey, encodeKey, escapeXML, keyBytes, xmlKey } from "./keys.js";
import { encodePath, presign, queryString, signHeaders } from "./sigv4.js";

const host = location.host;
const region = "us-east-1";
const minPartSize = 16 * 1024 * 1024;
const maxParts = 10000;
const partConcurrency = 3;
const partAttempts = 3;
const shareExpiries = [["1 hour", 3600], ["1 day", 86400], ["7 days", 604800]];

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
async function s3(method, bucket, key = "", { params = [], headers = {}, body, as = cred } = {}) {
  const { path, query } = target(bucket, key, params);
  const res = await fetch(path + (query ? "?" + query : ""), {
    method, body, headers: signHeaders(as, { method, host, path, query, headers }),
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

async function consoleUsage() {
  const path = "/_console/api/usage";
  const res = await fetch(path, { headers: signHeaders(cred, { method: "GET", host, path }) });
  if (!res.ok) throw await s3Error(res.status, await res.text());
  return res.json();
}

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

// listAll returns every encoded key under prefix, for deleting a folder.
async function listAll(bucket, prefix) {
  const keys = [];
  let token = "";
  for (;;) {
    const params = [["list-type", "2"], ["prefix", keyBytes(prefix)], ["encoding-type", "url"]];
    if (token) params.push(["continuation-token", token]);
    const res = await s3xml("GET", bucket, "", { params });
    for (const c of children(res, "Contents")) keys.push(child(c, "Key"));
    if (child(res, "IsTruncated") !== "true") return keys;
    token = child(res, "NextContinuationToken");
  }
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

const uploadList = el("div", { class: "uploads" });
// Cancel functions of the uploads in progress.
const uploads = new Set();
// Multipart upload IDs this page is still sending, which uploadsDialog must not abort.
const liveUploads = new Set();

window.addEventListener("beforeunload", (e) => {
  if (!uploads.size) return;
  e.preventDefault();
  e.returnValue = "";
});

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// upload sends file to an encoded key. It signs with the key that was signed in when it
// started, so signing out cancels it instead of leaving it to another key.
function upload(bucket, key, file, onDone) {
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

  const finish = (text, kind) => {
    uploads.delete(stop);
    status.textContent = text;
    status.className = kind;
    cancel.remove();
    setTimeout(() => row.remove(), kind === "error" ? 15000 : 4000);
  };
  const headers = { "content-type": file.type || "application/octet-stream" };

  const run = async () => {
    if (file.size <= minPartSize) {
      await put(as, bucket, key, [], headers, file, (n) => { bar.value = n; }, pending);
      return;
    }
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
            bar.value = sent.reduce((a, b) => a + b, 0);
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
      await s3("POST", bucket, key, { params: [["uploadId", uploadId]], body, as });
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
  }, (err) => finish(describe(err), "error"));
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

function shareDialog(bucket, key) {
  const select = el("select", {}, shareExpiries.map(([label, s]) => el("option", { value: s }, label)));
  const field = el("div");
  const render = () => field.replaceChildren(copyField(presignedURL(bucket, key, Number(select.value))));
  select.addEventListener("change", render);
  render();
  dialog("Share " + displayKey(key),
    el("p", { class: "muted" }, "Anyone with this link can download the object until it expires, as long as this key stays valid."),
    el("label", {}, "Expires in ", select), field);
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

async function detailsDialog(bucket, key, report) {
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
      if (name.startsWith("x-amz-meta-")) rows.push([name, value]);
    }
    dialog("Object", el("table", { class: "details" }, rows.map(([k, v]) => el("tr", {}, el("th", {}, k), el("td", {}, v)))),
      preview(bucket, key, type, size));
  } catch (err) {
    report(err);
  }
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
    keyInfo.readOnly ? el("span", { class: "badge" }, "read-only") : null,
    themeButton(),
    button("Sign out", () => {
      if (uploads.size && !confirm(`Cancel ${plural(uploads.size, "upload")} in progress and sign out?`)) return;
      for (const stop of uploads) stop();
      cred = null;
      keyInfo = { readOnly: false };
      location.hash = "";
      render();
    }, "link"));
}

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
    button("Incomplete uploads", () => uploadsDialog(bucket, prefix)),
    el("span", { class: "spacer" }), writable ? deleteSelected : null);

  const table = el("table", {},
    el("thead", {}, el("tr", {}, el("th", {}, writable ? all : null), sortHead("name"), sortHead("size", "num"), sortHead("modified"), el("th"))),
    body);
  const view = el("section", { class: "drop" },
    el("h1", { class: "crumbs" }, crumbs), toolbar, el("div", { class: "row" }, filter, count), uploadList, table, more,
    writable ? el("p", { class: "muted" }, "Drop files or folders here to upload them to this folder.") : null);
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
    return {
      folder: true, key: full, name, size: 0, modified: "",
      row: el("tr", { class: "folder" },
        el("td"),
        el("td", {}, el("a", { href: bucketHash(bucket, full) }, name)),
        el("td"), el("td"),
        el("td", { class: "actions" }, writable ? action("Delete folder", async () => {
          try {
            const keys = await listAll(bucket, full);
            if (!confirm(`Delete the folder ${name} and the ${plural(keys.length, "object")} in it?`)) return;
            await deleteKeys(bucket, keys);
            render();
          } catch (err) {
            report(err);
          }
        }, "link danger") : null)),
    };
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
      el("td", {}, button(e.name, () => detailsDialog(bucket, key, report), "link")),
      el("td", { class: "num" }, formatBytes(e.size)),
      el("td", {}, formatDate(e.modified)),
      el("td", { class: "actions" },
        button("Download", () => download(bucket, key), "link"),
        button("Share", () => shareDialog(bucket, key), "link"),
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
      el("td", {}, b.expire_after ?? ""),
      el("td", {}, b.public ? "public" : ""),
      el("td", { class: "muted" }, formatDate(b.measured_at)));
    view.append(
      el("p", { class: "muted" }, "Writes and deletes through the server count at once; files changed outside the server count from the next hourly measurement."),
      el("table", {},
        el("thead", {}, el("tr", {}, ["Bucket", "Objects", "Size", "Limit", "Expires after", "Reads", "Measured"].map((h) => el("th", {}, h)))),
        el("tbody", {}, usage.buckets.map((b) => row(b, b.name)), usage.total ? row(usage.total, "All buckets") : null)));
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
      keyInfo = { readOnly: Boolean((await consoleUsage().catch(() => ({}))).read_only) };
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

// parseBucketHash reads "#/b/<bucket>/<encoded prefix>"; it throws on a malformed one.
function parseBucketHash(hash) {
  const rest = hash.slice(4);
  const i = rest.indexOf("/");
  const bucket = decodeURIComponent(i < 0 ? rest : rest.slice(0, i));
  if (!bucket) throw new URIError("No bucket in " + hash);
  return [bucket, i < 0 ? "" : canonicalKey(rest.slice(i + 1))];
}

async function render() {
  const mine = ++generation;
  banner = null;
  if (!cred) {
    app.replaceChildren(el("div", { class: "topbar" }, themeButton()), loginView());
    return;
  }
  let hash = location.hash || "#/";
  let view;
  if (hash === "#/usage") {
    view = await usageView();
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
  const keep = banner;
  app.replaceChildren(header(hash.startsWith("#/b/") ? "#/" : hash), view);
  if (keep) app.prepend(keep);
}

window.addEventListener("hashchange", render);
render();
