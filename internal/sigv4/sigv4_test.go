package sigv4

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/somaz94/plainbucket/internal/s3err"
	"github.com/somaz94/plainbucket/internal/sigv4/sigv4test"
)

const (
	docAK = "AKIAIOSFODNN7EXAMPLE"
	docSK = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

var docNow = time.Date(2013, 5, 24, 0, 5, 0, 0, time.UTC)

func docCreds(ak string) (string, bool) {
	if ak == docAK {
		return docSK, true
	}
	return "", false
}

func code(e *s3err.Error) string {
	if e == nil {
		return ""
	}
	return e.Code
}

// TestVerifyHeader_AWSDocVector is the GET Object example from "Signature Calculations
// for the Authorization Header: Transferring Payload in a Single Chunk".
func TestVerifyHeader_AWSDocVector(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://examplebucket.s3.amazonaws.com/test.txt", nil)
	r.Header.Set("Range", "bytes=0-9")
	r.Header.Set("X-Amz-Content-Sha256", EmptySHA256)
	r.Header.Set("X-Amz-Date", "20130524T000000Z")
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+docAK+"/20130524/us-east-1/s3/aws4_request,"+
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date,"+
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41")
	a, e := Verify(r, docCreds, docNow)
	if e != nil {
		t.Fatalf("Verify: %v", e)
	}
	if a.AccessKey != docAK || a.PayloadHash != EmptySHA256 || a.Streaming() {
		t.Errorf("auth = %+v", a)
	}

	r.Header.Set("Range", "bytes=0-10")
	if _, e := Verify(r, docCreds, docNow); code(e) != "SignatureDoesNotMatch" {
		t.Errorf("tampered header: %v", e)
	}
}

// TestVerifyQuery_AWSDocVector is the GET example from "Authenticating Requests: Using Query Parameters".
func TestVerifyQuery_AWSDocVector(t *testing.T) {
	target := "https://examplebucket.s3.amazonaws.com/test.txt" +
		"?X-Amz-Algorithm=AWS4-HMAC-SHA256" +
		"&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request" +
		"&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-SignedHeaders=host" +
		"&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	r := httptest.NewRequest(http.MethodGet, target, nil)
	a, e := Verify(r, docCreds, docNow)
	if e != nil {
		t.Fatalf("Verify: %v", e)
	}
	if a.PayloadHash != UnsignedPayload {
		t.Errorf("payload = %q", a.PayloadHash)
	}

	if _, e := Verify(r, docCreds, docNow.Add(25*time.Hour)); code(e) != "AccessDenied" {
		t.Errorf("expired: %v", e)
	}
	if _, e := Verify(r, docCreds, docNow.Add(-time.Hour)); code(e) != "AccessDenied" {
		t.Errorf("not yet valid: %v", e)
	}
}

func TestVerifyQueryMalformed(t *testing.T) {
	base := url.Values{
		"X-Amz-Algorithm":     {Algorithm},
		"X-Amz-Credential":    {docAK + "/20130524/us-east-1/s3/aws4_request"},
		"X-Amz-Date":          {"20130524T000000Z"},
		"X-Amz-Expires":       {"3600"},
		"X-Amz-SignedHeaders": {"host"},
		"X-Amz-Signature":     {"00"},
	}
	cases := []struct {
		name, key, value, want string
	}{
		{"algorithm", "X-Amz-Algorithm", "AWS4-HMAC-SHA1", "AuthorizationQueryParametersError"},
		{"credential", "X-Amz-Credential", "nope", "AuthorizationQueryParametersError"},
		{"unknown key", "X-Amz-Credential", "OTHER/20130524/us-east-1/s3/aws4_request", "InvalidAccessKeyId"},
		{"date", "X-Amz-Date", "yesterday", "AuthorizationQueryParametersError"},
		{"date mismatch", "X-Amz-Date", "20130525T000000Z", "AuthorizationQueryParametersError"},
		{"expires", "X-Amz-Expires", "604801", "AuthorizationQueryParametersError"},
		{"signed headers", "X-Amz-SignedHeaders", "range", "AuthorizationQueryParametersError"},
		{"signature", "", "", "SignatureDoesNotMatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := url.Values{}
			for k, v := range base {
				q[k] = v
			}
			if tc.key != "" {
				q.Set(tc.key, tc.value)
			}
			r := httptest.NewRequest(http.MethodGet, "http://h/b/k?"+q.Encode(), nil)
			if _, e := Verify(r, docCreds, docNow); code(e) != tc.want {
				t.Errorf("got %v, want %s", e, tc.want)
			}
		})
	}
}

