package storage

import (
	"context"
	"crypto/md5" //nolint:gosec // the ETag is an MD5
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/somaz94/plainbucket/internal/checksum"
)

// Corruption is an object whose data no longer matches a digest recorded when it was
// written, or that can no longer be read. What is "etag", a checksum algorithm, or "read".
type Corruption struct {
	Bucket, Key string
	What        string
	Want, Got   string
}

// ScrubResult sums up a Scrub.
type ScrubResult struct {
	Objects int64 `json:"objects"` // verified
	Bytes   int64 `json:"bytes"`
	// Skipped objects have no trusted metadata (fsck reports those) or changed while read.
	Skipped int64 `json:"skipped"`
	// Unchecked objects record nothing to compare their data with: a composite checksum
	// alone, or a multipart ETag without part sizes (completed by v0.1.x).
	Unchecked int64     `json:"unchecked"`
	Corrupt   int64     `json:"corrupt"`
	Finished  time.Time `json:"finished"`
}

const (
	scrubChunk = 1 << 20
	scrubFile  = "scrub.json"
)

// scrubRead reads object data; tests replace it to inject read errors and races.
var scrubRead = func(f *os.File, b []byte) (int, error) { return f.Read(b) }

// Scrub reads every object with trusted metadata again and compares its data with the
// ETag and full-object checksums recorded when it was written, reporting each mismatch
// and each object that cannot be read. rate caps the bytes read per second, 0 for no
// cap, so a running server keeps its disk for requests. A completed run is kept, across
// restarts too, for LastScrub.
func (s *Store) Scrub(ctx context.Context, rate int64, report func(Corruption)) (ScrubResult, error) {
	var res ScrubResult
	buckets, err := s.ListBuckets()
	if err != nil {
		return res, err
	}
	pace := pacer{rate: rate}
	buf := make([]byte, scrubChunk)
	for _, b := range buckets {
		root := s.bucketPath(b.Name)
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil || !d.Type().IsRegular() {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			key := filepath.ToSlash(rel)
			if checkKey(key) != nil {
				return nil
			}
			out, n, bad, err := s.verify(ctx, b.Name, key, &pace, buf)
			if err != nil {
				return err // canceled
			}
			switch out {
			case scrubSkipped:
				res.Skipped++
			case scrubUnchecked:
				res.Unchecked++
			case scrubVerified:
				res.Objects++
				res.Bytes += n
				if bad != nil {
					res.Corrupt++
					report(*bad)
				}
			}
			return nil
		})
		if err != nil {
			return res, err
		}
	}
	res.Finished = s.now()
	s.lastScrub.Store(&res)
	if raw, err := json.Marshal(res); err == nil {
		_ = s.writeFileAtomic(s.sys(scrubFile), raw)
	}
	return res, nil
}

// LastScrub returns the result of the last completed Scrub, or false before the first.
func (s *Store) LastScrub() (ScrubResult, bool) {
	if r := s.lastScrub.Load(); r != nil {
		return *r, true
	}
	return ScrubResult{}, false
}

// loadScrub restores the last result a previous process saved.
func (s *Store) loadScrub() {
	raw, err := os.ReadFile(s.sys(scrubFile))
	if err != nil {
		return
	}
	var res ScrubResult
	if json.Unmarshal(raw, &res) == nil && !res.Finished.IsZero() {
		s.lastScrub.Store(&res)
	}
}

type scrubOutcome int

const (
	scrubSkipped scrubOutcome = iota
	scrubUnchecked
	scrubVerified
)

// verify hashes one object. Only a canceled ctx is returned as an error: any other
// failure to open or stat it means it is gone or changing, while a failure to read data
// whose metadata was trusted is a finding.
func (s *Store) verify(ctx context.Context, bucket, key string, pace *pacer, buf []byte) (scrubOutcome, int64, *Corruption, error) {
	f, err := s.r.Open(s.rel(s.dataPath(bucket, key)))
	if err != nil {
		return scrubSkipped, 0, nil, nil
	}
	defer func() { _ = f.Close() }()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return scrubSkipped, 0, nil, nil
	}
	m, trusted := s.readSidecar(bucket, key)
	if !trusted || m.Dir || !m.matches(before) {
		return scrubSkipped, 0, nil, nil
	}
	h := newObjectHashes(m)
	if !h.checks() {
		return scrubUnchecked, 0, nil, nil
	}

	var n int64
	for {
		k, rerr := scrubRead(f, buf)
		if k > 0 {
			h.write(buf[:k])
			n += int64(k)
			if err := pace.wait(ctx, int64(k)); err != nil {
				return scrubSkipped, n, nil, err
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			if !s.unchanged(bucket, key, before, m) {
				return scrubSkipped, n, nil, nil
			}
			return scrubVerified, n, &Corruption{Bucket: bucket, Key: key, What: "read", Want: "readable", Got: rerr.Error()}, nil
		}
	}
	if n != m.Size || !s.unchanged(bucket, key, before, m) {
		return scrubSkipped, n, nil, nil
	}
	if what, want, got, bad := h.mismatch(); bad {
		return scrubVerified, n, &Corruption{Bucket: bucket, Key: key, What: what, Want: want, Got: got}, nil
	}
	return scrubVerified, n, nil, nil
}

