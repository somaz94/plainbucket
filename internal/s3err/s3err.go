// Package s3err defines S3 error responses.
package s3err

import (
	"encoding/xml"
	"net/http"
)

// Error is an S3 error with its HTTP status.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func newErr(status int, code, msg string) *Error {
	return &Error{Status: status, Code: code, Message: msg}
}

func AccessDenied(msg string) *Error { return newErr(http.StatusForbidden, "AccessDenied", msg) }

func AuthHeaderMalformed(msg string) *Error {
	return newErr(http.StatusBadRequest, "AuthorizationHeaderMalformed", msg)
}

func AuthQueryMalformed(msg string) *Error {
	return newErr(http.StatusBadRequest, "AuthorizationQueryParametersError", msg)
}

func InvalidAccessKeyID() *Error {
	return newErr(http.StatusForbidden, "InvalidAccessKeyId", "The AWS access key ID you provided does not exist in our records.")
}

func SignatureDoesNotMatch() *Error {
	return newErr(http.StatusForbidden, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided.")
}

func RequestTimeTooSkewed() *Error {
	return newErr(http.StatusForbidden, "RequestTimeTooSkewed", "The difference between the request time and the server's time is too large.")
}

func ContentSHA256Mismatch() *Error {
	return newErr(http.StatusBadRequest, "XAmzContentSHA256Mismatch", "The provided 'x-amz-content-sha256' header does not match what was computed.")
}

func BadDigest(msg string) *Error { return newErr(http.StatusBadRequest, "BadDigest", msg) }

func InvalidDigest() *Error {
	return newErr(http.StatusBadRequest, "InvalidDigest", "The Content-MD5 you specified is not valid.")
}

func InvalidArgument(msg string) *Error {
	return newErr(http.StatusBadRequest, "InvalidArgument", msg)
}

func InvalidRequest(msg string) *Error {
	return newErr(http.StatusBadRequest, "InvalidRequest", msg)
}

func InvalidTag(msg string) *Error {
	return newErr(http.StatusBadRequest, "InvalidTag", msg)
}

func MalformedXML() *Error {
	return newErr(http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.")
}

func IncompleteBody() *Error {
	return newErr(http.StatusBadRequest, "IncompleteBody", "You did not provide the number of bytes specified by the Content-Length HTTP header.")
}

func MissingContentLength() *Error {
	return newErr(http.StatusLengthRequired, "MissingContentLength", "You must provide the Content-Length HTTP header.")
}

func EntityTooLarge() *Error {
	return newErr(http.StatusBadRequest, "EntityTooLarge", "Your proposed upload exceeds the maximum allowed object size.")
}

func InvalidPolicyDocument(msg string) *Error {
	return newErr(http.StatusBadRequest, "InvalidPolicyDocument", msg)
}

func MalformedPOSTRequest() *Error {
	return newErr(http.StatusBadRequest, "MalformedPOSTRequest", "The body of your POST request is not well-formed multipart/form-data.")
}

func MaxPostPreDataLengthExceeded() *Error {
	return newErr(http.StatusBadRequest, "MaxPostPreDataLengthExceededError", "Your POST request fields preceding the upload file were too large.")
}

func EntityTooSmall() *Error {
	return newErr(http.StatusBadRequest, "EntityTooSmall", "Your proposed upload is smaller than the minimum allowed object size.")
}

func InvalidBucketName() *Error {
	return newErr(http.StatusBadRequest, "InvalidBucketName", "The specified bucket is not valid.")
}

func KeyTooLong() *Error {
	return newErr(http.StatusBadRequest, "KeyTooLongError", "Your key is too long.")
}

func InvalidKey() *Error {
	return newErr(http.StatusBadRequest, "InvalidArgument", "The object key cannot be stored as a file path.")
}

func KeyConflict() *Error {
	return newErr(http.StatusConflict, "KeyConflict", "The key collides with an existing object or prefix on the filesystem.")
}

func NoSuchBucket() *Error {
	return newErr(http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist.")
}

func NoSuchKey() *Error {
	return newErr(http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
}

func NoSuchUpload() *Error {
	return newErr(http.StatusNotFound, "NoSuchUpload", "The specified multipart upload does not exist.")
}

func BucketAlreadyOwnedByYou() *Error {
	return newErr(http.StatusConflict, "BucketAlreadyOwnedByYou", "Your previous request to create the named bucket succeeded and you already own it.")
}

func BucketNotEmpty() *Error {
	return newErr(http.StatusConflict, "BucketNotEmpty", "The bucket you tried to delete is not empty.")
}

func InvalidPart() *Error {
	return newErr(http.StatusBadRequest, "InvalidPart", "One or more of the specified parts could not be found or its ETag did not match.")
}

func InvalidRange() *Error {
	return newErr(http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "The requested range is not satisfiable")
}

func InvalidPartNumber() *Error {
	return newErr(http.StatusRequestedRangeNotSatisfiable, "InvalidPartNumber", "The requested partnumber is not satisfiable.")
}

func InvalidPartOrder() *Error {
	return newErr(http.StatusBadRequest, "InvalidPartOrder", "The list of parts was not in ascending order.")
}

func PreconditionFailed() *Error {
	return newErr(http.StatusPreconditionFailed, "PreconditionFailed", "At least one of the pre-conditions you specified did not hold.")
}

// QuotaExceeded is plainbucket's own: S3 has no bucket size limit.
func QuotaExceeded() *Error {
	return newErr(http.StatusForbidden, "QuotaExceeded", "The bucket has reached its configured size limit.")
}

// InsufficientStorage is plainbucket's own: the disk holding the root is full, or would
// be left with less free space than min-free-space.
func InsufficientStorage() *Error {
	return newErr(http.StatusInsufficientStorage, "InsufficientStorage", "There is not enough free disk space to store the data.")
}

func SlowDown() *Error {
	return newErr(http.StatusServiceUnavailable, "SlowDown", "Please reduce your request rate.")
}

func ServiceUnavailable(msg string) *Error {
	return newErr(http.StatusServiceUnavailable, "ServiceUnavailable", msg)
}

func NotImplemented() *Error {
	return newErr(http.StatusNotImplemented, "NotImplemented", "A header or query you provided implies functionality that is not implemented.")
}

func MethodNotAllowed() *Error {
	return newErr(http.StatusMethodNotAllowed, "MethodNotAllowed", "The specified method is not allowed against this resource.")
}

func Internal() *Error {
	return newErr(http.StatusInternalServerError, "InternalError", "We encountered an internal error. Please try again.")
}

type body struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId"`
}

// Write sends e as an XML error document; HEAD responses carry the status only.
func (e *Error) Write(w http.ResponseWriter, r *http.Request, requestID string) {
	if r.Method == http.MethodHead {
		w.WriteHeader(e.Status)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(e.Status)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(body{Code: e.Code, Message: e.Message, Resource: r.URL.Path, RequestID: requestID})
}
