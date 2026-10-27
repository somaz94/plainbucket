package s3api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"syscall"

	"github.com/somaz94/plainbucket/internal/s3err"
	"github.com/somaz94/plainbucket/internal/storage"
)

var storageErrors = map[error]func() *s3err.Error{
	storage.ErrNoSuchBucket:       s3err.NoSuchBucket,
	storage.ErrNoSuchKey:          s3err.NoSuchKey,
	storage.ErrBucketExists:       s3err.BucketAlreadyOwnedByYou,
	storage.ErrBucketNotEmpty:     s3err.BucketNotEmpty,
	storage.ErrInvalidBucketName:  s3err.InvalidBucketName,
	storage.ErrInvalidKey:         s3err.InvalidKey,
	storage.ErrKeyTooLong:         s3err.KeyTooLong,
	storage.ErrConflict:           s3err.KeyConflict,
	storage.ErrShortBody:          s3err.IncompleteBody,
	storage.ErrPreconditionFailed: s3err.PreconditionFailed,
	storage.ErrNoSuchUpload:       s3err.NoSuchUpload,
	storage.ErrNoSuchTrash:        s3err.NoSuchKey,
	storage.ErrInvalidPart:        s3err.InvalidPart,
	storage.ErrInvalidChecksum: func() *s3err.Error {
		return s3err.InvalidRequest("The checksum does not match the algorithm or type of the upload.")
	},
	storage.ErrBadDigest: func() *s3err.Error {
		return s3err.BadDigest("The checksum you specified did not match the calculated checksum.")
	},
	storage.ErrInvalidPartOrder:    s3err.InvalidPartOrder,
	storage.ErrEntityTooSmall:      s3err.EntityTooSmall,
	storage.ErrEntityTooLarge:      s3err.EntityTooLarge,
	storage.ErrQuotaExceeded:       s3err.QuotaExceeded,
	storage.ErrInsufficientStorage: s3err.InsufficientStorage,
	syscall.ENOSPC:                 s3err.InsufficientStorage,
	syscall.EDQUOT:                 s3err.InsufficientStorage,
	storage.ErrMarkerNotEmpty: func() *s3err.Error {
		return s3err.InvalidArgument("An object key ending in / is a directory and must have an empty body.")
	},
	storage.ErrInvalidPartNumber: func() *s3err.Error {
		return s3err.InvalidArgument("Part number must be an integer between 1 and 10000, inclusive.")
	},
}

// toS3 maps err to its S3 error, or nil when it is not a known client error.
func toS3(err error) *s3err.Error {
	var e *s3err.Error
	if errors.As(err, &e) {
		return e
	}
	var re *storage.RangeError
	if errors.As(err, &re) {
		return s3err.InvalidArgument(fmt.Sprintf("Range specified is not valid for source object of size: %d", re.Size))
	}
	for target, mk := range storageErrors {
		if errors.Is(err, target) {
			return mk()
		}
	}
	return nil
}

// statusClientClosed records a request the client left before any response, as nginx does.
const statusClientClosed = 499

func clientGone(req *request, err error) bool {
	return errors.Is(err, context.Canceled) && req.r.Context().Err() != nil
}

func (h *Handler) fail(req *request, err error) {
	if clientGone(req, err) {
		h.log.Debug("request canceled by the client", "method", req.r.Method, "path", req.r.URL.Path, "request_id", req.id)
		return
	}
	e := toS3(err)
	if e == nil {
		h.log.Error("request failed", "method", req.r.Method, "path", req.r.URL.Path, "request_id", req.id, "err", err)
		e = s3err.Internal()
	} else if e.Status == http.StatusForbidden {
		// Debug, not warn: unsigned probes would flood the log.
		h.log.Debug("request denied", "method", req.r.Method, "path", req.r.URL.Path, "request_id", req.id, "code", e.Code, "reason", e.Message)
	} else if e.Status == http.StatusInsufficientStorage {
		h.log.Warn("write refused: not enough disk space", "method", req.r.Method, "path", req.r.URL.Path, "request_id", req.id, "err", err)
	}
	e.Write(req.w, req.r, req.id)
}
