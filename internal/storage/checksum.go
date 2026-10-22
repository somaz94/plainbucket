package storage

import (
	"hash"
	"io"
	"strconv"
	"strings"

	"github.com/somaz94/plainbucket/internal/checksum"
)

// Checksum types of a multipart object, as S3 names them.
const (
	ChecksumComposite  = "COMPOSITE"
	ChecksumFullObject = "FULL_OBJECT"
)

// DefaultChecksumType is the type S3 gives alg when the upload names none: CRC64NVME
// exists only as a full-object checksum, the others default to composite.
func DefaultChecksumType(alg string) string {
	if strings.EqualFold(alg, "CRC64NVME") {
		return ChecksumFullObject
	}
	return ChecksumComposite
}

// ValidChecksumType reports whether S3 allows typ for alg: SHA digests cannot be
// combined across parts, and CRC64NVME is never composite.
func ValidChecksumType(alg, typ string) bool {
	switch strings.ToUpper(alg) {
	case "CRC64NVME":
		return typ == ChecksumFullObject
	case "SHA1", "SHA256":
		return typ == ChecksumComposite
	case "CRC32", "CRC32C":
		return typ == ChecksumComposite || typ == ChecksumFullObject
	}
	return false
}

// objectChecksum computes a multipart object's checksum while its parts are appended:
// a full-object checksum hashes the data, a composite one hashes the part digests.
type objectChecksum struct {
	alg, typ string
	parts    int
	whole    hash.Hash // FULL_OBJECT: the data; COMPOSITE: the concatenated part digests
	cur      hash.Hash // COMPOSITE: the current part
}

// newObjectChecksum picks the algorithm from the upload, or else from the single
// checksum the completion declares, whose "-<parts>" suffix marks it composite. It
// returns nil when there is none, and ErrInvalidChecksum when the completion declares
// one it could not verify.
func newObjectChecksum(up MultipartChecksum, declared map[string]string, parts int) (*objectChecksum, error) {
	alg, typ := up.Algorithm, up.Type
	if alg == "" {
		if len(declared) == 0 {
			return nil, nil
		}
		if len(declared) > 1 {
			return nil, ErrInvalidChecksum
		}
		for a, v := range declared {
			alg, typ = a, ChecksumFullObject
			if strings.Contains(v, "-") {
				typ = ChecksumComposite
			}
		}
	}
	alg = strings.ToUpper(alg)
	if typ == "" {
		typ = DefaultChecksumType(alg)
	}
	if !ValidChecksumType(alg, typ) {
		return nil, ErrInvalidChecksum
	}
	for a := range declared {
		if a != alg {
			return nil, ErrInvalidChecksum
		}
	}
	whole, ok := checksum.New(alg)
	if !ok {
		return nil, ErrInvalidChecksum
	}
	c := &objectChecksum{alg: alg, typ: typ, parts: parts, whole: whole}
	if typ == ChecksumComposite {
		c.cur, _ = checksum.New(alg)
	}
	return c, nil
}

func (c *objectChecksum) algorithm() string {
	if c == nil {
		return ""
	}
	return c.alg
}

// part returns the writer a part's bytes go to besides dst.
func (c *objectChecksum) part(dst io.Writer) io.Writer {
	switch {
	case c == nil:
		return dst
	case c.cur != nil:
		return io.MultiWriter(dst, c.cur)
	default:
		return io.MultiWriter(dst, c.whole)
	}
}

func (c *objectChecksum) endPart() {
	if c != nil && c.cur != nil {
		c.whole.Write(c.cur.Sum(nil))
		c.cur.Reset()
	}
}

func (c *objectChecksum) value() string {
	v := checksum.Encode(c.whole.Sum(nil))
	if c.typ == ChecksumComposite {
		v += "-" + strconv.Itoa(c.parts)
	}
	return v
}