func TestVerifyNoAuth(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://h/b/k", nil)
	if _, e := Verify(r, docCreds, docNow); code(e) != "AccessDenied" {
		t.Errorf("anonymous: %v", e)
	}
	r = httptest.NewRequest(http.MethodGet, "http://h/b/k?AWSAccessKeyId=x&Signature=y", nil)
	if _, e := Verify(r, docCreds, docNow); code(e) != "InvalidRequest" {
		t.Errorf("sigv2 query: %v", e)
	}
	r = httptest.NewRequest(http.MethodGet, "http://h/b/k", nil)
	r.Header.Set("Authorization", "AWS AK:sig")
	if _, e := Verify(r, docCreds, docNow); code(e) != "InvalidRequest" {
		t.Errorf("sigv2 header: %v", e)
	}
}

// signer builds Authorization headers for tests, written independently of Verify.
type signer struct {
	ak, sk, region string
	at             time.Time
	useDate        bool // send Date instead of X-Amz-Date
}

func (s signer) date() string  { return s.at.UTC().Format(amzDateLayout) }
func (s signer) scope() string { return s.date()[:8] + "/" + s.region + "/s3/aws4_request" }
func (s signer) key() []byte   { return signingKey(s.sk, s.date()[:8], s.region, "s3") }
func hexHMAC(k []byte, d string) string {
	m := hmac.New(sha256.New, k)
	m.Write([]byte(d))
	return hex.EncodeToString(m.Sum(nil))
}

// sign sets X-Amz-Date, X-Amz-Content-Sha256 and Authorization, signing host plus every
// x-amz-* header and the extra names given. It returns the seed signature.
func (s signer) sign(r *http.Request, payload string, extra ...string) string {
	if s.useDate {
		r.Header.Set("Date", s.at.UTC().Format(http.TimeFormat))
		extra = append(extra, "date")
	} else {
		r.Header.Set("X-Amz-Date", s.date())
	}
	r.Header.Set("X-Amz-Content-Sha256", payload)
	names := append([]string{"host"}, extra...)
	for k := range r.Header {
		if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-amz-") {
			names = append(names, lk)
		}
	}
	sort.Strings(names)

	var pairs []string
	for k, vs := range r.URL.Query() {
		for _, v := range vs {
			pairs = append(pairs, url.QueryEscape(k)+"="+strings.ReplaceAll(url.QueryEscape(v), "+", "%20"))
		}
	}
	sort.Strings(pairs)

	var canon strings.Builder
	canon.WriteString(r.Method + "\n" + r.URL.EscapedPath() + "\n" + strings.Join(pairs, "&") + "\n")
	for _, n := range names {
		v := r.Host
		if n != "host" {
			v = r.Header.Get(n)
		}
		canon.WriteString(n + ":" + v + "\n")
	}
	canon.WriteString("\n" + strings.Join(names, ";") + "\n" + payload)
	sum := sha256.Sum256([]byte(canon.String()))
	sig := hexHMAC(s.key(), Algorithm+"\n"+s.date()+"\n"+s.scope()+"\n"+hex.EncodeToString(sum[:]))
	r.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		Algorithm, s.ak, s.scope(), strings.Join(names, ";"), sig))
	return sig
}

var testSigner = signer{ak: docAK, sk: docSK, region: "eu-west-1", at: docNow}

func TestVerifyHeaderQueryAndPayloadModes(t *testing.T) {
	for _, payload := range []string{UnsignedPayload, StreamingSigned, StreamingSignedTrailer, StreamingUnsignedTrailer, EmptySHA256} {
		r := httptest.NewRequest(http.MethodGet, "http://h:9000/b/a%20b+c~?list-type=2&prefix=x%2Fy&a=2&a=1&empty=", nil)
		testSigner.sign(r, payload)
		a, e := Verify(r, docCreds, docNow)
		if e != nil {
			t.Fatalf("%s: %v", payload, e)
		}
		if a.Streaming() != strings.HasPrefix(payload, "STREAMING-") {
			t.Errorf("%s: Streaming() = %v", payload, a.Streaming())
		}
	}
}

