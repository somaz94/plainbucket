// AWS Signature Version 4 for S3, with SHA-256 and HMAC in plain JavaScript:
// crypto.subtle exists only in secure contexts, and the console also runs over plain HTTP.

const K = new Uint32Array([
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
]);

const encoder = new TextEncoder();

function bytes(data) {
  return typeof data === "string" ? encoder.encode(data) : data;
}

// sha256 returns the digest of a string (as UTF-8) or a Uint8Array.
export function sha256(data) {
  const msg = bytes(data);
  const bitLen = msg.length * 8;
  const padded = new Uint8Array(((msg.length + 9 + 63) >> 6) << 6);
  padded.set(msg);
  padded[msg.length] = 0x80;
  const view = new DataView(padded.buffer);
  view.setUint32(padded.length - 8, Math.floor(bitLen / 0x100000000));
  view.setUint32(padded.length - 4, bitLen >>> 0);

  const h = new Uint32Array([
    0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
  ]);
  const w = new Uint32Array(64);
  const rotr = (x, n) => (x >>> n) | (x << (32 - n));
  for (let off = 0; off < padded.length; off += 64) {
    for (let i = 0; i < 16; i++) w[i] = view.getUint32(off + i * 4);
    for (let i = 16; i < 64; i++) {
      const s0 = rotr(w[i - 15], 7) ^ rotr(w[i - 15], 18) ^ (w[i - 15] >>> 3);
      const s1 = rotr(w[i - 2], 17) ^ rotr(w[i - 2], 19) ^ (w[i - 2] >>> 10);
      w[i] = w[i - 16] + s0 + w[i - 7] + s1;
    }
    let [a, b, c, d, e, f, g, k] = h;
    for (let i = 0; i < 64; i++) {
      const t1 = k + (rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25)) + ((e & f) ^ (~e & g)) + K[i] + w[i];
      const t2 = (rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22)) + ((a & b) ^ (a & c) ^ (b & c));
      k = g; g = f; f = e; e = (d + t1) >>> 0;
      d = c; c = b; b = a; a = (t1 + t2) >>> 0;
    }
    h[0] += a; h[1] += b; h[2] += c; h[3] += d; h[4] += e; h[5] += f; h[6] += g; h[7] += k;
  }
  const out = new Uint8Array(32);
  const outView = new DataView(out.buffer);
  h.forEach((v, i) => outView.setUint32(i * 4, v));
  return out;
}

export function hmacSHA256(key, data) {
  let k = bytes(key);
  if (k.length > 64) k = sha256(k);
  const block = new Uint8Array(64);
  block.set(k);
  const inner = new Uint8Array(64);
  const outer = new Uint8Array(64);
  for (let i = 0; i < 64; i++) {
    inner[i] = block[i] ^ 0x36;
    outer[i] = block[i] ^ 0x5c;
  }
  const msg = bytes(data);
  const first = new Uint8Array(64 + msg.length);
  first.set(inner);
  first.set(msg, 64);
  const second = new Uint8Array(64 + 32);
  second.set(outer);
  second.set(sha256(first), 64);
  return sha256(second);
}

export function hex(buf) {
  return Array.from(buf, (b) => b.toString(16).padStart(2, "0")).join("");
}

// uriEncode is S3's encoding: every byte but A-Z a-z 0-9 - . _ ~ as %XX, and "/" too
// unless encodeSlash is false. s is a string, encoded as UTF-8, or a Uint8Array.
export function uriEncode(s, encodeSlash = true) {
  let out = "";
  for (const b of typeof s === "string" ? encoder.encode(s) : s) {
    const c = String.fromCharCode(b);
    if (/[A-Za-z0-9\-._~]/.test(c) || (c === "/" && !encodeSlash)) out += c;
    else out += "%" + b.toString(16).toUpperCase().padStart(2, "0");
  }
  return out;
}

// queryString renders [[name, value], ...] sorted and encoded, which is both the
// canonical form and what the request sends.
export function queryString(params) {
  return params
    .map(([k, v]) => [uriEncode(k), uriEncode(v ?? "")])
    .sort((x, y) => (x[0] < y[0] ? -1 : x[0] > y[0] ? 1 : x[1] < y[1] ? -1 : x[1] > y[1] ? 1 : 0))
    .map(([k, v]) => k + "=" + v)
    .join("&");
}

function amzDate(date) {
  return date.toISOString().replace(/[-:]/g, "").replace(/\.\d{3}/, "");
}

