package s3api

import (
	"bytes"
	"crypto/md5" //nolint:gosec // Content-MD5 is MD5 by definition
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/somaz94/plainbucket/internal/checksum"
	"github.com/somaz94/plainbucket/internal/s3err"
	"github.com/somaz94/plainbucket/internal/sigv4"
)

// maxXMLBody bounds request documents such as DeleteObjects (1000 keys) and
// CompleteMultipartUpload (10000 parts).
const maxXMLBody = 2 << 20

// payload is a request body decoded from aws-chunked if needed, with every digest the
// client declared computed on the way through.
type payload struct {
	reader io.Reader
	size   int64

	chunked    *sigv4.ChunkedReader
	sha        hash.Hash
	wantSHA    string
	contentMD5 []byte
	hashes     map[string]hash.Hash // algorithm -> hash
	want       map[string]string    // algorithm -> value from a header
	trailing   map[string]bool      // algorithms whose value comes in a trailer
}

// bodyReader turns transport errors into IncompleteBody, so they are not mistaken for
// server-side write failures.
// It also enforces a minimum upload rate: the read deadline moves forward only once
// minProgress bytes have arrived, so a client trickling a byte now and then is cut off.
type bodyReader struct {
	r        io.Reader
	rc       *http.ResponseController
	timeout  time.Duration
	progress int
}

// minProgress is the data an upload must deliver within each BodyTimeout window.
const minProgress = 32 << 10

func newBodyReader(req *request, timeout time.Duration) *bodyReader {
	b := &bodyReader{r: req.r.Body, timeout: timeout}
	if timeout > 0 {
		b.rc = http.NewResponseController(req.w)
		if b.rc.SetReadDeadline(time.Now().Add(timeout)) != nil {
			b.rc = nil // not a server connection, as in tests with a recorder
		}
	}
	return b
}

func (b *bodyReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if b.rc != nil {
		if b.progress += n; b.progress >= minProgress {
			b.progress = 0
			_ = b.rc.SetReadDeadline(time.Now().Add(b.timeout))
		}
	}
	if err != nil && err != io.EOF {
		var e *s3err.Error
		if !errors.As(err, &e) {
			err = s3err.IncompleteBody()
		}
	}
	return n, err
}

// defaultChecksum is computed for an upload that declares no checksum, as S3 does.
const defaultChecksum = "CRC64NVME"

// newPayload prepares the request body. withDefault adds defaultChecksum when the client
// declared none, for bodies stored as whole objects.
func newPayload(req *request, timeout time.Duration, withDefault bool) (*payload, error) {
	r := req.r
	p := &payload{size: r.ContentLength, hashes: map[string]hash.Hash{}, want: map[string]string{}, trailing: map[string]bool{}}
	if req.auth.Streaming() {
		n, err := strconv.ParseInt(r.Header.Get("X-Amz-Decoded-Content-Length"), 10, 64)
		if err != nil || n < 0 {
			return nil, s3err.MissingContentLength()
		}
		p.size = n
		p.chunked = req.auth.NewChunkedReader(newBodyReader(req, timeout))
		p.reader = p.chunked
	} else {
		// A whole object may come as an HTTP chunked body of unknown length; parts and XML
		// documents must declare theirs.
		if p.size < 0 && (!withDefault || !slices.Contains(r.TransferEncoding, "chunked")) {
			return nil, s3err.MissingContentLength()
		}
		p.reader = newBodyReader(req, timeout)
		if ph := req.auth.PayloadHash; ph != sigv4.UnsignedPayload {
			p.sha, p.wantSHA = sha256.New(), ph
		}
	}

	if _, ok := r.Header["Content-Md5"]; ok {
		v := r.Header.Get("Content-MD5")
		sum, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(sum) != md5.Size {
			return nil, s3err.InvalidDigest()
		}
		p.contentMD5 = sum
	}
	for name := range r.Header {
		if alg, ok := checksum.FromHeader(name); ok {
			p.want[alg] = r.Header.Get(name)
		}
	}
	for t := range strings.SplitSeq(r.Header.Get("X-Amz-Trailer"), ",") {
		if alg, ok := checksum.FromHeader(strings.TrimSpace(t)); ok {
			p.trailing[alg] = true
		} else if strings.TrimSpace(t) != "" {
			return nil, s3err.InvalidRequest("Unsupported trailer " + t + ".")
		}
	}
	if len(p.trailing) > 0 && p.chunked == nil {
		return nil, s3err.InvalidRequest("x-amz-trailer requires an aws-chunked body.")
	}
	sdkAlg := r.Header.Get("X-Amz-Sdk-Checksum-Algorithm")
	for _, alg := range checksum.Algorithms {
		_, inHeader := p.want[alg]
		if inHeader || p.trailing[alg] || strings.EqualFold(sdkAlg, alg) {
			p.hashes[alg], _ = checksum.New(alg)
		}
	}
	if withDefault && len(p.hashes) == 0 {
		p.hashes[defaultChecksum], _ = checksum.New(defaultChecksum)
	}

	var sinks []io.Writer
	if p.sha != nil {
		sinks = append(sinks, p.sha)
	}
	for _, h := range p.hashes {
		sinks = append(sinks, h)
	}
	if len(sinks) > 0 {
		p.reader = io.TeeReader(p.reader, io.MultiWriter(sinks...))
	}
	return p, nil
}

// verify runs after exactly size bytes were read: it drains the rest of an aws-chunked
// body, then checks every declared digest and returns the computed checksums.
func (p *payload) verify(md5sum []byte) (map[string]string, error) {
	trailers := map[string]string{}
	if p.chunked != nil {
		if err := p.chunked.Finish(); err != nil {
			return nil, err
		}
		trailers = p.chunked.Trailers()
	}
	if p.sha != nil && hex.EncodeToString(p.sha.Sum(nil)) != p.wantSHA {
		return nil, s3err.ContentSHA256Mismatch()
	}
	if p.contentMD5 != nil && !bytes.Equal(p.contentMD5, md5sum) {
		return nil, s3err.BadDigest("The Content-MD5 you specified did not match what we received.")
	}
	out := make(map[string]string, len(p.hashes))
	for alg, h := range p.hashes {
		got := checksum.Encode(h.Sum(nil))
		want, ok := p.want[alg]
		if p.trailing[alg] {
			if want, ok = trailers[checksum.Header(alg)]; !ok {
				return nil, s3err.InvalidRequest("The trailer " + checksum.Header(alg) + " was declared but not sent.")
			}
		}
		if ok && want != got {
			return nil, s3err.BadDigest(fmt.Sprintf("The %s you specified did not match the calculated checksum.", alg))
		}
		out[alg] = got
	}
	return out, nil
}

// readXMLBody reads and verifies a small request document. checksums false leaves the
// x-amz-checksum-* headers unchecked, for requests where they describe something else.
func readXMLBody(req *request, timeout time.Duration, checksums bool) ([]byte, error) {
	p, err := newPayload(req, timeout, false)
	if err != nil {
		return nil, err
	}
	if !checksums {
		clear(p.want)
	}
	if p.size > maxXMLBody {
		return nil, s3err.InvalidRequest("The request document is too large.")
	}
	body := make([]byte, p.size)
	if _, err := io.ReadFull(p.reader, body); err != nil {
		return nil, s3err.IncompleteBody()
	}
	sum := md5.Sum(body) //nolint:gosec // Content-MD5
	if _, err := p.verify(sum[:]); err != nil {
		return nil, err
	}
	return body, nil
}

func setChecksumHeaders(h http.Header, sums map[string]string) {
	for alg, v := range sums {
		h.Set(checksum.Header(alg), v)
	}
}
