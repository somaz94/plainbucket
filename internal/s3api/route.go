package s3api

import (
	"net/http"

	"github.com/somaz94/plainbucket/internal/s3err"
)

// Subresources this server does not implement; requests naming one get 501 (bar those
// readableSubresource answers) instead of being mistaken for a plain operation.
var unsupported = map[string]bool{
	"accelerate": true, "acl": true, "analytics": true, "attributes": true, "cors": true,
	"encryption": true, "intelligent-tiering": true, "inventory": true, "legal-hold": true,
	"lifecycle": true, "logging": true, "metrics": true, "notification": true,
	"object-lock": true, "ownershipControls": true, "policy": true, "policyStatus": true,
	"publicAccessBlock": true, "replication": true, "requestPayment": true, "restore": true,
	"retention": true, "select": true, "session": true, "tagging": true, "torrent": true,
	"versions": true, "website": true,
}

// readableSubresource reports the requests for an otherwise unsupported subresource
// that are answered: object and bucket tags, an object's attributes, and a bucket's
// versions, which are its objects as the "null" version.
func readableSubresource(k string, req *request) bool {
	m := req.r.Method
	switch k {
	case "tagging":
		return req.bucket != "" && (m == http.MethodGet || m == http.MethodPut || m == http.MethodDelete)
	case "attributes":
		return req.key != "" && m == http.MethodGet
	case "versions":
		return req.key == "" && m == http.MethodGet
	}
	return false
}

func (h *Handler) serve(req *request) error {
	req.q = req.r.URL.Query()
	if isSTS(req.r, req.bucket) {
		h.serveSTS(req)
		return nil
	}
	if req.bucket != "" && isPostUpload(req.r, req.key, req.q) {
		return h.postObject(req)
	}
	if anonymous(req.r, req.q) {
		h.websiteKey(req)
		if err := h.authorizeAnonymous(req); err != nil {
			return err
		}
		if err := h.throttle(req, h.anonLimiter(req), true); err != nil {
			return err
		}
	} else {
		if err := h.verifySigned(req); err != nil {
			return err
		}
		if err := req.authorize(); err != nil {
			return err
		}
		if err := h.throttle(req, h.keyLimiter(req), false); err != nil {
			return err
		}
	}
	for k := range req.q {
		if unsupported[k] && !readableSubresource(k, req) {
			return s3err.NotImplemented()
		}
	}
	if v, ok := req.q["versionId"]; ok && (len(v) != 1 || v[0] != "null") {
		return s3err.NotImplemented()
	}

	switch {
	case req.bucket == "":
		if req.r.Method != http.MethodGet {
			return s3err.MethodNotAllowed()
		}
		return h.listBuckets(req)
	case req.key == "":
		return h.bucketOp(req)
	case req.website:
		return h.websiteNotFound(req, h.objectOp(req))
	default:
		return h.objectOp(req)
	}
}

func (h *Handler) bucketOp(req *request) error {
	q := req.q
	switch req.r.Method {
	case http.MethodGet:
		switch {
		case q.Has("location"):
			return h.getBucketLocation(req)
		case q.Has("tagging"):
			return h.getBucketTagging(req)
		case q.Has("versioning"):
			return h.getBucketVersioning(req)
		case q.Has("uploads"):
			return h.listMultipartUploads(req)
		case q.Has("versions"):
			return h.listObjectVersions(req)
		case q.Get("list-type") == "2":
			return h.listObjects(req, true)
		case q.Has("list-type"):
			return s3err.InvalidArgument("list-type must be 2.")
		default:
			return h.listObjects(req, false)
		}
	case http.MethodHead:
		return h.headBucket(req)
	case http.MethodPut:
		if q.Has("versioning") {
			return s3err.NotImplemented()
		}
		if q.Has("tagging") {
			return h.putBucketTagging(req)
		}
		return h.createBucket(req)
	case http.MethodDelete:
		if q.Has("tagging") {
			return h.deleteBucketTagging(req)
		}
		return h.deleteBucket(req)
	case http.MethodPost:
		if q.Has("delete") {
			return h.deleteObjects(req)
		}
	}
	return s3err.MethodNotAllowed()
}

func (h *Handler) objectOp(req *request) error {
	q := req.q
	if q.Has("versioning") {
		return s3err.NotImplemented()
	}
	switch req.r.Method {
	case http.MethodGet, http.MethodHead:
		if q.Has("tagging") {
			return h.getObjectTagging(req)
		}
		if q.Has("attributes") {
			return h.getObjectAttributes(req)
		}
		if q.Has("uploadId") && req.r.Method == http.MethodGet {
			return h.listParts(req)
		}
		return h.getObject(req)
	case http.MethodPut:
		if q.Has("tagging") {
			return h.putObjectTagging(req)
		}
		if q.Has("uploadId") || q.Has("partNumber") {
			return h.uploadPart(req)
		}
		if req.r.Header.Get("X-Amz-Copy-Source") != "" {
			return h.copyObject(req)
		}
		return h.putObject(req)
	case http.MethodDelete:
		if q.Has("tagging") {
			return h.deleteObjectTagging(req)
		}
		if q.Has("uploadId") {
			return h.abortMultipartUpload(req)
		}
		return h.deleteObject(req)
	case http.MethodPost:
		switch {
		case q.Has("uploads"):
			return h.createMultipartUpload(req)
		case q.Has("uploadId"):
			return h.completeMultipartUpload(req)
		}
	}
	return s3err.MethodNotAllowed()
}
