package s3api

import (
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/somaz94/plainbucket/internal/notify"
	"github.com/somaz94/plainbucket/internal/s3err"
	"github.com/somaz94/plainbucket/internal/sigv4"
	"github.com/somaz94/plainbucket/internal/storage"
)

const (
	maxPostField  = 64 << 10 // one form field before the file
	maxPostFields = 1 << 20  // all of them
)

// isPostUpload reports whether a request is a browser POST upload: a multipart form
// posted to a bucket. It authenticates through its form fields, not its headers.
func isPostUpload(r *http.Request, key string, q url.Values) bool {
	if r.Method != http.MethodPost || key != "" || q.Has("delete") {
		return false
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == "multipart/form-data"
}

// postObject serves PostObject, the HTML form upload a presigned POST policy allows.
// Only Signature Version 4 is accepted, and anonymous uploads never are.
func (h *Handler) postObject(req *request) error {
	mr, err := req.r.MultipartReader()
	if err != nil {
		return s3err.MalformedPOSTRequest()
	}
	fields, file, err := readPostFields(mr)
	if err != nil {
		return err
	}
	if file == nil {
		return s3err.InvalidArgument("POST requires exactly one file upload per request.")
	}
	defer func() { _ = file.Close() }()

	if fields["x-amz-signature"] == "" && fields["policy"] == "" && fields["x-amz-credential"] == "" {
		return s3err.AccessDenied("Anonymous POST uploads are not allowed.")
	}
	a := h.access.Load()
	auth, e := sigv4.VerifyPost(fields, a.secret)
	if e != nil {
		return e
	}
	req.auth, req.cred = auth, a.creds[auth.AccessKey]
	if err := req.authorize(); err != nil {
		return err
	}
	lm := h.keyLimiter(req)
	if err := h.throttle(req, lm, false); err != nil {
		return err
	}
	minLen, maxLen, err := checkPostPolicy(fields["policy"], req.bucket, fields, h.now())
	if err != nil {
		return err
	}
	key, ok := fields["key"]
	if !ok || key == "" {
		return s3err.InvalidArgument("Bucket POST must contain a field named 'key'.")
	}
	req.key = strings.ReplaceAll(key, "${filename}", path.Base(file.FileName()))

	hdr := http.Header{}
	for name, v := range fields {
		hdr.Set(name, v)
	}
	if hdr.Get("Content-Type") == "" {
		hdr.Set("Content-Type", file.Header.Get("Content-Type"))
	}
	if h.opts.MaxObjectSize > 0 && (maxLen < 0 || h.opts.MaxObjectSize < maxLen) {
		maxLen = h.opts.MaxObjectSize
	}
	md := metadataFromHeaders(hdr)
	if doc := fields["tagging"]; doc != "" {
		if md.Tags, err = parseTagging([]byte(doc)); err != nil {
			return err
		}
	}
	// The multipart reader holds the body from before throttle wrapped it.
	var src io.Reader = file
	if lm != nil {
		src = lm.Reader(req.r.Context(), file)
	}
	body := &sizeRange{r: src, min: minLen, max: maxLen}
	info, err := h.store.PutObject(req.bucket, req.key, body, -1, storage.PutOptions{Metadata: md})
	if err != nil {
		return err
	}
	h.metrics.receivedBytes.Add(info.Size)
	h.publish(req, notify.ObjectCreatedPost, req.key, info)
	return h.postResponse(req, fields, info.ETag)
}

// readPostFields reads the form up to the file part, which it returns unread; fields
// after the file are ignored, as on S3.
func readPostFields(mr *multipart.Reader) (map[string]string, *multipart.Part, error) {
	fields := map[string]string{}
	total := 0
	for parts := 0; ; parts++ {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) && parts > 0 {
			return fields, nil, nil
		}
		if err != nil {
			return nil, nil, s3err.MalformedPOSTRequest()
		}
		name := strings.ToLower(part.FormName())
		if name == "file" {
			return fields, part, nil
		}
		v, err := io.ReadAll(io.LimitReader(part, maxPostField+1))
		_ = part.Close()
		if err != nil {
			return nil, nil, s3err.MalformedPOSTRequest()
		}
		if total += len(v); len(v) > maxPostField || total > maxPostFields {
			return nil, nil, s3err.MaxPostPreDataLengthExceeded()
		}
		if _, dup := fields[name]; !dup && name != "" {
			fields[name] = string(v)
		}
	}
}

// postPolicy is the decoded policy document of a POST upload.
type postPolicy struct {
	Expiration string            `json:"expiration"`
	Conditions []json.RawMessage `json:"conditions"`
}

// postExempt reports the fields a policy need not mention.
func postExempt(name string) bool {
	return name == "policy" || name == "x-amz-signature" || name == "file" || strings.HasPrefix(name, "x-ignore-")
}

