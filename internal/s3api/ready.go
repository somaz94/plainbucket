package s3api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/somaz94/plainbucket/internal/storage"
)

// readyTimeout bounds the storage probe, below the chart's readiness probe timeout.
const readyTimeout = 2 * time.Second

type readiness struct {
	Ready    bool `json:"ready"`
	ReadOnly bool `json:"read_only"`
	LowSpace bool `json:"low_space"`
}

// serveReady answers /readyz. Only a failed storage probe makes the server unready: in
// read-only mode or below the free-space minimum it still serves reads, and the chart
// runs a single pod, so leaving the Service would cut those off too.
func (h *Handler) serveReady(w http.ResponseWriter) {
	res, status := readiness{Ready: true, ReadOnly: h.readOnly.Load()}, http.StatusOK
	free, err := h.store.Probe(readyTimeout)
	h.rootMoved.Store(errors.Is(err, storage.ErrRootMoved))
	if err != nil {
		res.Ready, status = false, http.StatusServiceUnavailable
		if h.unready.CompareAndSwap(false, true) {
			h.log.Warn("not ready: the storage root cannot be read", "err", err)
		}
	} else {
		if h.unready.CompareAndSwap(true, false) {
			h.log.Info("ready again: the storage root answers")
		}
		floor := h.store.MinFree()
		res.LowSpace = floor > 0 && free >= 0 && free < floor
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(res)
}

// serveHealth answers /healthz. It fails only once /readyz found the root's path leading
// elsewhere, which a restart fixes; it never waits on the storage, so a hung mount does
// not make the kubelet restart the pod over and over.
func (h *Handler) serveHealth(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if h.rootMoved.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("storage root moved: restart to reopen it"))
		return
	}
	_, _ = w.Write([]byte("ok"))
}
