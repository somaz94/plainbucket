package s3api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/somaz94/plainbucket/internal/ratelimit"
	"github.com/somaz94/plainbucket/internal/sigv4/sigv4test"
)

// assume sends a signed AssumeRole request with the given form values.
func (x *harness) assume(ak, sk string, form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	x.t.Helper()
	return x.assumeBody(ak, sk, form.Encode(), hdr)
}

func (x *harness) assumeBody(ak, sk, body string, hdr map[string]string) *httptest.ResponseRecorder {
	x.t.Helper()
	r := httptest.NewRequest(http.MethodPost, "http://pb.test/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	sum := sha256.Sum256([]byte(body))
	sigv4test.Signer{AccessKey: ak, SecretKey: sk, Region: "us-east-1", Service: "sts"}.Sign(r, hex.EncodeToString(sum[:]), testNow)
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)
	return rec
}

func roleForm(extra ...string) url.Values {
	f := url.Values{"Action": {"AssumeRole"}, "Version": {"2011-06-15"}, "RoleArn": {"arn:aws:iam::000000000000:role/ci"}, "RoleSessionName": {"job-1"}}
	for i := 0; i+1 < len(extra); i += 2 {
		f.Set(extra[i], extra[i+1])
	}
	return f
}

func stsCode(rec *httptest.ResponseRecorder) string {
	var e stsErrorResponse
	_ = xml.Unmarshal(rec.Body.Bytes(), &e)
	return e.Error.Code
}

func issued(t *testing.T, rec *httptest.ResponseRecorder) stsCredentials {
	t.Helper()
	var res assumeRoleResponse
	if rec.Code != http.StatusOK || xml.Unmarshal(rec.Body.Bytes(), &res) != nil || res.Result.Credentials.SessionToken == "" {
		t.Fatalf("assume role: %d %s", rec.Code, rec.Body.String())
	}
	return res.Result.Credentials
}