func TestVerifyHeaderFailures(t *testing.T) {
	fresh := func() *http.Request {
		r := httptest.NewRequest(http.MethodPut, "http://h/b/k", nil)
		testSigner.sign(r, UnsignedPayload)
		return r
	}
	cases := []struct {
		name   string
		mutate func(*http.Request)
		now    time.Time
		want   string
	}{
		{"malformed", func(r *http.Request) { r.Header.Set("Authorization", Algorithm+" Credential=x") }, docNow, "AuthorizationHeaderMalformed"},
		{"unknown key", func(r *http.Request) {
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), docAK, "OTHER", 1))
		}, docNow, "InvalidAccessKeyId"},
		{"no date", func(r *http.Request) { r.Header.Del("X-Amz-Date") }, docNow, "AccessDenied"},
		{"date mismatch", func(r *http.Request) { r.Header.Set("X-Amz-Date", "20130525T000000Z") }, docNow, "AuthorizationHeaderMalformed"},
		{"skew", func(*http.Request) {}, docNow.Add(time.Hour), "RequestTimeTooSkewed"},
		{"no payload", func(r *http.Request) { r.Header.Del("X-Amz-Content-Sha256") }, docNow, "InvalidRequest"},
		{"bad payload", func(r *http.Request) { r.Header.Set("X-Amz-Content-Sha256", "abc") }, docNow, "InvalidArgument"},
		{"host unsigned", func(r *http.Request) {
			r.Header.Set("Authorization", strings.Replace(r.Header.Get("Authorization"), "SignedHeaders=host;", "SignedHeaders=", 1))
		}, docNow, "AuthorizationHeaderMalformed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := fresh()
			tc.mutate(r)
			if _, e := Verify(r, docCreds, tc.now); code(e) != tc.want {
				t.Errorf("got %v, want %s", e, tc.want)
			}
		})
	}
}

func TestVerifyHeaderDateFallbackAndContentLength(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "http://h/b/k", strings.NewReader("hello"))
	r.Header.Set("Content-Length", "5")
	dateSigner := testSigner
	dateSigner.useDate = true
	dateSigner.sign(r, UnsignedPayload, "content-length")
	r.Header.Del("Content-Length") // net/http keeps it only in r.ContentLength
	if _, e := Verify(r, docCreds, docNow); e != nil {
		t.Fatalf("Date header and r.ContentLength: %v", e)
	}
}

func TestURIEncode(t *testing.T) {
	if got := URIEncodePath("a b/ü+~"); got != "a%20b/%C3%BC%2B~" {
		t.Errorf("URIEncodePath = %q", got)
	}
	if URIEncodePath("") != "" || uriEncode("", false) != "/" {
		t.Error("empty encoding")
	}
	if got := uriEncode("a/b", true); got != "a%2Fb" {
		t.Errorf("slash = %q", got)
	}
}

// chunkedBody encodes data as aws-chunked with the given chunk size, signing each chunk
// with the seed signature chain when signed, and appending trailers when non-nil.
func chunkedBody(s signer, seed string, data []byte, size int, signed bool, trailers [][2]string) string {
	var b strings.Builder
	prev := seed
	emit := func(chunk []byte) {
		if signed {
			h := sha256.Sum256(chunk)
			sts := "AWS4-HMAC-SHA256-PAYLOAD\n" + s.date() + "\n" + s.scope() + "\n" + prev + "\n" + EmptySHA256 + "\n" + hex.EncodeToString(h[:])
			prev = hexHMAC(s.key(), sts)
			fmt.Fprintf(&b, "%x;chunk-signature=%s\r\n", len(chunk), prev)
		} else {
			fmt.Fprintf(&b, "%x\r\n", len(chunk))
		}
		if len(chunk) > 0 {
			b.Write(chunk)
			b.WriteString("\r\n")
		}
	}
	for len(data) > 0 {
		n := min(size, len(data))
		emit(data[:n])
		data = data[n:]
	}
	emit(nil)
	if trailers == nil {
		b.WriteString("\r\n")
		return b.String()
	}
	var canon strings.Builder
	for _, tr := range trailers {
		fmt.Fprintf(&b, "%s:%s\r\n", tr[0], tr[1])
		canon.WriteString(tr[0] + ":" + tr[1] + "\n")
	}
	if signed {
		h := sha256.Sum256([]byte(canon.String()))
		sts := "AWS4-HMAC-SHA256-TRAILER\n" + s.date() + "\n" + s.scope() + "\n" + prev + "\n" + hex.EncodeToString(h[:])
		fmt.Fprintf(&b, "x-amz-trailer-signature:%s\r\n", hexHMAC(s.key(), sts))
	}
	b.WriteString("\r\n")
	return b.String()
}

