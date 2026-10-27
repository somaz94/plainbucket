package s3api

import (
	"cmp"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/somaz94/plainbucket/internal/s3err"
	"github.com/somaz94/plainbucket/internal/session"
	"github.com/somaz94/plainbucket/internal/sigv4"
)

// STS limits: what AssumeRole allows on AWS, and the size of a form we read.
const (
	minSessionDuration     = 15 * time.Minute
	maxSessionDuration     = 12 * time.Hour
	defaultSessionDuration = time.Hour
	maxSTSBody             = 64 << 10
	maxPolicyBuckets       = 100 // keeps the token well under session.MaxTokenLen
)

const stsNS = "https://sts.amazonaws.com/doc/2011-06-15/"

// stsAccount is the account every ARN names: there are no AWS accounts here.
const stsAccount = "000000000000"

var sessionNameRe = regexp.MustCompile(`^[\w+=,.@-]{2,64}$`)

// isSTS reports a request for the STS API, which AWS SDKs send as a form POST to /.
func isSTS(r *http.Request, bucket string) bool {
	return bucket == "" && r.Method == http.MethodPost
}

type stsError struct {
	status        int
	code, message string
}

func (e *stsError) Error() string { return e.code + ": " + e.message }

func stsFail(code, msg string) *stsError { return &stsError{http.StatusBadRequest, code, msg} }

// serveSTS answers an STS request in STS's XML, errors included.
func (h *Handler) serveSTS(req *request) {
	if err := h.stsAction(req); err != nil {
		var e *stsError
		if !errors.As(err, &e) {
			s3e := toS3(err)
			if s3e == nil {
				h.log.Error("sts request failed", "request_id", req.id, "err", err)
				s3e = s3err.Internal()
			}
			e = &stsError{s3e.Status, s3e.Code, s3e.Message}
		}
		typ := "Sender"
		if e.status >= http.StatusInternalServerError {
			typ = "Receiver"
		}
		writeXML(req.w, e.status, stsErrorResponse{NS: stsNS, Error: stsErrorBody{Type: typ, Code: e.code, Message: e.message}, RequestID: req.id})
	}
}

func (h *Handler) stsAction(req *request) error {
	if req.r.Header.Get("Authorization") == "" {
		if err := h.throttle(req, h.anonLimiter(req), true); err != nil {
			return err
		}
		return &stsError{http.StatusForbidden, "AccessDenied", "STS requests must be signed."}
	}
	body, err := io.ReadAll(io.LimitReader(req.r.Body, maxSTSBody+1))
	if err != nil {
		return err
	}
	if len(body) > maxSTSBody {
		return stsFail("InvalidParameterValue", "The request body is too large.")
	}
	// Named before verifying, so a refused request is counted under the action it asked for.
	form, formErr := url.ParseQuery(string(body))
	if form.Get("Action") == "GetCallerIdentity" {
		req.op = "GetCallerIdentity"
	}
	a := h.access.Load()
	creds, temp, err := h.signingKeys(a, req.r.Header.Get("X-Amz-Security-Token"))
	if s3e := toS3(err); s3e != nil && s3e.Code == "InvalidToken" {
		return &stsError{http.StatusForbidden, "InvalidClientTokenId", "The security token included in the request is invalid."}
	} else if err != nil {
		return err
	}
	auth, e := sigv4.VerifySTS(req.r, body, creds, h.now())
	if e != nil {
		return e
	}
	req.auth, req.cred = auth, cmp.Or(temp, a.creds[auth.AccessKey])
	if err := h.throttle(req, h.keyLimiter(req), false); err != nil {
		return err
	}
	if formErr != nil {
		return stsFail("InvalidParameterValue", "The request body is not form-encoded.")
	}
	switch action := form.Get("Action"); action {
	case "AssumeRole":
		return h.assumeRole(req, form)
	case "GetCallerIdentity":
		return h.callerIdentity(req)
	default:
		return stsFail("InvalidAction", "Could not find operation "+action+" for version "+form.Get("Version")+".")
	}
}

