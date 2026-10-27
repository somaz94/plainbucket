package s3api

import (
	"net/http"
	"net/url"
)

// operation names the S3 or STS operation a request asks for, following the dispatch in serve,
// for the metrics and the access log. It does not depend on the request being authorized,
// so a refused PutObject still counts as PutObject.
func operation(r *http.Request, bucket, key string) string {
	q := r.URL.Query()
	if r.Method == http.MethodOptions {
		return "Preflight"
	}
	probe := &request{r: r, key: key}
	for k := range q {
		if unsupported[k] && !readableSubresource(k, probe) {
			return "NotImplemented"
		}
	}
	if v, ok := q["versionId"]; ok && (len(v) != 1 || v[0] != "null") {
		return "NotImplemented"
	}
	switch {
	case bucket == "":
		switch r.Method {
		case http.MethodGet:
			return "ListBuckets"
		case http.MethodPost:
			return "AssumeRole"
		}
	case isPostUpload(r, key, q):
		return "PostObject"
	case key == "":
		return bucketOperation(r.Method, q)
	default:
		return objectOperation(r, q)
	}
	return "Unknown"
}

func bucketOperation(method string, q url.Values) string {
	switch method {
	case http.MethodGet:
		switch {
		case q.Has("location"):
			return "GetBucketLocation"
		case q.Has("versioning"):
			return "GetBucketVersioning"
		case q.Has("uploads"):
			return "ListMultipartUploads"
		case q.Has("versions"):
			return "ListObjectVersions"
		case q.Get("list-type") == "2":
			return "ListObjectsV2"
		case !q.Has("list-type"):
			return "ListObjects"
		}
	case http.MethodHead:
		return "HeadBucket"
	case http.MethodPut:
		if q.Has("versioning") {
			return "NotImplemented"
		}
		return "CreateBucket"
	case http.MethodDelete:
		return "DeleteBucket"
	case http.MethodPost:
		if q.Has("delete") {
			return "DeleteObjects"
		}
	}
	return "Unknown"
}

func objectOperation(r *http.Request, q url.Values) string {
	if q.Has("versioning") {
		return "NotImplemented"
	}
	copySource := r.Header.Get("X-Amz-Copy-Source") != ""
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		switch {
		case q.Has("tagging"):
			return "GetObjectTagging"
		case q.Has("attributes"):
			return "GetObjectAttributes"
		case q.Has("uploadId") && r.Method == http.MethodGet:
			return "ListParts"
		case r.Method == http.MethodHead:
			return "HeadObject"
		}
		return "GetObject"
	case http.MethodPut:
		switch {
		case q.Has("tagging"):
			return "PutObjectTagging"
		case (q.Has("uploadId") || q.Has("partNumber")) && copySource:
			return "UploadPartCopy"
		case q.Has("uploadId") || q.Has("partNumber"):
			return "UploadPart"
		case copySource:
			return "CopyObject"
		}
		return "PutObject"
	case http.MethodDelete:
		switch {
		case q.Has("tagging"):
			return "DeleteObjectTagging"
		case q.Has("uploadId"):
			return "AbortMultipartUpload"
		}
		return "DeleteObject"
	case http.MethodPost:
		switch {
		case q.Has("uploads"):
			return "CreateMultipartUpload"
		case q.Has("uploadId"):
			return "CompleteMultipartUpload"
		}
	}
	return "Unknown"
}
