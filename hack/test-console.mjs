// Drives the console's signer and key handling (internal/console/static/{sigv4,keys}.js)
// against a running server: header-signed and presigned requests, multipart, keys XML
// cannot carry, the usage API and key scopes.
// Run by hack/test-console.sh, which starts the server and sets the environment.
import { createHash } from "node:crypto";
import * as keys from "../internal/console/static/keys.js";
import * as sigv4 from "../internal/console/static/sigv4.js";

const endpoint = new URL(process.env.ENDPOINT);
const host = endpoint.host;
const admin = { accessKey: process.env.ACCESS_KEY, secretKey: process.env.SECRET_KEY, region: "us-east-1" };
const scoped = { accessKey: "console-scoped", secretKey: "console-scoped-secret", region: "us-east-1" };
let passed = 0;

function ok(name) {
  passed++;
  console.log("  ok  " + name);
}

function fail(name, detail) {
  console.error("  FAIL " + name + ": " + detail);
  process.exit(1);
}

async function s3(cred, method, bucket, key = "", { params = [], headers = {}, body } = {}) {
  const path = bucket === "" ? "/" : sigv4.encodePath(bucket, key);
  const query = sigv4.queryString(params);
  const signed = sigv4.signHeaders(cred, { method, host, path, query, headers });
  return fetch(endpoint.origin + path + (query ? "?" + query : ""), { method, headers: signed, body });
}

async function expectStatus(name, res, want) {
  if (res.status !== want) fail(name, `status ${res.status}, want ${want}: ${await res.text()}`);
  ok(name);
  return res;
}

const sha = (b) => createHash("sha256").update(b).digest("hex");

await expectStatus("create bucket", await s3(admin, "PUT", "console-e2e"), 200);
const list = await (await expectStatus("list buckets", await s3(admin, "GET", ""), 200)).text();
if (!list.includes("<Name>console-e2e</Name>")) fail("list buckets", list);

const key = "dir with space/ünïcode+plus.txt";
await expectStatus("put object", await s3(admin, "PUT", "console-e2e", key, {
  headers: { "content-type": "text/plain", "x-amz-meta-origin": "console" }, body: "hello console",
}), 200);
const head = await expectStatus("head object", await s3(admin, "HEAD", "console-e2e", key), 200);
if (head.headers.get("x-amz-meta-origin") !== "console") fail("head metadata", [...head.headers].join());

const listed = await (await expectStatus("list objects", await s3(admin, "GET", "console-e2e", "", {
  params: [["list-type", "2"], ["delimiter", "/"], ["prefix", "dir with space/"]],
}), 200)).text();
if (!listed.includes("<Key>dir with space/ünïcode+plus.txt</Key>")) fail("list objects", listed);

// Multipart: two 5 MiB parts and a short last one, uploaded concurrently.
const data = Buffer.alloc(11 * 1024 * 1024);
for (let i = 0; i < data.length; i++) data[i] = (i * 31) & 0xff;
const created = await (await expectStatus("create upload", await s3(admin, "POST", "console-e2e", "big.bin", {
  params: [["uploads", ""]],
}), 200)).text();
const uploadId = created.match(/<UploadId>([^<]+)</)[1];
const bounds = [[0, 5 << 20], [5 << 20, 10 << 20], [10 << 20, data.length]];
const etags = [];
await Promise.all(bounds.map(async ([a, b], i) => {
  const res = await s3(admin, "PUT", "console-e2e", "big.bin", {
    params: [["partNumber", String(i + 1)], ["uploadId", uploadId]], body: data.subarray(a, b),
  });
  if (res.status !== 200) fail("upload part " + (i + 1), await res.text());
  etags[i] = res.headers.get("etag");
}));
ok("upload parts");
const doc = "<CompleteMultipartUpload>" +
  etags.map((e, i) => `<Part><PartNumber>${i + 1}</PartNumber><ETag>${e}</ETag></Part>`).join("") +
  "</CompleteMultipartUpload>";
await expectStatus("complete upload", await s3(admin, "POST", "console-e2e", "big.bin", {
  params: [["uploadId", uploadId]], body: doc,
}), 200);

// Presigned download with the attachment override the console's download button uses.
const dl = sigv4.presign(admin, {
  method: "GET", host, path: sigv4.encodePath("console-e2e", "big.bin"), expires: 60,
  params: [["response-content-disposition", 'attachment; filename="big.bin"']],
});
const got = await expectStatus("presigned get", await fetch(`${endpoint.origin}${sigv4.encodePath("console-e2e", "big.bin")}?${dl}`), 200);
if (got.headers.get("content-disposition") !== 'attachment; filename="big.bin"') fail("disposition", got.headers.get("content-disposition"));
if (sha(Buffer.from(await got.arrayBuffer())) !== sha(data)) fail("presigned get", "body differs");
ok("multipart round trip");

const shared = sigv4.presign(admin, { method: "GET", host, path: sigv4.encodePath("console-e2e", key), expires: 3600 });
const sharedRes = await expectStatus("presigned get of an encoded key", await fetch(`${endpoint.origin}${sigv4.encodePath("console-e2e", key)}?${shared}`), 200);
if ((await sharedRes.text()) !== "hello console") fail("shared body", "differs");