// TestChunked_AWSDocVector checks the seed and chunk signatures from "Signature
// Calculations for the Authorization Header: Transferring Payload in Multiple Chunks".
func TestChunked_AWSDocVector(t *testing.T) {
	doc := signer{ak: docAK, sk: docSK, region: "us-east-1", at: time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)}
	data := []byte(strings.Repeat("a", 66560))
	body := chunkedBody(doc, "4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9", data, 65536, true, nil)
	for _, sig := range []string{
		"ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648",
		"0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497",
		"b6c6ea8a5354eaf15b3cb7646744f4275b71ea724fed81ceb9323e279d449df9",
	} {
		if !strings.Contains(body, sig) {
			t.Fatalf("chunk signature %s missing: test signer is wrong", sig)
		}
	}

	r := httptest.NewRequest(http.MethodPut, "http://s3.amazonaws.com/examplebucket/chunkObject.txt", strings.NewReader(body))
	r.Header.Set("X-Amz-Date", "20130524T000000Z")
	r.Header.Set("X-Amz-Storage-Class", "REDUCED_REDUNDANCY")
	r.Header.Set("Content-Encoding", "aws-chunked")
	r.Header.Set("X-Amz-Decoded-Content-Length", "66560")
	r.Header.Set("Content-Length", "66824")
	r.Header.Set("X-Amz-Content-Sha256", StreamingSigned)
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+docAK+"/20130524/us-east-1/s3/aws4_request,"+
		"SignedHeaders=content-encoding;content-length;host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-storage-class,"+
		"Signature=4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9")
	a, e := Verify(r, docCreds, doc.at)
	if e != nil {
		t.Fatalf("Verify: %v", e)
	}
	cr := a.NewChunkedReader(r.Body)
	got, err := io.ReadAll(cr)
	if err != nil || string(got) != string(data) {
		t.Fatalf("decoded %d bytes, err %v", len(got), err)
	}
	if err := cr.Finish(); err != nil {
		t.Errorf("Finish: %v", err)
	}
}

func streamingRequest(t *testing.T, payload, body string) *ChunkedReader {
	t.Helper()
	r := httptest.NewRequest(http.MethodPut, "http://h/b/k", strings.NewReader(body))
	r.Header.Set("X-Amz-Decoded-Content-Length", "0")
	seed := testSigner.sign(r, payload)
	a, e := Verify(r, docCreds, docNow)
	if e != nil {
		t.Fatalf("Verify: %v", e)
	}
	if a.signature != seed {
		t.Fatalf("seed mismatch")
	}
	return a.NewChunkedReader(r.Body)
}

func seedFor(payload string) string {
	r := httptest.NewRequest(http.MethodPut, "http://h/b/k", nil)
	r.Header.Set("X-Amz-Decoded-Content-Length", "0")
	return testSigner.sign(r, payload)
}

