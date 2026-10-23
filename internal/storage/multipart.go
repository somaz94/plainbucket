package storage

import (
	"context"
	"crypto/md5" //nolint:gosec // S3 ETag format
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	maxPartNumber  = 10000
	uploadMetaFile = "upload.json"
)

var (
	uploadIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

	ErrInvalidPartNumber = errors.New("part number must be between 1 and 10000")
)

type uploadMeta struct {
	Bucket    string            `json:"bucket"`
	Key       string            `json:"key"`
	Initiated time.Time         `json:"initiated"`
	Checksum  MultipartChecksum `json:"checksum,omitzero"`
	Metadata
}

// MultipartChecksum is the checksum CreateMultipartUpload asks the object to carry.
type MultipartChecksum struct {
	Algorithm string `json:"algorithm,omitempty"`
	Type      string `json:"type,omitempty"` // COMPOSITE or FULL_OBJECT
}

// CompleteChecksums are the checksums a CompleteMultipartUpload request declares.
type CompleteChecksums struct {
	Object map[string]string         // algorithm -> value, from the request headers
	Parts  map[int]map[string]string // part number -> algorithm -> value, from the document
}

type partMeta struct {
	ETag      string            `json:"etag"`
	Size      int64             `json:"size"`
	ModTime   time.Time         `json:"mod_time"`
	Checksums map[string]string `json:"checksums,omitempty"`
}

// PartInfo describes an uploaded part.
type PartInfo struct {
	Number    int
	ETag      string
	Size      int64
	ModTime   time.Time
	Checksums map[string]string
}

// UploadInfo describes an in-progress multipart upload.
type UploadInfo struct {
	Key       string
	UploadID  string
	Initiated time.Time
}

// CompletePart names a part in a CompleteMultipartUpload request.
type CompletePart struct {
	Number int
	ETag   string
}

func partFile(n int) string { return fmt.Sprintf("part-%05d", n) }

