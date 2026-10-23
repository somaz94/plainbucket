package s3api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/somaz94/plainbucket/internal/s3err"
	"github.com/somaz94/plainbucket/internal/sigv4/sigv4test"
)

// postForm is a form in send order: fields, the file part unless noFile, then after.
type postForm struct {
	fields   [][2]string
	filename string
	file     string
	noFile   bool
	after    [][2]string
}

func encodePolicy(exp time.Time, conds ...any) string {
	raw, _ := json.Marshal(map[string]any{"expiration": exp.UTC().Format(time.RFC3339), "conditions": conds})
	return base64.StdEncoding.EncodeToString(raw)
}

// signedForm signs a policy whose conditions cover the signature fields as well as conds,
// and returns the form fields in a deterministic order, extra fields after them.
func signedForm(s sigv4test.Signer, exp time.Time, conds []any, extra ...[2]string) [][2]string {
	amzDate := testNow.UTC().Format("20060102T150405Z")
	cred := s.AccessKey + "/" + amzDate[:8] + "/" + s.Region + "/s3/aws4_request"
	conds = append(conds, map[string]string{"x-amz-algorithm": "AWS4-HMAC-SHA256"},
		map[string]string{"x-amz-credential": cred}, map[string]string{"x-amz-date": amzDate})
	f := s.PostFields(encodePolicy(exp, conds...), testNow)
	out := [][2]string{}
	for _, name := range []string{"x-amz-algorithm", "x-amz-credential", "x-amz-date", "policy", "x-amz-signature"} {
		out = append(out, [2]string{name, f[name]})
	}
	return append(out, extra...)
}

func (x *harness) postTo(host, target string, f postForm) *httptest.ResponseRecorder {
	x.t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for _, kv := range f.fields {
		_ = w.WriteField(kv[0], kv[1])
	}
	if !f.noFile {
		fw, _ := w.CreateFormFile("file", f.filename)
		_, _ = fw.Write([]byte(f.file))
	}
	for _, kv := range f.after {
		_ = w.WriteField(kv[0], kv[1])
	}
	_ = w.Close()
	r := httptest.NewRequest(http.MethodPost, "http://"+host+target, &body)
	r.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)
	return rec
}

func (x *harness) post(target string, f postForm) *httptest.ResponseRecorder {
	return x.postTo("pb.test", target, f)
}

