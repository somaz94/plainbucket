// Upload links: a browser POST policy, signed with a key in the console, carried in the
// fragment of a console URL. Only the signed fields travel; what the upload page shows is
// read back from the policy, so a link edited by hand cannot promise more than the server
// will accept.
import { base64, postFields } from "./sigv4.js";

// S3 caps a POST upload at 5 GiB.
export const maxLinkBytes = 5 * 1024 * 1024 * 1024;
const prefix = "#/u/";
const signed = ["policy", "x-amz-algorithm", "x-amz-credential", "x-amz-date", "x-amz-signature"];

function base64url(text) {
  return base64(new TextEncoder().encode(text)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function decode(b64) {
  const bin = atob(b64.replace(/-/g, "+").replace(/_/g, "/"));
  return new TextDecoder("utf-8", { fatal: true }).decode(Uint8Array.from(bin, (c) => c.charCodeAt(0)));
}

// makeLink returns the fragment of a link that uploads into folder of bucket for seconds,
// files of up to max bytes. attachment makes them download, not open, when served, which
// keeps uploads to a public bucket from becoming pages on its host.
export function makeLink(cred, { bucket, folder, max, seconds, attachment = false, date = new Date() }) {
  const conditions = [{ bucket }, ["starts-with", "$key", folder], ["starts-with", "$Content-Type", ""], ["content-length-range", 0, max]];
  if (attachment) conditions.push({ "content-disposition": "attachment" });
  return prefix + base64url(JSON.stringify(postFields(cred, { conditions, expires: seconds, date })));
}

// parseLink reads a fragment made by makeLink, or returns null when it is damaged.
export function parseLink(hash) {
  try {
    if (!hash.startsWith(prefix)) return null;
    const raw = JSON.parse(decode(hash.slice(prefix.length)));
    const fields = {};
    for (const name of signed) {
      if (typeof raw[name] !== "string" || !raw[name]) return null;
      fields[name] = raw[name];
    }
    const policy = JSON.parse(decode(fields.policy));
    const out = { fields, expires: new Date(policy.expiration), attachment: false };
    for (const c of policy.conditions ?? []) {
      if (Array.isArray(c) && c[0] === "starts-with" && c[1] === "$key") out.folder = c[2];
      else if (Array.isArray(c) && c[0] === "content-length-range") out.max = c[2];
      else if (c && typeof c.bucket === "string") out.bucket = c.bucket;
      else if (c && c["content-disposition"] === "attachment") out.attachment = true;
    }
    const ok = typeof out.bucket === "string" && out.bucket && typeof out.folder === "string" &&
      Number.isSafeInteger(out.max) && out.max > 0 && !Number.isNaN(out.expires.getTime());
    return ok ? out : null;
  } catch {
    return null;
  }
}
