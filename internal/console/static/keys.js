// Keys are kept in the form a listing with encoding-type=url returns them, S3's URI
// encoding with "/" left as is. Unlike a decoded string it keeps keys that are not UTF-8.
import { uriEncode } from "./sigv4.js";

const utf8 = new TextEncoder();
const strict = new TextDecoder("utf-8", { fatal: true });
const lossy = new TextDecoder();

// s is a raw (typed or dropped) name, not yet encoded.
export function encodeKey(s) {
  return uriEncode(s, false);
}

// keyBytes returns the bytes of an encoded key; it throws on a malformed escape.
export function keyBytes(enc) {
  const out = [];
  for (let i = 0; i < enc.length;) {
    if (enc[i] === "%") {
      const h = enc.slice(i + 1, i + 3);
      if (!/^[0-9A-Fa-f]{2}$/.test(h)) throw new URIError("Malformed key: " + enc);
      out.push(parseInt(h, 16));
      i += 3;
    } else {
      const c = String.fromCodePoint(enc.codePointAt(i));
      out.push(...utf8.encode(c));
      i += c.length;
    }
  }
  return Uint8Array.from(out);
}

// canonicalKey re-encodes an encoded key as the server lists it.
export function canonicalKey(enc) {
  return uriEncode(keyBytes(enc), false);
}

// keyText decodes a key, or returns null if it is not UTF-8.
export function keyText(enc) {
  try {
    return strict.decode(keyBytes(enc));
  } catch {
    return null;
  }
}

// displayKey shows a key to the user without ever throwing: bytes that are not UTF-8
// become U+FFFD, and control characters their visible symbols.
export function displayKey(enc) {
  let s;
  try {
    s = keyText(enc) ?? lossy.decode(keyBytes(enc));
  } catch {
    s = enc;
  }
  return s.replace(/[\u0000-\u001F\u007F]/g, (c) => (c === "\u007F" ? "␡" : String.fromCharCode(0x2400 + c.charCodeAt(0))));
}

// escapeXML escapes text, and the whitespace an XML parser would otherwise normalize.
export function escapeXML(s) {
  return s.replace(/[<>&"'\t\n\r]/g, (c) => `&#${c.charCodeAt(0)};`);
}

// xmlKey returns a key as XML text, or null if XML 1.0 cannot carry it.
export function xmlKey(enc) {
  const s = keyText(enc);
  if (s === null || /[\u0000-\u0008\u000B\u000C\u000E-\u001F￾￿]/.test(s)) return null;
  return escapeXML(s);
}