func TestPostObject(t *testing.T) {
	x := newHarness(t, func(o *Options) {
		o.MaxObjectSize = 20
		o.Domains = []string{"s3.test"}
		o.Credentials = append(o.Credentials,
			Credential{AccessKey: "reader", SecretKey: "reader-secret", ReadOnly: true},
			Credential{AccessKey: "scoped", SecretKey: "scoped-secret", Buckets: []string{"other"}})
	})
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	exp := testNow.Add(time.Hour)
	conds := []any{map[string]string{"bucket": "bkt"}, []string{"starts-with", "$key", "up/"}, map[string]string{"Content-Type": "text/plain"},
		[]string{"starts-with", "$x-amz-meta-tag", ""}}
	form := func(extra ...[2]string) postForm {
		fields := signedForm(x.signer, exp, conds, append([][2]string{{"key", "up/${filename}"}, {"Content-Type", "text/plain"}, {"x-amz-meta-tag", "t1"}}, extra...)...)
		return postForm{fields: fields, filename: "dir/hello.txt", file: "hello", after: [][2]string{{"ignored-after-file", "x"}}}
	}

	rec := x.post("/bkt", form())
	if rec.Code != http.StatusNoContent || rec.Header().Get("ETag") == "" || rec.Header().Get("Location") != "http://pb.test/bkt/up/hello.txt" {
		t.Fatalf("post: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	got := x.mustDo(http.MethodGet, "/bkt/up/hello.txt", nil, nil, http.StatusOK)
	if got.Body.String() != "hello" || got.Header().Get("Content-Type") != "text/plain" || rawHeader(got.Header(), "x-amz-meta-tag") != "t1" {
		t.Errorf("stored: %v %q", got.Header(), got.Body.String())
	}

	statusConds := append(conds, []string{"starts-with", "$success_action_status", ""}, []string{"starts-with", "$success_action_redirect", ""})
	statusForm := func(extra ...[2]string) postForm {
		fields := signedForm(x.signer, exp, statusConds, append([][2]string{{"key", "up/s"}, {"Content-Type", "text/plain"}, {"x-amz-meta-tag", ""}}, extra...)...)
		return postForm{fields: fields, filename: "s", file: "s"}
	}
	rec = x.post("/bkt", statusForm([2]string{"success_action_status", "201"}))
	var doc postResponse
	if rec.Code != http.StatusCreated || xml.Unmarshal(rec.Body.Bytes(), &doc) != nil || doc.Key != "up/s" || doc.Bucket != "bkt" || doc.ETag == "" {
		t.Errorf("201: %d %s", rec.Code, rec.Body.String())
	}
	if rec := x.post("/bkt", statusForm([2]string{"success_action_status", "200"})); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("200: %d %s", rec.Code, rec.Body.String())
	}
	if rec := x.post("/bkt", statusForm([2]string{"success_action_status", "418"})); rec.Code != http.StatusNoContent {
		t.Errorf("unknown status: %d", rec.Code)
	}
	rec = x.post("/bkt", statusForm([2]string{"success_action_redirect", "https://app.example/done?x=1"}))
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "https://app.example/done?") ||
		!strings.Contains(loc, "key=up%2Fs") || !strings.Contains(loc, "x=1") || !strings.Contains(loc, "etag=") {
		t.Errorf("redirect: %d %s", rec.Code, loc)
	}
	if rec := x.post("/bkt", statusForm([2]string{"x-ignore-me", "anything"})); rec.Code != http.StatusNoContent {
		t.Errorf("x-ignore- field: %d %s", rec.Code, rec.Body.String())
	}

	// On a virtual host the bucket comes from the host, and so does the Location.
	if rec := x.postTo("bkt.s3.test", "/", form()); rec.Code != http.StatusNoContent || rec.Header().Get("Location") != "http://bkt.s3.test/up/hello.txt" {
		t.Errorf("virtual host: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}

	cases := []struct {
		name string
		f    postForm
		code string
	}{
		{"extra field", form([2]string{"x-amz-meta-other", "1"}), "AccessDenied"},
		{"condition failed", postForm{fields: signedForm(x.signer, exp, conds, [2]string{"key", "elsewhere/x"}, [2]string{"Content-Type", "text/plain"}, [2]string{"x-amz-meta-tag", ""}), file: "x"}, "AccessDenied"},
		{"eq failed", postForm{fields: signedForm(x.signer, exp, conds, [2]string{"key", "up/x"}, [2]string{"Content-Type", "text/html"}, [2]string{"x-amz-meta-tag", ""}), file: "x"}, "AccessDenied"},
		{"wrong bucket", postForm{fields: signedForm(x.signer, exp, []any{map[string]string{"bucket": "other"}, map[string]string{"key": "k"}}, [2]string{"key", "k"}), file: "x"}, "AccessDenied"},
		{"expired", postForm{fields: signedForm(x.signer, testNow.Add(-time.Minute), conds, [2]string{"key", "up/x"}), file: "x"}, "AccessDenied"},
		{"no key", postForm{fields: signedForm(x.signer, exp, nil), file: "x"}, "InvalidArgument"},
		{"no file", postForm{fields: signedForm(x.signer, exp, []any{map[string]string{"key": "k"}}, [2]string{"key", "k"}), noFile: true}, "InvalidArgument"},
		{"anonymous", postForm{fields: [][2]string{{"key", "k"}}, file: "x"}, "AccessDenied"},
		{"signature v2", postForm{fields: [][2]string{{"key", "k"}, {"AWSAccessKeyId", "AK"}, {"policy", "e30="}, {"signature", "x"}}, file: "x"}, "AccessDenied"},
		{"bad algorithm", postForm{fields: [][2]string{{"key", "k"}, {"x-amz-algorithm", "AWS4-HMAC-SHA1"}, {"policy", "e30="}}, file: "x"}, "InvalidArgument"},
		{"bad credential", postForm{fields: [][2]string{{"key", "k"}, {"x-amz-algorithm", "AWS4-HMAC-SHA256"}, {"x-amz-credential", "AK/x"}}, file: "x"}, "InvalidArgument"},
		{"bad date", postForm{fields: [][2]string{{"key", "k"}, {"x-amz-algorithm", "AWS4-HMAC-SHA256"}, {"x-amz-credential", "AK/20261022/us-east-1/s3/aws4_request"}, {"x-amz-date", "yesterday"}}, file: "x"}, "InvalidArgument"},
		{"no signature", postForm{fields: [][2]string{{"key", "k"}, {"x-amz-algorithm", "AWS4-HMAC-SHA256"}, {"x-amz-credential", "AK/20261022/us-east-1/s3/aws4_request"}, {"x-amz-date", "20261022T120000Z"}}, file: "x"}, "InvalidArgument"},
		{"unknown key", postForm{fields: signedForm(sigv4test.Signer{AccessKey: "nobody", SecretKey: "x", Region: "us-east-1"}, exp, nil, [2]string{"key", "k"}), file: "x"}, "InvalidAccessKeyId"},
		{"bad signature", postForm{fields: signedForm(sigv4test.Signer{AccessKey: "AK", SecretKey: "wrong", Region: "us-east-1"}, exp, nil, [2]string{"key", "k"}), file: "x"}, "SignatureDoesNotMatch"},
		{"read-only key", postForm{fields: signedForm(sigv4test.Signer{AccessKey: "reader", SecretKey: "reader-secret", Region: "us-east-1"}, exp, []any{map[string]string{"key": "k"}}, [2]string{"key", "k"}), file: "x"}, "AccessDenied"},
		{"other bucket's key", postForm{fields: signedForm(sigv4test.Signer{AccessKey: "scoped", SecretKey: "scoped-secret", Region: "us-east-1"}, exp, []any{map[string]string{"key": "k"}}, [2]string{"key", "k"}), file: "x"}, "AccessDenied"},
		{"too large for the server", postForm{fields: signedForm(x.signer, exp, []any{map[string]string{"key": "big"}}, [2]string{"key", "big"}), file: strings.Repeat("x", 21)}, "EntityTooLarge"},
		{"above the range", postForm{fields: signedForm(x.signer, exp, []any{map[string]string{"key": "r"}, []any{"content-length-range", 2, 4}}, [2]string{"key", "r"}), file: "12345"}, "EntityTooLarge"},
		{"below the range", postForm{fields: signedForm(x.signer, exp, []any{map[string]string{"key": "r"}, []any{"content-length-range", "2", "4"}}, [2]string{"key", "r"}), file: "1"}, "EntityTooSmall"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if rec := x.post("/bkt", c.f); errCode(rec) != c.code {
				t.Errorf("code %q (%d), want %s: %s", errCode(rec), rec.Code, c.code, rec.Body.String())
			}
		})
	}
	for _, k := range []string{"big", "r", "k", "elsewhere/x", "up/x"} {
		x.mustDo(http.MethodHead, "/bkt/"+k, nil, nil, http.StatusNotFound)
	}
	if rec := x.post("/bkt", postForm{fields: signedForm(x.signer, exp, []any{map[string]string{"key": "r"}, []any{"content-length-range", 2, 4}}, [2]string{"key", "r"}), file: "123"}); rec.Code != http.StatusNoContent {
		t.Errorf("within the range: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPostPolicyDocument(t *testing.T) {
	fields := map[string]string{"key": "k"}
	enc := func(doc string) string { return base64.StdEncoding.EncodeToString([]byte(doc)) }
	exp := testNow.Add(time.Hour).Format(time.RFC3339)
	for _, doc := range []string{
		"not base64!",
		enc("{"),
		enc(`{"conditions": []}`),
		enc(`{"expiration": "tomorrow", "conditions": []}`),
		enc(`{"expiration": "` + exp + `"}`),
		enc(`{"expiration": "` + exp + `", "conditions": [["eq", "$key"]]}`),
		enc(`{"expiration": "` + exp + `", "conditions": [["matches", "$key", "k"]]}`),
		enc(`{"expiration": "` + exp + `", "conditions": [["eq", "key", "k"]]}`),
		enc(`{"expiration": "` + exp + `", "conditions": [["content-length-range", 5, 1]]}`),
		enc(`{"expiration": "` + exp + `", "conditions": [["content-length-range", -1, 1]]}`),
		enc(`{"expiration": "` + exp + `", "conditions": [{"key": 1}]}`),
	} {
		if _, _, err := checkPostPolicy(doc, "b", fields, testNow); errCodeOf(err) != "InvalidPolicyDocument" {
			t.Errorf("%s: %v", doc, err)
		}
	}
	// Field names are matched without regard to case, and starts-with "" allows anything.
	ok := enc(`{"expiration": "` + exp + `", "conditions": [{"KeY": "k"}, ["starts-with", "$X-Amz-Meta-A", ""]]}`)
	if _, _, err := checkPostPolicy(ok, "b", map[string]string{"key": "k", "x-amz-meta-a": "v"}, testNow); err != nil {
		t.Errorf("case-insensitive fields: %v", err)
	}
}

func errCodeOf(err error) string {
	var e *s3err.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestPostMalformed(t *testing.T) {
	x := newHarness(t, nil)
	x.mustDo(http.MethodPut, "/bkt", nil, nil, http.StatusOK)
	r := httptest.NewRequest(http.MethodPost, "http://pb.test/bkt", strings.NewReader("--nope"))
	r.Header.Set("Content-Type", "multipart/form-data; boundary=b")
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)
	if errCode(rec) != "MalformedPOSTRequest" {
		t.Errorf("malformed: %s", rec.Body.String())
	}
	r = httptest.NewRequest(http.MethodPost, "http://pb.test/bkt", strings.NewReader(""))
	r.Header.Set("Content-Type", "multipart/form-data")
	rec = httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)
	if errCode(rec) != "MalformedPOSTRequest" {
		t.Errorf("no boundary: %s", rec.Body.String())
	}
	big := postForm{fields: [][2]string{{"key", strings.Repeat("k", maxPostField+1)}}, file: "x"}
	if rec := x.post("/bkt", big); errCode(rec) != "MaxPostPreDataLengthExceededError" {
		t.Errorf("large field: %s", rec.Body.String())
	}
}
