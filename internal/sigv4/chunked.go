package sigv4

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"strconv"
	"strings"

	"github.com/somaz94/plainbucket/internal/s3err"
)

const (
	maxChunkHeader = 4096
	maxChunkSize   = 1 << 30
	maxTrailers    = 16
)

var errChunkFormat = errors.New("malformed aws-chunked body")

// ChunkedReader decodes an aws-chunked body, verifying chunk and trailer signatures
// when the payload mode is signed. Read returns io.EOF only after the final chunk and
// trailers have been read and verified.
type ChunkedReader struct {
	br      *bufio.Reader
	auth    *Auth
	signed  bool
	trailer bool

	prevSig   string
	chunkSig  string
	chunkHash hash.Hash
	remaining int64
	done      bool
	err       error
	trailers  map[string]string
}

// NewChunkedReader decodes body according to a.PayloadHash, which must be a STREAMING-* mode.
func (a *Auth) NewChunkedReader(body io.Reader) *ChunkedReader {
	return &ChunkedReader{
		br:        bufio.NewReaderSize(body, 64<<10),
		auth:      a,
		signed:    a.PayloadHash == StreamingSigned || a.PayloadHash == StreamingSignedTrailer,
		trailer:   a.PayloadHash == StreamingSignedTrailer || a.PayloadHash == StreamingUnsignedTrailer,
		prevSig:   a.signature,
		chunkHash: sha256.New(),
		trailers:  map[string]string{},
	}
}

// Trailers returns the trailing headers, keyed by lower-case name, once Read hit io.EOF.
func (c *ChunkedReader) Trailers() map[string]string { return c.trailers }

// Finish reads the rest of the body, which must hold no further data, so the final
// chunk signature and the trailers get verified.
func (c *ChunkedReader) Finish() error {
	var one [1]byte
	n, err := c.Read(one[:])
	if n > 0 {
		return s3err.InvalidArgument("The decoded body is longer than x-amz-decoded-content-length.")
	}
	if err == io.EOF {
		return nil
	}
	return err
}

func (c *ChunkedReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	for c.remaining == 0 {
		if c.done {
			return 0, io.EOF
		}
		if err := c.nextChunk(); err != nil {
			c.err = err
			return 0, err
		}
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.br.Read(p)
	c.remaining -= int64(n)
	if c.signed {
		c.chunkHash.Write(p[:n])
	}
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		c.err = s3err.IncompleteBody()
		return n, c.err
	}
	if c.remaining == 0 {
		if err := c.endChunk(); err != nil {
			c.err = err
		}
	}
	return n, nil
}

// nextChunk reads a chunk header; a zero-size chunk also consumes the trailer section.
func (c *ChunkedReader) nextChunk() error {
	line, err := c.readLine()
	if err == io.EOF {
		return s3err.IncompleteBody() // the final zero-size chunk is mandatory
	}
	if err != nil {
		return err
	}
	sizeHex, ext, _ := strings.Cut(line, ";")
	size, err := strconv.ParseInt(strings.TrimSpace(sizeHex), 16, 64)
	if err != nil || size < 0 || size > maxChunkSize {
		return s3err.InvalidArgument("Invalid chunk size.")
	}
	if c.signed {
		sig, ok := strings.CutPrefix(ext, "chunk-signature=")
		if !ok {
			return s3err.InvalidArgument("Missing chunk signature.")
		}
		c.chunkSig = sig
	}
	c.remaining = size
	if size > 0 {
		return nil
	}

	if err := c.verifyChunk(); err != nil {
		return err
	}
	c.done = true
	return c.readTrailers()
}

// endChunk consumes the CRLF after chunk data and verifies the chunk signature.
func (c *ChunkedReader) endChunk() error {
	line, err := c.readLine()
	if err == io.EOF {
		return s3err.IncompleteBody()
	}
	if err != nil {
		return err
	}
	if line != "" {
		return s3err.InvalidArgument("Chunk data is longer than its declared size.")
	}
	return c.verifyChunk()
}

func (c *ChunkedReader) verifyChunk() error {
	if !c.signed {
		return nil
	}
	sts := "AWS4-HMAC-SHA256-PAYLOAD\n" + c.auth.amzDate + "\n" + c.auth.scope + "\n" + c.prevSig + "\n" +
		EmptySHA256 + "\n" + hex.EncodeToString(c.chunkHash.Sum(nil))
	expected := hex.EncodeToString(hmacSHA256(c.auth.key, sts))
	if !constantTimeEqual(expected, c.chunkSig) {
		return s3err.SignatureDoesNotMatch()
	}
	c.prevSig = c.chunkSig
	c.chunkHash.Reset()
	return nil
}

func (c *ChunkedReader) readTrailers() error {
	var canonical strings.Builder
	trailerSig := ""
	for range maxTrailers + 2 {
		line, err := c.readLine()
		if err == io.EOF && !c.trailer {
			return nil
		}
		if err == io.EOF {
			return s3err.IncompleteBody() // trailers end with an empty line
		}
		if err != nil {
			return err
		}
		if line == "" {
			if c.signed && c.trailer {
				return c.verifyTrailer(canonical.String(), trailerSig)
			}
			return nil
		}
		if !c.trailer {
			return s3err.InvalidArgument("Unexpected data after the final chunk.")
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return s3err.InvalidArgument("Malformed trailer.")
		}
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		if name == "x-amz-trailer-signature" {
			trailerSig = value
			continue
		}
		c.trailers[name] = value
		canonical.WriteString(name + ":" + value + "\n")
	}
	return s3err.InvalidArgument("Too many trailers.")
}

func (c *ChunkedReader) verifyTrailer(canonical, sig string) error {
	sum := sha256.Sum256([]byte(canonical))
	sts := "AWS4-HMAC-SHA256-TRAILER\n" + c.auth.amzDate + "\n" + c.auth.scope + "\n" + c.prevSig + "\n" +
		hex.EncodeToString(sum[:])
	expected := hex.EncodeToString(hmacSHA256(c.auth.key, sts))
	if !constantTimeEqual(expected, sig) {
		return s3err.SignatureDoesNotMatch()
	}
	return nil
}

// readLine reads one CRLF-terminated line without the terminator.
func (c *ChunkedReader) readLine() (string, error) {
	var b []byte
	for {
		frag, err := c.br.ReadSlice('\n')
		b = append(b, frag...)
		if len(b) > maxChunkHeader {
			return "", s3err.InvalidArgument("Chunk header is too long.")
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF && len(b) == 0 {
			return "", io.EOF
		}
		if err != nil {
			return "", s3err.IncompleteBody()
		}
		break
	}
	line := strings.TrimSuffix(string(b), "\n")
	line, ok := strings.CutSuffix(line, "\r")
	if !ok {
		return "", s3err.InvalidArgument(errChunkFormat.Error())
	}
	return line, nil
}
