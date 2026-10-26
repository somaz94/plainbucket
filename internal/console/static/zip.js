// Builds an uncompressed (stored) ZIP archive piece by piece: each entry's header is
// written once its data, size and CRC-32 are known, and ZIP64 records are added only
// where a size, an offset or the entry count needs them.

const table = new Uint32Array(256);
for (let n = 0; n < 256; n++) {
  let c = n;
  for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
  table[n] = c >>> 0;
}

// crc32 continues crc over bytes; start with 0.
export function crc32(bytes, crc = 0) {
  let c = ~crc >>> 0;
  for (let i = 0; i < bytes.length; i++) c = table[(c ^ bytes[i]) & 0xff] ^ (c >>> 8);
  return ~c >>> 0;
}

const max32 = 0xffffffff;
const max16 = 0xffff;

class Bytes {
  constructor(size) {
    this.buf = new Uint8Array(size);
    this.view = new DataView(this.buf.buffer);
    this.at = 0;
  }
  u16(v) { this.view.setUint16(this.at, v, true); this.at += 2; return this; }
  u32(v) { this.view.setUint32(this.at, v, true); this.at += 4; return this; }
  u64(v) { this.view.setBigUint64(this.at, BigInt(v), true); this.at += 8; return this; }
  raw(b) { this.buf.set(b, this.at); this.at += b.length; return this; }
}

// entryName returns an archive path for key bytes. A backslash or a colon may sit inside an
// S3 key segment, but Windows extractors read them as a separator or a drive, which could
// put a file outside the extraction directory.
export function entryName(bytes) {
  return bytes.map((b) => (b === 0x5c || b === 0x3a ? 0x5f : b));
}

// dosTime packs a Date into the MS-DOS time and date fields, which cover 1980 to 2107.
function dosTime(d) {
  if (isNaN(d) || d.getFullYear() < 1980) d = new Date(1980, 0, 1);
  else if (d.getFullYear() > 2107) d = new Date(2107, 11, 31, 23, 59, 58);
  return {
    time: (d.getHours() << 11) | (d.getMinutes() << 5) | (d.getSeconds() >> 1),
    date: ((d.getFullYear() - 1980) << 9) | ((d.getMonth() + 1) << 5) | d.getDate(),
  };
}

export class ZipBuilder {
  // forceZip64 writes ZIP64 records even where they are not needed, for tests.
  constructor({ forceZip64 = false } = {}) {
    this.offset = 0;
    this.entries = [];
    this.force = forceZip64;
  }

  // header returns the local header of the next entry, to be followed by its size bytes
  // of data. name is the path's bytes; a directory's ends in "/" and has no data.
  header({ name, utf8 = true, modified = new Date(), size = 0, crc = 0 }) {
    const dir = name[name.length - 1] === 0x2f;
    const zip64 = this.force || size >= max32 || this.offset >= max32;
    const e = { name, utf8, dir, size, crc, offset: this.offset, zip64, ...dosTime(modified) };
    const b = new Bytes(30 + name.length + (zip64 ? 20 : 0));
    b.u32(0x04034b50).u16(zip64 ? 45 : 20).u16(utf8 ? 0x0800 : 0).u16(0).u16(e.time).u16(e.date).u32(crc);
    if (zip64) {
      b.u32(max32).u32(max32).u16(name.length).u16(20).raw(name).u16(0x0001).u16(16).u64(size).u64(size);
    } else {
      b.u32(size).u32(size).u16(name.length).u16(0).raw(name);
    }
    this.entries.push(e);
    this.offset += b.buf.length + size;
    return b.buf;
  }

  // end returns the central directory and the end records.
  end() {
    const parts = [];
    const start = this.offset;
    for (const e of this.entries) {
      const big = e.zip64;
      const farOffset = this.force || e.offset >= max32;
      const extra = big ? 16 + (farOffset ? 8 : 0) : farOffset ? 8 : 0;
      const b = new Bytes(46 + e.name.length + (extra ? 4 + extra : 0));
      b.u32(0x02014b50).u16((3 << 8) | 45).u16(big || farOffset ? 45 : 20).u16(e.utf8 ? 0x0800 : 0).u16(0)
        .u16(e.time).u16(e.date).u32(e.crc)
        .u32(big ? max32 : e.size).u32(big ? max32 : e.size)
        .u16(e.name.length).u16(extra ? 4 + extra : 0).u16(0).u16(0).u16(0)
        .u32(((e.dir ? 0o40755 : 0o100644) << 16 | (e.dir ? 0x10 : 0)) >>> 0)
        .u32(farOffset ? max32 : e.offset).raw(e.name);
      if (extra) {
        b.u16(0x0001).u16(extra);
        if (big) b.u64(e.size).u64(e.size);
        if (farOffset) b.u64(e.offset);
      }
      parts.push(b.buf);
    }
    const size = parts.reduce((n, p) => n + p.length, 0);
    const count = this.entries.length;
    if (this.force || count >= max16 || start >= max32 || size >= max32) {
      const end64 = start + size;
      parts.push(new Bytes(56).u32(0x06064b50).u64(44).u16((3 << 8) | 45).u16(45).u32(0).u32(0)
        .u64(count).u64(count).u64(size).u64(start).buf);
      parts.push(new Bytes(20).u32(0x07064b50).u32(0).u64(end64).u32(1).buf);
      parts.push(new Bytes(22).u32(0x06054b50).u16(0).u16(0).u16(max16).u16(max16).u32(max32).u32(max32).u16(0).buf);
    } else {
      parts.push(new Bytes(22).u32(0x06054b50).u16(0).u16(0).u16(count).u16(count).u32(size).u32(start).u16(0).buf);
    }
    return parts;
  }
}