func TestAssumeRoleAndTemporaryKeys(t *testing.T) {
	x := newHarness(t, func(o *Options) {
		o.AccessLog = true
		o.Credentials = append(o.Credentials, Credential{AccessKey: "scoped", SecretKey: "scoped-secret", Buckets: []string{"aaa", "bbb"}})
	})
	for _, b := range []string{"/aaa", "/bbb", "/ccc"} {
		x.mustDo(http.MethodPut, b, nil, nil, http.StatusOK)
		x.mustDo(http.MethodPut, b+"/k", []byte("data"), nil, http.StatusOK)
	}
	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject","s3:ListBucket"],"Resource":["arn:aws:s3:::aaa","arn:aws:s3:::aaa/*"]}]}`
	rec := x.assume("AK", "SK", roleForm("Policy", policy, "DurationSeconds", "900"), nil)
	c := issued(t, rec)
	if !strings.HasPrefix(c.AccessKeyID, "ASIA") || c.Expiration != isoTime(testNow.Add(15*time.Minute)) {
		t.Errorf("credentials %+v", c)
	}
	token := map[string]string{"X-Amz-Security-Token": c.SessionToken}
	x.as(c.AccessKeyID, c.SecretAccessKey, func() {
		x.mustDo(http.MethodGet, "/aaa/k", nil, token, http.StatusOK)
		x.mustDo(http.MethodGet, "/aaa?list-type=2", nil, token, http.StatusOK)
		x.expectErr(http.MethodPut, "/aaa/new", []byte("x"), token, "AccessDenied") // read-only
		x.expectErr(http.MethodGet, "/bbb/k", nil, token, "AccessDenied")           // outside the policy
		// Without its token the key is unknown; an altered token is refused before signing.
		x.expectErr(http.MethodGet, "/aaa/k", nil, nil, "InvalidAccessKeyId")
		x.expectErr(http.MethodGet, "/aaa/k", nil, map[string]string{"X-Amz-Security-Token": c.SessionToken + "x"}, "InvalidToken")
		// A temporary key cannot issue another.
		if rec := x.assume(c.AccessKeyID, c.SecretAccessKey, roleForm(), token); stsCode(rec) != "AccessDenied" {
			t.Errorf("chained assume: %d %s", rec.Code, rec.Body.String())
		}
		// Nor sign a POST upload, whose form carries no token the server reads.
		form := signedForm(x.signer, testNow.Add(time.Hour), []any{map[string]string{"bucket": "aaa"}, []any{"starts-with", "$key", ""}},
			[2]string{"key", "posted"}, [2]string{"x-amz-security-token", c.SessionToken})
		if rec := x.post("/aaa", postForm{fields: form, file: "x"}); errCode(rec) != "InvalidAccessKeyId" {
			t.Errorf("post with a temporary key: %d %s", rec.Code, rec.Body.String())
		}
	})
	for _, want := range []string{"access_key=" + c.AccessKeyID, "issuer=AK", "session=job-1"} {
		if !strings.Contains(x.logs.String(), want) {
			t.Errorf("access log lacks %q", want)
		}
	}

	// The token also works in a presigned URL.
	r := httptest.NewRequest(http.MethodGet, "http://pb.test/aaa/k", nil)
	sigv4test.Signer{AccessKey: c.AccessKeyID, SecretKey: c.SecretAccessKey, Region: "us-east-1"}.Presign(r, testNow, 600, map[string]string{"X-Amz-Security-Token": c.SessionToken})
	pre := httptest.NewRecorder()
	if x.h.ServeHTTP(pre, r); pre.Code != http.StatusOK || pre.Body.String() != "data" {
		t.Errorf("presigned with a token: %d %s", pre.Code, pre.Body.String())
	}

	// A scoped key's session never reaches past its scope.
	x.as("scoped", "scoped-secret", func() {
		if rec := x.assume("scoped", "scoped-secret", roleForm("Policy", `{"Statement":{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::ccc/*"}}`), nil); stsCode(rec) != "AccessDenied" {
			t.Errorf("policy beyond the scope: %s", rec.Body.String())
		}
	})
	sc := issued(t, x.assume("scoped", "scoped-secret", roleForm(), nil))
	x.as(sc.AccessKeyID, sc.SecretAccessKey, func() {
		tok := map[string]string{"X-Amz-Security-Token": sc.SessionToken}
		x.mustDo(http.MethodPut, "/bbb/new", []byte("x"), tok, http.StatusOK)
		x.expectErr(http.MethodGet, "/ccc/k", nil, tok, "AccessDenied")
	})

	// The issuer's scope as it is now still bounds the session: with no bucket in common, none.
	nc := issued(t, x.assume("scoped", "scoped-secret", roleForm("Policy", `{"Statement":{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::aaa/*"}}`), nil))
	x.as(nc.AccessKeyID, nc.SecretAccessKey, func() {
		tok := map[string]string{"X-Amz-Security-Token": nc.SessionToken}
		x.mustDo(http.MethodGet, "/aaa/k", nil, tok, http.StatusOK)
		x.expectErr(http.MethodGet, "/bbb/k", nil, tok, "AccessDenied")
	})
	x.h.SetAccess([]Credential{{AccessKey: "AK", SecretKey: "SK"}, {AccessKey: "scoped", SecretKey: "scoped-secret", Buckets: []string{"bbb"}}}, nil, nil)
	x.as(nc.AccessKeyID, nc.SecretAccessKey, func() {
		tok := map[string]string{"X-Amz-Security-Token": nc.SessionToken}
		x.expectErr(http.MethodGet, "/aaa/k", nil, tok, "AccessDenied")
		x.expectErr(http.MethodGet, "/bbb/k", nil, tok, "AccessDenied")
		if body := x.mustDo(http.MethodGet, "/", nil, tok, http.StatusOK).Body.String(); strings.Contains(body, "<Name>") {
			t.Errorf("buckets listed with no scope left: %s", body)
		}
	})

	// Expiry, and the end of the issuing key, end the session.
	x.h.now = func() time.Time { return testNow.Add(16 * time.Minute) }
	x.as(c.AccessKeyID, c.SecretAccessKey, func() { x.expectErr(http.MethodGet, "/aaa/k", nil, token, "ExpiredToken") })
	x.h.now = func() time.Time { return testNow }
	x.h.SetAccess([]Credential{{AccessKey: "AK", SecretKey: "rotated"}}, nil, nil)
	x.as(c.AccessKeyID, c.SecretAccessKey, func() { x.expectErr(http.MethodGet, "/aaa/k", nil, token, "InvalidToken") })
	x.h.SetAccess([]Credential{{AccessKey: "other", SecretKey: "other-secret"}}, nil, nil)
	x.as(c.AccessKeyID, c.SecretAccessKey, func() { x.expectErr(http.MethodGet, "/aaa/k", nil, token, "InvalidToken") })
}

func TestAssumeRoleRefuses(t *testing.T) {
	x := newHarness(t, nil)
	for name, c := range map[string]struct {
		form url.Values
		code string
	}{
		"other action":     {url.Values{"Action": {"GetCallerIdentity"}}, "InvalidAction"},
		"no session name":  {roleForm("RoleSessionName", ""), "ValidationError"},
		"short duration":   {roleForm("DurationSeconds", "60"), "ValidationError"},
		"long duration":    {roleForm("DurationSeconds", "86400"), "ValidationError"},
		"policy not json":  {roleForm("Policy", "{"), "MalformedPolicyDocument"},
		"deny":             {roleForm("Policy", `{"Statement":[{"Effect":"Deny","Action":"s3:*","Resource":"*"}]}`), "MalformedPolicyDocument"},
		"condition":        {roleForm("Policy", `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{}}]}`), "MalformedPolicyDocument"},
		"iam action":       {roleForm("Policy", `{"Statement":[{"Effect":"Allow","Action":"iam:*","Resource":"*"}]}`), "MalformedPolicyDocument"},
		"other resource":   {roleForm("Policy", `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:sqs:::q"}]}`), "MalformedPolicyDocument"},
		"wildcard bucket":  {roleForm("Policy", `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::logs-*"}]}`), "MalformedPolicyDocument"},
		"no action":        {roleForm("Policy", `{"Statement":[{"Effect":"Allow","Resource":"*"}]}`), "MalformedPolicyDocument"},
		"statement number": {roleForm("Policy", `{"Statement":5}`), "MalformedPolicyDocument"},
		"no statement":     {roleForm("Policy", `{"Statement":[]}`), "MalformedPolicyDocument"},
		"null statement":   {roleForm("Policy", `{"Statement":null}`), "MalformedPolicyDocument"},
		"not an object":    {roleForm("Policy", `[]`), "MalformedPolicyDocument"},
		"other key":        {roleForm("Policy", `{"statement":{"Effect":"Allow","Action":"s3:*","Resource":"*"}}`), "MalformedPolicyDocument"},
		"key prefix":       {roleForm("Policy", `{"Statement":{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::a/x/*"}}`), "MalformedPolicyDocument"},
		"one write action": {roleForm("Policy", `{"Statement":{"Effect":"Allow","Action":"s3:PutObject","Resource":"*"}}`), "MalformedPolicyDocument"},
		"mixed statements": {roleForm("Policy", `{"Statement":[{"Effect":"Allow","Action":"s3:List*","Resource":"arn:aws:s3:::a"},{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::b"}]}`), "MalformedPolicyDocument"},
		"many buckets":     {roleForm("Policy", manyBuckets(maxPolicyBuckets+1)), "MalformedPolicyDocument"},
		"duration wraps":   {roleForm("DurationSeconds", "18446745974"), "ValidationError"},
		"duration text":    {roleForm("DurationSeconds", "1h"), "ValidationError"},
		"no resource":      {roleForm("Policy", `{"Statement":{"Effect":"Allow","Action":"s3:*"}}`), "MalformedPolicyDocument"},
	} {
		if rec := x.assume("AK", "SK", c.form, nil); rec.Code == http.StatusOK || stsCode(rec) != c.code {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	// Unsigned, it is refused as an S3 error would be, in STS's form.
	r := httptest.NewRequest(http.MethodPost, "http://pb.test/", strings.NewReader("Action=AssumeRole"))
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden || stsCode(rec) != "AccessDenied" {
		t.Errorf("unsigned: %d %s", rec.Code, rec.Body.String())
	}
	if rec := x.assume("AK", "wrong", roleForm(), nil); stsCode(rec) != "SignatureDoesNotMatch" {
		t.Errorf("wrong secret: %s", rec.Body.String())
	}
	if rec := x.assumeBody("AK", "SK", "Action=%zz", nil); stsCode(rec) != "InvalidParameterValue" {
		t.Errorf("not a form: %s", rec.Body.String())
	}
	r = httptest.NewRequest(http.MethodPost, "http://pb.test/", iotest.ErrReader(errors.New("reset")))
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AK")
	var e stsErrorResponse
	rec = httptest.NewRecorder()
	if x.h.ServeHTTP(rec, r); rec.Code != http.StatusInternalServerError || xml.Unmarshal(rec.Body.Bytes(), &e) != nil || e.Error.Type != "Receiver" {
		t.Errorf("unreadable body: %d %s", rec.Code, rec.Body.String())
	}
	big := roleForm("Policy", strings.Repeat("x", maxSTSBody))
	if rec := x.assume("AK", "SK", big, nil); stsCode(rec) != "InvalidParameterValue" {
		t.Errorf("large body: %s", rec.Body.String())
	}
}

func TestSessionPolicyScope(t *testing.T) {
	for doc, want := range map[string]struct {
		buckets  string
		readOnly bool
	}{
		``: {"", false},
		`{"Statement":{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::a/*"}}`:                                                                        {"a", true},
		`{"Statement":[{"Effect":"Allow","Action":["s3:List*"],"Resource":"arn:aws:s3:::a"},{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`: {"a,b", true},
		`{"Statement":{"Effect":"Allow","Action":"s3:Get*","Resource":["arn:aws:s3:::a/*","*"]}}`:                                                                       {"", true},
		`{"Statement":{"Sid":"x","Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::*"}}`:                                                                        {"", false},
	} {
		b, ro, err := parseSessionPolicy(doc)
		if err != nil || strings.Join(b, ",") != want.buckets || ro != want.readOnly {
			t.Errorf("%s: %q %v %v", doc, b, ro, err)
		}
	}
	if b, _, err := parseSessionPolicy(manyBuckets(maxPolicyBuckets)); err != nil || len(b) != maxPolicyBuckets {
		t.Errorf("%d buckets: %d %v", maxPolicyBuckets, len(b), err)
	}
}

func manyBuckets(n int) string {
	arns := make([]string, n)
	for i := range arns {
		arns[i] = `"arn:aws:s3:::b` + strconv.Itoa(i) + `"`
	}
	return `{"Statement":{"Effect":"Allow","Action":"s3:*","Resource":[` + strings.Join(arns, ",") + `]}}`
}

func TestTemporaryKeySharesRateLimit(t *testing.T) {
	x := newHarness(t, func(o *Options) {
		o.Credentials = []Credential{{AccessKey: "AK", SecretKey: "SK", RateLimit: ratelimit.Limit{Requests: 0.001, Burst: 2}}}
	})
	c := issued(t, x.assume("AK", "SK", roleForm(), nil)) // the first of AK's two
	x.as(c.AccessKeyID, c.SecretAccessKey, func() {
		tok := map[string]string{"X-Amz-Security-Token": c.SessionToken}
		x.expectErr(http.MethodGet, "/", nil, tok, "")
		x.expectErr(http.MethodGet, "/", nil, tok, "SlowDown")
	})
	// A server-side refusal is the receiver's error in STS's form.
	var e stsErrorResponse
	if rec := x.assume("AK", "SK", roleForm(), nil); rec.Code != http.StatusServiceUnavailable || xml.Unmarshal(rec.Body.Bytes(), &e) != nil || e.Error.Type != "Receiver" {
		t.Errorf("throttled assume: %d %s", rec.Code, rec.Body.String())
	}
}

func TestUnsignedAssumeRoleIsThrottled(t *testing.T) {
	x := newHarness(t, func(o *Options) { o.AnonymousRateLimit = ratelimit.Limit{Requests: 0.001, Burst: 1} })
	for _, want := range []string{"AccessDenied", "SlowDown"} {
		rec := httptest.NewRecorder()
		x.h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://pb.test/", strings.NewReader("Action=AssumeRole")))
		if stsCode(rec) != want {
			t.Errorf("unsigned assume: %d %s, want %s", rec.Code, rec.Body.String(), want)
		}
	}
	if got := stsFail("Code", "message").Error(); got != "Code: message" {
		t.Errorf("stsError = %q", got)
	}
}
