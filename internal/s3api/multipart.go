package s3api

import (
	"encoding/xml"
	"net/http"
	"strconv"

	"github.com/somaz94/plainbucket/internal/s3err"
	"github.com/somaz94/plainbucket/internal/sigv4"
	"github.com/somaz94/plainbucket/internal/storage"
)

const maxListParts = 1000

func (h *Handler) createMultipartUpload(req *request) error {
	id, err := h.store.CreateUpload(req.bucket, req.key, metadataFromHeaders(req.r.Header))
	if err != nil {
		return err
	}
	if alg := req.r.Header.Get("X-Amz-Checksum-Algorithm"); alg != "" {
		req.w.Header().Set("X-Amz-Checksum-Algorithm", alg)
	}
	writeXML(req.w, http.StatusOK, initiateMultipartUploadResult{NS: s3NS, Bucket: req.bucket, Key: req.key, UploadID: id})
	return nil
}

func (h *Handler) uploadPart(req *request) error {
	if req.r.Header.Get("X-Amz-Copy-Source") != "" {
		return s3err.NotImplemented()
	}
	n, err := strconv.Atoi(req.q.Get("partNumber"))
	if err != nil || !req.q.Has("uploadId") {
		return s3err.InvalidArgument("UploadPart requires partNumber and uploadId.")
	}
	p, err := newPayload(req, false)
	if err != nil {
		return err
	}
	if err := h.checkSize(p.size); err != nil {
		return err
	}
	part, err := h.store.PutPart(req.bucket, req.key, req.q.Get("uploadId"), n, p.reader, p.size, p.verify)
	if err != nil {
		return err
	}
	req.w.Header().Set("ETag", part.ETag)
	setChecksumHeaders(req.w.Header(), part.Checksums)
	req.w.WriteHeader(http.StatusOK)
	return nil
}

func (h *Handler) completeMultipartUpload(req *request) error {
	body, err := readXMLBody(req)
	if err != nil {
		return err
	}
	var doc completeMultipartUpload
	if xml.Unmarshal(body, &doc) != nil {
		return s3err.MalformedXML()
	}
	parts := make([]storage.CompletePart, len(doc.Parts))
	for i, p := range doc.Parts {
		parts[i] = storage.CompletePart{Number: p.PartNumber, ETag: p.ETag}
	}
	cond, err := writeConditions(req.r.Header)
	if err != nil {
		return err
	}
	info, err := h.store.CompleteUpload(req.r.Context(), req.bucket, req.key, req.q.Get("uploadId"), parts, cond, h.opts.MaxObjectSize)
	if err != nil {
		return err
	}
	loc := "http://" + req.r.Host + "/" + req.bucket + "/" + sigv4.URIEncodePath(req.key)
	if req.r.TLS != nil {
		loc = "https" + loc[4:]
	}
	writeXML(req.w, http.StatusOK, completeMultipartUploadResult{NS: s3NS, Location: loc, Bucket: req.bucket, Key: req.key, ETag: info.ETag})
	return nil
}

func (h *Handler) abortMultipartUpload(req *request) error {
	if err := h.store.AbortUpload(req.bucket, req.key, req.q.Get("uploadId")); err != nil {
		return err
	}
	req.w.WriteHeader(http.StatusNoContent)
	return nil
}

func intParam(v string, def, max int) (int, error) {
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, s3err.InvalidArgument("Expected a non-negative integer.")
	}
	return min(n, max), nil
}

func (h *Handler) listParts(req *request) error {
	marker, err := intParam(req.q.Get("part-number-marker"), 0, 10000)
	if err != nil {
		return err
	}
	maxParts, err := intParam(req.q.Get("max-parts"), maxListParts, maxListParts)
	if err != nil {
		return err
	}
	id := req.q.Get("uploadId")
	parts, truncated, err := h.store.ListParts(req.bucket, req.key, id, marker, maxParts)
	if err != nil {
		return err
	}
	res := listPartsResult{
		NS: s3NS, Bucket: req.bucket, Key: req.key, UploadID: id, Initiator: serverOwner, Owner: serverOwner,
		StorageClass: "STANDARD", PartNumberMarker: marker, MaxParts: maxParts, IsTruncated: truncated,
	}
	for _, p := range parts {
		item := partItem{PartNumber: p.Number, LastModified: isoTime(p.ModTime), ETag: p.ETag, Size: p.Size}
		partChecksums(&item, p.Checksums)
		res.Parts = append(res.Parts, item)
		res.NextPartNumberMarker = p.Number
	}
	writeXML(req.w, http.StatusOK, res)
	return nil
}

func (h *Handler) listMultipartUploads(req *request) error {
	if req.q.Get("delimiter") != "" {
		return s3err.NotImplemented()
	}
	maxUploads, err := intParam(req.q.Get("max-uploads"), maxListParts, maxListParts)
	if err != nil {
		return err
	}
	prefix, keyMarker, idMarker := req.q.Get("prefix"), req.q.Get("key-marker"), req.q.Get("upload-id-marker")
	ups, truncated, err := h.store.ListUploads(req.bucket, prefix, keyMarker, idMarker, maxUploads)
	if err != nil {
		return err
	}
	res := listMultipartUploadsResult{
		NS: s3NS, Bucket: req.bucket, KeyMarker: keyMarker, UploadIDMarker: idMarker, Prefix: prefix,
		MaxUploads: maxUploads, IsTruncated: truncated,
	}
	for _, u := range ups {
		res.Uploads = append(res.Uploads, uploadItem{
			Key: u.Key, UploadID: u.UploadID, Initiator: serverOwner, Owner: serverOwner,
			StorageClass: "STANDARD", Initiated: isoTime(u.Initiated),
		})
	}
	if truncated && len(ups) > 0 {
		res.NextKeyMarker, res.NextUploadIDMarker = ups[len(ups)-1].Key, ups[len(ups)-1].UploadID
	}
	writeXML(req.w, http.StatusOK, res)
	return nil
}
