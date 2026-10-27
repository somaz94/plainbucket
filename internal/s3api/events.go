package s3api

import (
	"net"
	"time"

	"github.com/somaz94/plainbucket/internal/notify"
	"github.com/somaz94/plainbucket/internal/storage"
)

// Replicator copies committed changes elsewhere; Changed must not block.
type Replicator interface {
	Changed(bucket, key string)
	Stats() (copied, deleted, failed, dropped int64, queued int)
	LastCompared() map[string]time.Time // by "<bucket>/<rule id>"
}

func (h *Handler) replicate(bucket, key string) {
	if h.opts.Replicator != nil {
		h.opts.Replicator.Changed(bucket, key)
	}
}

// publish reports the change to key, made by req, to the replicator and, as event
// name, to the bucket's webhooks.
func (h *Handler) publish(req *request, name, key string, info storage.ObjectInfo) {
	h.replicate(req.bucket, key)
	if h.opts.Notifier == nil {
		return
	}
	e := notify.Event{Name: name, Bucket: req.bucket, Key: key, Size: info.Size, ETag: info.ETag, RequestID: req.id, Time: h.now()}
	if req.cred != nil {
		e.Principal = req.cred.AccessKey
	}
	if host, _, err := net.SplitHostPort(req.r.RemoteAddr); err == nil {
		e.SourceIP = host
	}
	h.opts.Notifier.Publish(e)
}