func TestChunkedModes(t *testing.T) {
	data := []byte(strings.Repeat("0123456789", 2000))
	trailers := [][2]string{{"x-amz-checksum-crc32", "AAAAAA=="}}
	cases := []struct {
		payload  string
		signed   bool
		trailers [][2]string
	}{
		{StreamingSigned, true, nil},
		{StreamingSignedTrailer, true, trailers},
		{StreamingUnsignedTrailer, false, trailers},
	}
	for _, tc := range cases {
		t.Run(tc.payload, func(t *testing.T) {
			body := chunkedBody(testSigner, seedFor(tc.payload), data, 8192, tc.signed, tc.trailers)
			cr := streamingRequest(t, tc.payload, body)
			got, err := io.ReadAll(io.LimitReader(cr, int64(len(data))))
			if err != nil || string(got) != string(data) {
				t.Fatalf("decoded %d bytes, err %v", len(got), err)
			}
			if err := cr.Finish(); err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if tc.trailers != nil && cr.Trailers()["x-amz-checksum-crc32"] != "AAAAAA==" {
				t.Errorf("trailers = %v", cr.Trailers())
			}
		})
	}
}

func TestChunkedTampering(t *testing.T) {
	data := []byte(strings.Repeat("x", 100))
	good := chunkedBody(testSigner, seedFor(StreamingSignedTrailer), data, 64, true, [][2]string{{"x-amz-checksum-crc32", "AAAAAA=="}})

	cases := map[string]string{
		"chunk data":        strings.Replace(good, "xxxx", "xxxy", 1),
		"trailer value":     strings.Replace(good, "AAAAAA==", "AAAAAB==", 1),
		"truncated":         good[:80],
		"bad size":          "zz;chunk-signature=00\r\n",
		"missing signature": "10\r\n",
		"long chunk":        strings.Replace(good, "40;", "3f;", 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			cr := streamingRequest(t, StreamingSignedTrailer, body)
			_, err := io.ReadAll(cr)
			var e *s3err.Error
			if !errors.As(err, &e) {
				t.Fatalf("err = %v, want an S3 error", err)
			}
		})
	}
}

func TestChunkedFinishExtraData(t *testing.T) {
	data := []byte("abcdef")
	body := chunkedBody(testSigner, seedFor(StreamingSigned), data, 3, true, nil)
	cr := streamingRequest(t, StreamingSigned, body)
	if _, err := io.ReadAll(io.LimitReader(cr, 3)); err != nil {
		t.Fatal(err)
	}
	if err := cr.Finish(); err == nil {
		t.Error("Finish accepted data beyond the declared length")
	}
}

func TestChunkedUnexpectedTrailer(t *testing.T) {
	body := chunkedBody(testSigner, seedFor(StreamingSigned), []byte("ab"), 2, true, nil)
	body = strings.TrimSuffix(body, "\r\n") + "x-amz-checksum-crc32:AAAAAA==\r\n\r\n"
	cr := streamingRequest(t, StreamingSigned, body)
	if _, err := io.ReadAll(cr); err == nil {
		t.Error("trailer accepted for a non-trailer payload")
	}
}

func TestReadLineLimits(t *testing.T) {
	cr := &ChunkedReader{}
	long := strings.Repeat("a", maxChunkHeader+10) + "\r\n"
	cr.br = newBufReader(long)
	if _, err := cr.readLine(); err == nil {
		t.Error("over-long line accepted")
	}
	cr.br = newBufReader("no-crlf\n")
	if _, err := cr.readLine(); err == nil {
		t.Error("bare LF accepted")
	}
	cr.br = newBufReader("partial")
	if _, err := cr.readLine(); err == nil {
		t.Error("unterminated line accepted")
	}
}

// newBufReader uses the smallest buffer so long lines take the ErrBufferFull path.
func newBufReader(s string) *bufio.Reader {
	return bufio.NewReaderSize(strings.NewReader(s), 16)
}

func TestUnsignedAmzHeaderRejected(t *testing.T) {
	r := httptest.NewRequest(http.MethodPut, "http://h/b/k", nil)
	testSigner.sign(r, UnsignedPayload)
	r.Header.Set("X-Amz-Copy-Source", "other/secret")
	if _, e := Verify(r, docCreds, docNow); code(e) != "AccessDenied" {
		t.Errorf("header auth: %v", e)
	}

	target := "https://examplebucket.s3.amazonaws.com/test.txt" +
		"?X-Amz-Algorithm=AWS4-HMAC-SHA256" +
		"&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request" +
		"&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-SignedHeaders=host" +
		"&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	r = httptest.NewRequest(http.MethodGet, target, nil)
	r.Header.Set("X-Amz-Copy-Source", "other/secret")
	if _, e := Verify(r, docCreds, docNow); code(e) != "AccessDenied" {
		t.Errorf("presigned: %v", e)
	}
}