// Rename and metadata edits as the console sends them: a copy that never overwrites, and
// a copy onto itself guarded by the ETag the dialog was opened with.
const moved = "moved/ü.txt";
const copyHeaders = { "x-amz-copy-source": sigv4.encodePath("console-e2e", key), "if-none-match": "*" };
await expectStatus("copy to a new name", await s3(admin, "PUT", "console-e2e", moved, { headers: copyHeaders }), 200);
await expectStatus("copy never overwrites", await s3(admin, "PUT", "console-e2e", moved, { headers: copyHeaders }), 412);
const etag = (await s3(admin, "HEAD", "console-e2e", moved)).headers.get("etag");
const replace = (ifMatch) => s3(admin, "PUT", "console-e2e", moved, { headers: {
  "x-amz-copy-source": sigv4.encodePath("console-e2e", moved), "x-amz-metadata-directive": "REPLACE",
  "x-amz-copy-source-if-match": ifMatch, "content-type": "text/markdown", "cache-control": "no-cache", "x-amz-meta-note": "renamed",
} });
await expectStatus("replace metadata", await replace(etag), 200);
const edited = await s3(admin, "HEAD", "console-e2e", moved);
if (edited.headers.get("content-type") !== "text/markdown" || edited.headers.get("cache-control") !== "no-cache" ||
  edited.headers.get("x-amz-meta-note") !== "renamed") fail("replaced metadata", [...edited.headers].join());
await expectStatus("stale metadata edit refused", await replace('"00000000000000000000000000000000"'), 412);
await expectStatus("delete the moved copy", await s3(admin, "DELETE", "console-e2e", moved), 204);

// Objects on the console's host run sandboxed, away from the console's origin.
const csp = (await s3(admin, "GET", "console-e2e", key)).headers.get("content-security-policy") ?? "";
if (!csp.startsWith("sandbox")) fail("object sandbox", csp);
ok("object sandbox");

// A "\r" in DeleteObjects XML must arrive as "\r", not be normalized to the "\n" key.
for (const k of ["cr\rkey", "cr\nkey"]) {
  await expectStatus("put " + JSON.stringify(k), await s3(admin, "PUT", "console-e2e", k, { body: "x" }), 200);
}
const crDoc = `<Delete><Object><Key>${keys.xmlKey(keys.encodeKey("cr\rkey"))}</Key></Object></Delete>`;
await expectStatus("delete a key with \\r", await s3(admin, "POST", "console-e2e", "", { params: [["delete", ""]], body: crDoc }), 200);
await expectStatus("\\r key deleted", await s3(admin, "HEAD", "console-e2e", "cr\rkey"), 404);
await expectStatus("\\n key kept", await s3(admin, "HEAD", "console-e2e", "cr\nkey"), 200);
await expectStatus("delete the \\n key", await s3(admin, "DELETE", "console-e2e", "cr\nkey"), 204);

// A key that is not UTF-8 keeps its bytes through the listing's encoded form. Some file
// systems (APFS) refuse such names, so the server may not be able to store one.
const raw = "ff%FFkey";
const rawPut = await s3(admin, "PUT", "console-e2e", keys.keyBytes(raw), { body: "x" });
if (rawPut.status === 200) {
  const rawList = await (await s3(admin, "GET", "console-e2e", "", {
    params: [["list-type", "2"], ["prefix", keys.keyBytes("ff")], ["encoding-type", "url"]],
  })).text();
  if (!rawList.includes(`<Key>${raw}</Key>`)) fail("list a non-UTF-8 key", rawList);
  if (keys.xmlKey(raw) !== null || keys.displayKey(raw) !== "ff\uFFFDkey") fail("non-UTF-8 key", keys.displayKey(raw));
  await expectStatus("delete a non-UTF-8 key", await s3(admin, "DELETE", "console-e2e", keys.keyBytes(raw)), 204);
} else {
  console.log(`  skip non-UTF-8 key: the server answered ${rawPut.status}`);
}

const usagePath = "/_console/api/usage";
const usage = await (await expectStatus("usage api", await fetch(endpoint.origin + usagePath, {
  headers: sigv4.signHeaders(admin, { method: "GET", host, path: usagePath }),
}), 200)).json();
if (!usage.total || !usage.buckets.some((b) => b.name === "console-e2e")) fail("usage api", JSON.stringify(usage));
const presignedUsage = sigv4.presign(admin, { method: "GET", host, path: usagePath, expires: 60 });
await expectStatus("presigned usage api", await fetch(`${endpoint.origin}${usagePath}?${presignedUsage}`), 200);

const scopedUsage = await (await expectStatus("scoped usage api", await fetch(endpoint.origin + usagePath, {
  headers: sigv4.signHeaders(scoped, { method: "GET", host, path: usagePath }),
}), 200)).json();
if (scopedUsage.total || scopedUsage.buckets.some((b) => b.name === "console-e2e")) fail("scoped usage", JSON.stringify(scopedUsage));
await expectStatus("scoped key denied", await s3(scoped, "GET", "console-e2e", "big.bin"), 403);

const del = "<Delete><Object><Key>big.bin</Key></Object><Object><Key>" + key + "</Key></Object></Delete>";
await expectStatus("delete objects", await s3(admin, "POST", "console-e2e", "", { params: [["delete", ""]], body: del }), 200);
await expectStatus("delete bucket", await s3(admin, "DELETE", "console-e2e"), 204);

console.log(`all ${passed} checks passed`);