function signingKey(secretKey, day, region) {
  let k = hmacSHA256("AWS4" + secretKey, day);
  k = hmacSHA256(k, region);
  k = hmacSHA256(k, "s3");
  return hmacSHA256(k, "aws4_request");
}

function signature(cred, method, path, query, headers, signedHeaders, payload, stamp) {
  const canonicalHeaders = signedHeaders.map((n) => n + ":" + String(headers[n]).trim() + "\n").join("");
  const canonical = [method, path, query, canonicalHeaders, signedHeaders.join(";"), payload].join("\n");
  const scope = `${stamp.slice(0, 8)}/${cred.region}/s3/aws4_request`;
  const toSign = ["AWS4-HMAC-SHA256", stamp, scope, hex(sha256(canonical))].join("\n");
  return { scope, sig: hex(hmacSHA256(signingKey(cred.secretKey, stamp.slice(0, 8), cred.region), toSign)) };
}

// signHeaders returns the headers to send with a request: the given ones plus x-amz-date,
// x-amz-content-sha256 (UNSIGNED-PAYLOAD) and Authorization. It signs host and every
// x-amz-* header, as the server requires. path must already be encoded (encodePath).
export function signHeaders(cred, { method, host, path, query = "", headers = {}, date = new Date() }) {
  const stamp = amzDate(date);
  const all = {};
  for (const [k, v] of Object.entries(headers)) all[k.toLowerCase()] = v;
  all["x-amz-date"] = stamp;
  all["x-amz-content-sha256"] = "UNSIGNED-PAYLOAD";
  const toSign = { ...all, host };
  const signed = Object.keys(toSign).filter((n) => n === "host" || n.startsWith("x-amz-")).sort();
  const { scope, sig } = signature(cred, method, path, query, toSign, signed, "UNSIGNED-PAYLOAD", stamp);
  all.authorization = `AWS4-HMAC-SHA256 Credential=${cred.accessKey}/${scope}, SignedHeaders=${signed.join(";")}, Signature=${sig}`;
  return all;
}

// presign returns the query string of a presigned URL valid for expires seconds; params
// are extra [[name, value]] pairs, such as response-content-disposition.
export function presign(cred, { method, host, path, params = [], expires, date = new Date() }) {
  const stamp = amzDate(date);
  const scope = `${stamp.slice(0, 8)}/${cred.region}/s3/aws4_request`;
  const q = [
    ...params,
    ["X-Amz-Algorithm", "AWS4-HMAC-SHA256"],
    ["X-Amz-Credential", `${cred.accessKey}/${scope}`],
    ["X-Amz-Date", stamp],
    ["X-Amz-Expires", String(expires)],
    ["X-Amz-SignedHeaders", "host"],
  ];
  const query = queryString(q);
  const { sig } = signature(cred, method, path, query, { host }, ["host"], "UNSIGNED-PAYLOAD", stamp);
  return query + "&X-Amz-Signature=" + sig;
}

// encodePath builds "/bucket/key" with each segment encoded as S3 signs it; key is a
// string or the key's bytes.
export function encodePath(bucket, key = "") {
  let p = "/" + uriEncode(bucket);
  if (key.length) p += "/" + uriEncode(key, false);
  return p;
}

export function base64(bytesIn) {
  let s = "";
  for (let i = 0; i < bytesIn.length; i += 0x8000) s += String.fromCharCode(...bytesIn.subarray(i, i + 0x8000));
  return btoa(s);
}

// postFields returns the signed fields of a browser POST upload: a policy that expires in
// expires seconds with the given conditions, which must cover every other field sent.
export function postFields(cred, { conditions, expires, date = new Date() }) {
  const stamp = amzDate(date);
  const credential = `${cred.accessKey}/${stamp.slice(0, 8)}/${cred.region}/s3/aws4_request`;
  const policy = base64(encoder.encode(JSON.stringify({
    expiration: new Date(date.getTime() + expires * 1000).toISOString(),
    conditions: [...conditions,
      { "x-amz-algorithm": "AWS4-HMAC-SHA256" }, { "x-amz-credential": credential }, { "x-amz-date": stamp }],
  })));
  return {
    "x-amz-algorithm": "AWS4-HMAC-SHA256",
    "x-amz-credential": credential,
    "x-amz-date": stamp,
    policy,
    "x-amz-signature": hex(hmacSHA256(signingKey(cred.secretKey, stamp.slice(0, 8), cred.region), policy)),
  };
}