// CreateUpload starts a multipart upload and returns its ID.
func (s *Store) CreateUpload(bucket, key string, md Metadata, ck MultipartChecksum) (string, error) {
	if err := s.requireBucket(bucket); err != nil {
		return "", err
	}
	if err := checkKey(key); err != nil {
		return "", err
	}
	if isDirKey(key) {
		return "", ErrMarkerNotEmpty
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	raw, err := json.Marshal(uploadMeta{Bucket: bucket, Key: key, Initiated: s.now().UTC(), Checksum: ck, Metadata: md})
	if err != nil {
		return "", err
	}
	if err := s.writeFileAtomic(filepath.Join(s.sys("uploads", id), uploadMetaFile), raw); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) loadUpload(bucket, key, id string) (uploadMeta, string, error) {
	var u uploadMeta
	if !uploadIDRe.MatchString(id) {
		return u, "", ErrNoSuchUpload
	}
	dir := s.sys("uploads", id)
	raw, err := os.ReadFile(filepath.Join(dir, uploadMetaFile))
	if err != nil || json.Unmarshal(raw, &u) != nil || u.Bucket != bucket || u.Key != key {
		return u, "", ErrNoSuchUpload
	}
	return u, dir, nil
}

// PutPart stores part n of an upload, replacing an earlier upload of the same part.
func (s *Store) PutPart(bucket, key, id string, n int, body io.Reader, size int64, verify Verifier) (PartInfo, error) {
	if n < 1 || n > maxPartNumber {
		return PartInfo{}, ErrInvalidPartNumber
	}
	if err := s.requireBucket(bucket); err != nil {
		return PartInfo{}, err
	}
	_, dir, err := s.loadUpload(bucket, key, id)
	if err != nil {
		return PartInfo{}, err
	}
	tmp, sum, checks, err := s.receive(body, size, verify)
	if err != nil {
		return PartInfo{}, err
	}
	return s.placePart(bucket, key, id, dir, n, tmp, sum, size, checks)
}

// RangeError rejects a copy range that does not fit the source object.
type RangeError struct{ Size int64 }

func (e *RangeError) Error() string {
	return fmt.Sprintf("range not valid for a source object of %d bytes", e.Size)
}

func (e *RangeError) Is(target error) bool { return target == ErrInvalidRange }

// ByteRange is an inclusive byte range, as in x-amz-copy-source-range.
type ByteRange struct{ Start, End int64 }

// PutPartCopy stores part n of an upload from rng of srcBucket/srcKey, or the whole
// object when rng is nil (UploadPartCopy). The copy stops when ctx is done.
func (s *Store) PutPartCopy(ctx context.Context, srcBucket, srcKey string, rng *ByteRange, bucket, key, id string, n int, o CopyOptions) (PartInfo, error) {
	if n < 1 || n > maxPartNumber {
		return PartInfo{}, ErrInvalidPartNumber
	}
	if err := s.requireBucket(bucket); err != nil {
		return PartInfo{}, err
	}
	_, dir, err := s.loadUpload(bucket, key, id)
	if err != nil {
		return PartInfo{}, err
	}
	f, src, err := s.GetObject(srcBucket, srcKey)
	if err != nil {
		return PartInfo{}, err
	}
	if f == nil {
		return PartInfo{}, ErrNoSuchKey // a directory marker has no bytes to copy
	}
	defer func() { _ = f.Close() }()
	if err := o.checkSource(src); err != nil {
		return PartInfo{}, err
	}
	start, length := int64(0), src.Size
	if rng != nil {
		if rng.Start > rng.End || rng.End >= src.Size {
			return PartInfo{}, &RangeError{Size: src.Size}
		}
		start, length = rng.Start, rng.End-rng.Start+1
	}
	tmp, sum, _, err := s.receive(ctxReader{ctx, io.NewSectionReader(f, start, length)}, length, nil)
	if err != nil {
		return PartInfo{}, err
	}
	return s.placePart(bucket, key, id, dir, n, tmp, sum, length, nil)
}

// placePart moves the received part file tmp into the upload directory.
func (s *Store) placePart(bucket, key, id, dir string, n int, tmp string, sum []byte, size int64, checks map[string]string) (PartInfo, error) {
	defer func() { _ = os.Remove(tmp) }()
	pm := partMeta{ETag: md5ETag(sum), Size: size, ModTime: s.now().UTC(), Checksums: checks}
	raw, err := json.Marshal(pm)
	if err != nil {
		return PartInfo{}, err
	}
	// The data and its metadata land under the upload lock, so a retried part racing the
	// original cannot leave one attempt's ETag describing the other's bytes.
	unlock := s.lockUpload(id)
	defer unlock()
	if _, _, err := s.loadUpload(bucket, key, id); err != nil {
		return PartInfo{}, err
	}
	if err := s.place(tmp, filepath.Join(dir, partFile(n)), sysDirPerm); err != nil {
		return PartInfo{}, err
	}
	if err := s.writeFileAtomic(filepath.Join(dir, partFile(n)+".json"), raw); err != nil {
		return PartInfo{}, err
	}
	return PartInfo{Number: n, ETag: pm.ETag, Size: size, ModTime: pm.ModTime, Checksums: checks}, nil
}

func readPart(dir string, n int) (partMeta, error) {
	var pm partMeta
	raw, err := os.ReadFile(filepath.Join(dir, partFile(n)+".json"))
	if err != nil || json.Unmarshal(raw, &pm) != nil {
		return pm, ErrInvalidPart
	}
	fi, err := os.Stat(filepath.Join(dir, partFile(n)))
	if err != nil || fi.Size() != pm.Size {
		return pm, ErrInvalidPart
	}
	return pm, nil
}

// CompleteUpload assembles the listed parts into the object. The ETag is the MD5 of the
// concatenated part MD5s followed by -<part count>, as S3 computes it. maxSize (0 = no
// limit) is checked before anything is written, so an oversized upload never replaces
// the existing object.
func (s *Store) CompleteUpload(ctx context.Context, bucket, key, id string, parts []CompletePart, cond Conditions, maxSize int64, ck CompleteChecksums) (ObjectInfo, error) {
	unlock := s.lockUpload(id)
	defer unlock()
	u, dir, err := s.loadUpload(bucket, key, id)
	if err != nil {
		return ObjectInfo{}, err
	}
	if len(parts) == 0 {
		return ObjectInfo{}, ErrInvalidPart
	}
	for i := 1; i < len(parts); i++ {
		if parts[i].Number <= parts[i-1].Number {
			return ObjectInfo{}, ErrInvalidPartOrder
		}
	}
	metas := make([]partMeta, len(parts))
	for i, p := range parts {
		pm, err := readPart(dir, p.Number)
		if err != nil || trimETag(p.ETag) != trimETag(pm.ETag) {
			return ObjectInfo{}, ErrInvalidPart
		}
		if i < len(parts)-1 && pm.Size < s.MinPartSize {
			return ObjectInfo{}, ErrEntityTooSmall
		}
		for alg, v := range ck.Parts[p.Number] {
			if got, ok := pm.Checksums[alg]; !ok || got != v {
				return ObjectInfo{}, ErrInvalidPart
			}
		}
		metas[i] = pm
	}
	sum, err := newObjectChecksum(u.Checksum, ck.Object, len(parts))
	if err != nil {
		return ObjectInfo{}, err
	}
	var total int64
	for _, pm := range metas {
		total += pm.Size
	}
	if maxSize > 0 && total > maxSize {
		return ObjectInfo{}, ErrEntityTooLarge
	}
	if err := s.fits(bucket, key, total); err != nil {
		return ObjectInfo{}, err
	}
	// The parts are copied into one file, so the object needs its size again.
	release, _, err := s.reserve(total)
	if err != nil {
		return ObjectInfo{}, err
	}
	defer release()

	tmp, err := os.CreateTemp(s.sys("tmp"), "complete-*")
	if err != nil {
		return ObjectInfo{}, err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	etagSum := md5.New() //nolint:gosec // S3 ETag format
	records := make([]PartRecord, len(parts))
	for i, p := range parts {
		if err = ctx.Err(); err != nil {
			break
		}
		raw, _ := hex.DecodeString(trimETag(metas[i].ETag))
		etagSum.Write(raw)
		// Only the object's algorithm: the sidecar is read on every GET and listing row.
		records[i] = PartRecord{Size: metas[i].Size}
		if v, ok := metas[i].Checksums[sum.algorithm()]; ok {
			records[i].Checksums = map[string]string{sum.alg: v}
		}
		if err = appendFile(sum.part(tmp), filepath.Join(dir, partFile(p.Number))); err != nil {
			break
		}
		sum.endPart()
	}
	if err == nil {
		err = tmp.Chmod(filePerm)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return ObjectInfo{}, err
	}

	etag := `"` + hex.EncodeToString(etagSum.Sum(nil)) + "-" + strconv.Itoa(len(parts)) + `"`
	m := sidecar{ETag: etag, Parts: records, Metadata: u.Metadata}
	if sum != nil {
		v := sum.value()
		if want, ok := ck.Object[sum.alg]; ok && want != v {
			return ObjectInfo{}, ErrBadDigest
		}
		m.Checksums, m.ChecksumType = map[string]string{sum.alg: v}, sum.typ
	}
	info, err := s.commit(bucket, key, name, m, cond)
	if err != nil {
		return ObjectInfo{}, err
	}
	_ = os.RemoveAll(dir)
	return info, nil
}

func appendFile(dst io.Writer, src string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(dst, f)
	return err
}

// AbortUpload discards an upload and its parts.
func (s *Store) AbortUpload(bucket, key, id string) error {
	unlock := s.lockUpload(id)
	defer unlock()
	_, dir, err := s.loadUpload(bucket, key, id)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// ListParts returns up to max parts numbered above marker, and whether more remain.
func (s *Store) ListParts(bucket, key, id string, marker, max int) ([]PartInfo, bool, error) {
	_, dir, err := s.loadUpload(bucket, key, id)
	if err != nil {
		return nil, false, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, false, err
	}
	var parts []PartInfo
	for _, e := range ents {
		numStr, ok := strings.CutPrefix(e.Name(), "part-")
		if !ok || strings.HasSuffix(numStr, ".json") {
			continue
		}
		n, err := strconv.Atoi(numStr)
		if err != nil || n <= marker {
			continue
		}
		pm, err := readPart(dir, n)
		if err != nil {
			continue
		}
		parts = append(parts, PartInfo{Number: n, ETag: pm.ETag, Size: pm.Size, ModTime: pm.ModTime, Checksums: pm.Checksums})
	}
	slices.SortFunc(parts, func(a, b PartInfo) int { return a.Number - b.Number })
	if len(parts) > max {
		return parts[:max], true, nil
	}
	return parts, false, nil
}

// ListUploads returns up to max uploads of bucket whose key starts with prefix, ordered
// by key then upload ID, after the (keyMarker, idMarker) position.
func (s *Store) ListUploads(bucket, prefix, keyMarker, idMarker string, max int) ([]UploadInfo, bool, error) {
	if err := s.requireBucket(bucket); err != nil {
		return nil, false, err
	}
	ents, err := os.ReadDir(s.sys("uploads"))
	if err != nil {
		return nil, false, err
	}
	var ups []UploadInfo
	for _, e := range ents {
		var u uploadMeta
		raw, err := os.ReadFile(filepath.Join(s.sys("uploads", e.Name()), uploadMetaFile))
		if err != nil || json.Unmarshal(raw, &u) != nil || u.Bucket != bucket || !strings.HasPrefix(u.Key, prefix) {
			continue
		}
		if u.Key < keyMarker || (u.Key == keyMarker && (idMarker == "" || e.Name() <= idMarker)) {
			continue
		}
		ups = append(ups, UploadInfo{Key: u.Key, UploadID: e.Name(), Initiated: u.Initiated})
	}
	slices.SortFunc(ups, func(a, b UploadInfo) int {
		if c := strings.Compare(a.Key, b.Key); c != 0 {
			return c
		}
		return strings.Compare(a.UploadID, b.UploadID)
	})
	if len(ups) > max {
		return ups[:max], true, nil
	}
	return ups, false, nil
}