// assumeRole issues temporary credentials: the signing key's access, narrowed by the
// request's Policy, for DurationSeconds. RoleArn names nothing here and is not checked.
func (h *Handler) assumeRole(req *request, form url.Values) error {
	if req.cred.parent != "" {
		return &stsError{http.StatusForbidden, "AccessDenied", "Temporary credentials cannot assume a role."}
	}
	name := form.Get("RoleSessionName")
	if !sessionNameRe.MatchString(name) {
		return stsFail("ValidationError", "RoleSessionName must be 2 to 64 letters, digits or +=,.@_- characters.")
	}
	duration := defaultSessionDuration
	if v := form.Get("DurationSeconds"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < int(minSessionDuration/time.Second) || n > int(maxSessionDuration/time.Second) {
			return stsFail("ValidationError", "DurationSeconds must be between 900 and 43200.")
		}
		duration = time.Duration(n) * time.Second
	}
	buckets, readOnly, err := parseSessionPolicy(form.Get("Policy"))
	if err != nil {
		return err
	}
	for _, b := range buckets {
		if !req.cred.allows(b) {
			return &stsError{http.StatusForbidden, "AccessDenied", "The policy names bucket " + b + ", which the signing key may not access."}
		}
	}
	expires := h.now().Add(duration).UTC().Truncate(time.Second)
	sc := session.Issue(req.cred.SecretKey, session.Claims{Parent: req.cred.AccessKey, Buckets: buckets, ReadOnly: readOnly, Name: name, Expires: expires})
	writeXML(req.w, http.StatusOK, assumeRoleResponse{NS: stsNS, Result: assumeRoleResult{
		Credentials: stsCredentials{AccessKeyID: sc.AccessKey, SecretAccessKey: sc.SecretKey, SessionToken: sc.Token, Expiration: isoTime(expires)},
		User:        assumedRoleUser{ID: sc.AccessKey + ":" + name, Arn: assumedRoleArn(name)},
	}, RequestID: req.id})
	return nil
}

// callerIdentity reports a long-term key as an IAM user, a temporary one as the role AssumeRole named.
func (h *Handler) callerIdentity(req *request) error {
	res := callerIdentityResult{Account: stsAccount, UserID: req.cred.AccessKey, Arn: "arn:aws:iam::" + stsAccount + ":user/" + req.cred.AccessKey}
	if req.cred.parent != "" {
		res.UserID, res.Arn = req.cred.AccessKey+":"+req.cred.session, assumedRoleArn(req.cred.session)
	}
	writeXML(req.w, http.StatusOK, callerIdentityResponse{NS: stsNS, Result: res, RequestID: req.id})
	return nil
}

func assumedRoleArn(session string) string {
	return "arn:aws:sts::" + stsAccount + ":assumed-role/plainbucket/" + session
}