// checkPostPolicy enforces the policy the form was signed with and returns the
// content-length-range, -1 for an unset bound.
func checkPostPolicy(policy, bucket string, fields map[string]string, now time.Time) (minLen, maxLen int64, err error) {
	raw, err := base64.StdEncoding.DecodeString(policy)
	var p postPolicy
	if err != nil || json.Unmarshal(raw, &p) != nil {
		return 0, 0, s3err.InvalidPolicyDocument("Invalid Policy: Invalid JSON.")
	}
	if p.Expiration == "" {
		return 0, 0, s3err.InvalidPolicyDocument("Invalid Policy: Policy missing expiration.")
	}
	exp, err := time.Parse(time.RFC3339, p.Expiration)
	if err != nil {
		return 0, 0, s3err.InvalidPolicyDocument("Invalid Policy: Invalid 'expiration' value: '" + p.Expiration + "'")
	}
	if p.Conditions == nil {
		return 0, 0, s3err.InvalidPolicyDocument("Invalid Policy: Policy missing conditions.")
	}
	if now.After(exp) {
		return 0, 0, s3err.AccessDenied("Invalid according to Policy: Policy expired.")
	}

	// An absent field counts as empty, so ["starts-with", "$x", ""] allows omitting it.
	value := func(name string) string {
		if name == "bucket" {
			return bucket
		}
		return fields[name]
	}
	failed := func(c json.RawMessage) error {
		return s3err.AccessDenied("Invalid according to Policy: Policy Condition failed: " + string(c))
	}
	covered := map[string]bool{}
	minLen, maxLen = -1, -1
	for _, c := range p.Conditions {
		var exact map[string]string
		if json.Unmarshal(c, &exact) == nil {
			for k, want := range exact {
				name := strings.ToLower(k)
				covered[name] = true
				if value(name) != want {
					return 0, 0, failed(c)
				}
			}
			continue
		}
		var op []json.RawMessage
		if json.Unmarshal(c, &op) != nil || len(op) != 3 {
			return 0, 0, s3err.InvalidPolicyDocument("Invalid Policy: Invalid Condition: " + string(c))
		}
		var kind string
		_ = json.Unmarshal(op[0], &kind)
		if kind == "content-length-range" {
			lo, err1 := strconv.ParseInt(strings.Trim(string(op[1]), `"`), 10, 64)
			hi, err2 := strconv.ParseInt(strings.Trim(string(op[2]), `"`), 10, 64)
			if err1 != nil || err2 != nil || lo < 0 || hi < lo {
				return 0, 0, s3err.InvalidPolicyDocument("Invalid Policy: Invalid content-length-range: " + string(c))
			}
			minLen, maxLen = lo, hi
			continue
		}
		var target, want string
		if json.Unmarshal(op[1], &target) != nil || json.Unmarshal(op[2], &want) != nil || !strings.HasPrefix(target, "$") ||
			(kind != "eq" && kind != "starts-with") {
			return 0, 0, s3err.InvalidPolicyDocument("Invalid Policy: Invalid Condition: " + string(c))
		}
		name := strings.ToLower(target[1:])
		covered[name] = true
		got := value(name)
		if (kind == "eq" && got != want) || (kind == "starts-with" && !strings.HasPrefix(got, want)) {
			return 0, 0, failed(c)
		}
	}
	for name := range fields {
		if !covered[name] && !postExempt(name) {
			return 0, 0, s3err.AccessDenied("Invalid according to Policy: Extra input fields: " + name)
		}
	}
	return minLen, maxLen, nil
}

// sizeRange passes a file through, failing once it passes max or, at its end, if it is
// shorter than min, so a refused upload is never committed.
type sizeRange struct {
	r        io.Reader
	n        int64
	min, max int64 // -1 for none
}

func (s *sizeRange) Read(b []byte) (int, error) {
	n, err := s.r.Read(b)
	s.n += int64(n)
	if s.max >= 0 && s.n > s.max {
		return n, s3err.EntityTooLarge()
	}
	if errors.Is(err, io.EOF) && s.n < s.min {
		return n, s3err.EntityTooSmall()
	}
	return n, err
}

type postResponse struct {
	XMLName  xml.Name `xml:"PostResponse"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

// postResponse answers as the form asks: a redirect, an empty 200, a 201 with a
// document, or by default 204.
func (h *Handler) postResponse(req *request, fields map[string]string, etag string) error {
	scheme := "http"
	if req.r.TLS != nil {
		scheme = "https"
	}
	location := scheme + "://" + req.r.Host + "/" + sigv4.URIEncodePath(req.key)
	if !req.vhost {
		location = scheme + "://" + req.r.Host + "/" + req.bucket + "/" + sigv4.URIEncodePath(req.key)
	}
	hdr := req.w.Header()
	hdr.Set("ETag", etag)
	hdr.Set("Location", location)

	redirect := fields["success_action_redirect"]
	if redirect == "" {
		redirect = fields["redirect"]
	}
	if u, err := url.Parse(redirect); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		q := u.Query()
		q.Set("bucket", req.bucket)
		q.Set("key", req.key)
		q.Set("etag", etag)
		u.RawQuery = q.Encode()
		http.Redirect(req.w, req.r, u.String(), http.StatusSeeOther)
		return nil
	}
	switch fields["success_action_status"] {
	case "200":
		req.w.WriteHeader(http.StatusOK)
	case "201":
		writeXML(req.w, http.StatusCreated, postResponse{Location: location, Bucket: req.bucket, Key: req.key, ETag: etag})
	default:
		req.w.WriteHeader(http.StatusNoContent)
	}
	return nil
}