// unchanged reports whether the file read is still the object, with the same metadata.
// It looks under the key lock, which a write holds across placing data and sidecar: on
// a filesystem with coarse mtimes the old sidecar can otherwise match new data.
func (s *Store) unchanged(bucket, key string, before os.FileInfo, m sidecar) bool {
	unlock := s.lock(bucket, key)
	defer unlock()
	cur, err := s.r.Stat(s.rel(s.dataPath(bucket, key)))
	if err != nil || !os.SameFile(before, cur) || !m.matches(cur) {
		return false
	}
	now, ok := s.readSidecar(bucket, key)
	return ok && now.ETag == m.ETag && now.matches(cur)
}

// objectHashes computes what the sidecar recorded: the ETag, from the whole object or
// from its parts, and the full-object checksums.
type objectHashes struct {
	m        sidecar
	whole    hash.Hash // a single-part ETag
	byParts  bool      // a multipart ETag, rebuilt from the recorded part sizes
	parts    []int64   // part sizes still to come
	part     hash.Hash
	partLeft int64
	partSums []byte
	sums     map[string]hash.Hash
}

func newObjectHashes(m sidecar) *objectHashes {
	h := &objectHashes{m: m, sums: map[string]hash.Hash{}}
	etag := trimETag(m.ETag)
	if count, multi := multipartCount(etag); !multi && len(etag) == 2*md5.Size {
		h.whole = md5.New() //nolint:gosec // the ETag is an MD5
	} else if multi && count > 0 && count == len(m.Parts) && partsTotal(m.Parts) == m.Size {
		h.byParts = true
		for _, p := range m.Parts {
			h.parts = append(h.parts, p.Size)
		}
		h.nextPart()
	}
	// A composite checksum is a digest of part digests; only full-object ones are re-read.
	if m.ChecksumType != ChecksumComposite {
		for alg := range m.Checksums {
			if sum, ok := checksum.New(alg); ok {
				h.sums[alg] = sum
			}
		}
	}
	return h
}

func (h *objectHashes) checks() bool { return h.whole != nil || h.byParts || len(h.sums) > 0 }

func partsTotal(parts []PartRecord) int64 {
	var n int64
	for _, p := range parts {
		n += p.Size
	}
	return n
}

// multipartCount reads the part count of a multipart ETag ("<md5>-<parts>").
func multipartCount(etag string) (int, bool) {
	_, count, ok := strings.Cut(etag, "-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(count)
	return n, err == nil
}

func (h *objectHashes) nextPart() {
	if h.part != nil {
		h.partSums = h.part.Sum(h.partSums)
		h.part = nil
	}
	if len(h.parts) > 0 {
		h.part, h.partLeft, h.parts = md5.New(), h.parts[0], h.parts[1:] //nolint:gosec // the ETag is an MD5
	}
}

func (h *objectHashes) write(b []byte) {
	if h.whole != nil {
		h.whole.Write(b)
	}
	for _, sum := range h.sums {
		sum.Write(b)
	}
	for len(b) > 0 && h.part != nil {
		k := min(int64(len(b)), h.partLeft)
		h.part.Write(b[:k])
		h.partLeft -= k
		b = b[k:]
		if h.partLeft == 0 {
			h.nextPart()
		}
	}
}

// mismatch reports the first recorded digest the data does not match.
func (h *objectHashes) mismatch() (what, want, got string, bad bool) {
	var etag string
	switch {
	case h.whole != nil:
		etag = md5ETag(h.whole.Sum(nil))
	case h.byParts:
		for h.part != nil {
			h.nextPart() // also parts of size 0 the data never reached
		}
		sum := md5.Sum(h.partSums) //nolint:gosec // the ETag is an MD5
		etag = `"` + hex.EncodeToString(sum[:]) + "-" + strconv.Itoa(len(h.m.Parts)) + `"`
	}
	if etag != "" && trimETag(etag) != trimETag(h.m.ETag) {
		return "etag", h.m.ETag, etag, true
	}
	for alg, sum := range h.sums {
		if v := checksum.Encode(sum.Sum(nil)); v != h.m.Checksums[alg] {
			return alg, h.m.Checksums[alg], v, true
		}
	}
	return "", "", "", false
}

// pacer holds reads to rate bytes per second. Time spent not reading earns no burst, so
// a large file after a long walk is read no faster than the rest.
type pacer struct {
	rate int64
	next time.Time // when the bytes read so far are paid for
}

func (p *pacer) wait(ctx context.Context, n int64) error {
	if p.rate <= 0 {
		return ctx.Err()
	}
	if now := time.Now(); p.next.Before(now) {
		p.next = now
	}
	p.next = p.next.Add(time.Duration(float64(n) / float64(p.rate) * float64(time.Second)))
	d := time.Until(p.next)
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