// parseSessionPolicy reads the subset of an IAM session policy that a bucket scope and a
// read-only flag express exactly: Allow statements on whole buckets, all of them either
// s3:* or Get and List actions only. Anything else is refused rather than widened.
func parseSessionPolicy(doc string) (buckets []string, readOnly bool, err error) {
	if doc == "" {
		return nil, false, nil
	}
	bad := func(msg string) error { return stsFail("MalformedPolicyDocument", msg) }
	var top map[string]json.RawMessage
	if json.Unmarshal([]byte(doc), &top) != nil {
		return nil, false, bad("The policy is not a JSON object.")
	}
	for k := range top {
		if k != "Version" && k != "Id" && k != "Statement" {
			return nil, false, bad(k + " is not supported in a session policy here.")
		}
	}
	var stmts []map[string]json.RawMessage
	if json.Unmarshal(top["Statement"], &stmts) != nil {
		var one map[string]json.RawMessage
		if json.Unmarshal(top["Statement"], &one) != nil {
			return nil, false, bad("Statement must be an object or a list of them.")
		}
		stmts = []map[string]json.RawMessage{one}
	}
	if len(stmts) == 0 {
		return nil, false, bad("The policy has no statement, so it would allow nothing.")
	}
	all := false
	for i, st := range stmts {
		for k := range st {
			if k != "Sid" && k != "Effect" && k != "Action" && k != "Resource" {
				return nil, false, bad(k + " is not supported in a session policy here.")
			}
		}
		var effect string
		if json.Unmarshal(st["Effect"], &effect) != nil || effect != "Allow" {
			return nil, false, bad("Only Allow statements are supported.")
		}
		actions, ok := stringOrList(st["Action"])
		if !ok {
			return nil, false, bad("Action must be a string or a list of them.")
		}
		write := false
		for _, a := range actions {
			op, ok := strings.CutPrefix(strings.ToLower(a), "s3:")
			switch {
			case !ok:
				return nil, false, bad(a + " is not an s3 action.")
			case op == "*":
				write = true
			case !strings.HasPrefix(op, "get") && !strings.HasPrefix(op, "list"):
				return nil, false, bad(a + " cannot be expressed here: a session allows s3:*, or Get and List actions only.")
			}
		}
		if i == 0 {
			readOnly = !write
		} else if readOnly == write {
			return nil, false, bad("Every statement must allow s3:*, or every one Get and List actions only.")
		}
		resources, ok := stringOrList(st["Resource"])
		if !ok {
			return nil, false, bad("Resource must be a string or a list of them.")
		}
		for _, r := range resources {
			b, ok := policyBucket(r)
			switch {
			case !ok:
				return nil, false, bad(r + " is not a whole S3 bucket, such as arn:aws:s3:::name or arn:aws:s3:::name/*.")
			case b == "*":
				all = true
			case !slices.Contains(buckets, b):
				buckets = append(buckets, b)
			}
		}
	}
	if all {
		buckets = nil
	}
	if len(buckets) > maxPolicyBuckets {
		return nil, false, bad("The policy names more than " + strconv.Itoa(maxPolicyBuckets) + " buckets.")
	}
	return buckets, readOnly, nil
}

func stringOrList(raw json.RawMessage) ([]string, bool) {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}, true
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil && len(list) > 0 {
		return list, true
	}
	return nil, false
}

// policyBucket returns the bucket of arn:aws:s3:::name or arn:aws:s3:::name/*, or "*". A
// key prefix narrower than the whole bucket cannot be expressed.
func policyBucket(arn string) (string, bool) {
	if arn == "*" {
		return "*", true
	}
	rest, ok := strings.CutPrefix(arn, "arn:aws:s3:::")
	if !ok {
		return "", false
	}
	b, key, hasKey := strings.Cut(rest, "/")
	if hasKey && key != "*" {
		return "", false
	}
	if b == "*" {
		return "*", true
	}
	return b, b != "" && !strings.ContainsAny(b, "*?")
}

type assumeRoleResponse struct {
	XMLName   xml.Name         `xml:"AssumeRoleResponse"`
	NS        string           `xml:"xmlns,attr"`
	Result    assumeRoleResult `xml:"AssumeRoleResult"`
	RequestID string           `xml:"ResponseMetadata>RequestId"`
}

type assumeRoleResult struct {
	Credentials stsCredentials  `xml:"Credentials"`
	User        assumedRoleUser `xml:"AssumedRoleUser"`
}

type stsCredentials struct {
	AccessKeyID     string `xml:"AccessKeyId"`
	SecretAccessKey string `xml:"SecretAccessKey"`
	SessionToken    string `xml:"SessionToken"`
	Expiration      string `xml:"Expiration"`
}

type assumedRoleUser struct {
	ID  string `xml:"AssumedRoleId"`
	Arn string `xml:"Arn"`
}

type callerIdentityResponse struct {
	XMLName   xml.Name             `xml:"GetCallerIdentityResponse"`
	NS        string               `xml:"xmlns,attr"`
	Result    callerIdentityResult `xml:"GetCallerIdentityResult"`
	RequestID string               `xml:"ResponseMetadata>RequestId"`
}

type callerIdentityResult struct {
	Arn     string `xml:"Arn"`
	UserID  string `xml:"UserId"`
	Account string `xml:"Account"`
}

type stsErrorResponse struct {
	XMLName   xml.Name     `xml:"ErrorResponse"`
	NS        string       `xml:"xmlns,attr"`
	Error     stsErrorBody `xml:"Error"`
	RequestID string       `xml:"RequestId"`
}

type stsErrorBody struct {
	Type    string `xml:"Type"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}