func TestChunkedTruncatedEnd(t *testing.T) {
	data := []byte("abcdef")
	signed := chunkedBody(testSigner, seedFor(StreamingSigned), data, 3, true, nil)
	noFinal := signed[:strings.LastIndex(signed, "0;chunk-signature=")]
	trailed := chunkedBody(testSigner, seedFor(StreamingSignedTrailer), data, 3, true, [][2]string{{"x-amz-checksum-crc32", "AAAAAA=="}})
	noTrailerSig := trailed[:strings.Index(trailed, "x-amz-trailer-signature")]
	for name, c := range map[string]struct{ payload, body string }{
		"no final chunk":          {StreamingSigned, noFinal},
		"no trailer signature":    {StreamingSignedTrailer, noTrailerSig},
		"data chunk without CRLF": {StreamingSigned, signed[:strings.Index(signed, "abc")+3]},
	} {
		t.Run(name, func(t *testing.T) {
			cr := streamingRequest(t, c.payload, c.body)
			_, _ = io.ReadAll(io.LimitReader(cr, int64(len(data))))
			if err := cr.Finish(); err == nil {
				t.Error("truncated body accepted")
			}
		})
	}
}

// TestVerifyQuery_HeadOnGetURL checks the GitLab Runner pattern: HEAD on a URL presigned for GET.
func TestVerifyQuery_HeadOnGetURL(t *testing.T) {
	target := "https://examplebucket.s3.amazonaws.com/test.txt" +
		"?X-Amz-Algorithm=AWS4-HMAC-SHA256" +
		"&X-Amz-Credential=" + docAK + "%2F20130524%2Fus-east-1%2Fs3%2Faws4_request" +
		"&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-SignedHeaders=host" +
		"&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if _, e := Verify(httptest.NewRequest(http.MethodHead, target, nil), docCreds, docNow); e != nil {
		t.Errorf("HEAD on a GET URL: %v", e)
	}
	if _, e := Verify(httptest.NewRequest(http.MethodPut, target, nil), docCreds, docNow); code(e) != "SignatureDoesNotMatch" {
		t.Errorf("PUT on a GET URL: %v", e)
	}
	if _, e := Verify(httptest.NewRequest(http.MethodDelete, target, nil), docCreds, docNow); code(e) != "SignatureDoesNotMatch" {
		t.Errorf("DELETE on a GET URL: %v", e)
	}
}

func TestVerifySTS(t *testing.T) {
	body := []byte("Action=AssumeRole&Version=2011-06-15&RoleArn=arn%3Aaws%3Aiam%3A%3A0%3Arole%2Fx&RoleSessionName=s")
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	signed := func(service string, b []byte) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "http://pb.test/", strings.NewReader(string(b)))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		sigv4test.Signer{AccessKey: docAK, SecretKey: docSK, Region: "us-east-1", Service: service}.Sign(r, hash, docNow)
		return r
	}
	if a, e := VerifySTS(signed("sts", body), body, docCreds, docNow); e != nil || a.AccessKey != docAK {
		t.Fatalf("sts: %v", e)
	}
	// Signed for S3, it is not an STS request; and an S3 verifier refuses an STS signature.
	if _, e := VerifySTS(signed("s3", body), body, docCreds, docNow); code(e) != "AuthorizationHeaderMalformed" {
		t.Errorf("s3-signed: %s", code(e))
	}
	if _, e := Verify(signed("sts", body), docCreds, docNow); code(e) != "AuthorizationHeaderMalformed" {
		t.Errorf("sts signature on S3: %s", code(e))
	}
	// The body the signature covers is the body read.
	if _, e := VerifySTS(signed("sts", body), []byte("Action=AssumeRole&DurationSeconds=43200"), docCreds, docNow); code(e) != "XAmzContentSHA256Mismatch" {
		t.Errorf("other body: %s", code(e))
	}
	if _, e := VerifySTS(httptest.NewRequest(http.MethodPost, "/", nil), nil, docCreds, docNow); code(e) != "AccessDenied" {
		t.Errorf("unsigned: %s", code(e))
	}
}
