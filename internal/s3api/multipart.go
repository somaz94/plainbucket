package s3api

import (
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"

	"github.com/somaz94/plainbucket/internal/checksum"
	"github.com/somaz94/plainbucket/internal/s3err"
	"github.com/somaz94/plainbucket/internal/storage"
)

const (
	maxListParts  = 1000
	maxPartNumber = 10000
)

func (h *Handler) createMultipartUpload(req *request) error {
	ck := storage.MultipartChecksum{
		Algorithm: strings.ToUpper(req.r.Header.Get("X-Amz-Checksum-Algorithm")),
		Type:      strings.ToUpper(req.r.Header.Get("X-Amz-Checksum-Type")),
	}
	if ck.Algorithm == "" && ck.Type != "" {
		return s3err.InvalidRequest("x-amz-checksum-type requires x-amz-checksum-algorithm.")
	}
	if ck.Algorithm != "" {
		if _, ok := checksum.New(ck.Algorithm); !ok {
			return s3err.InvalidRequest("Unsupported checksum algorithm " + ck.Algorithm + ".")
		}
		if ck.Type == "" {
			ck.Type = storage.DefaultChecksumType(ck.Algorithm)
		}
		if !storage.ValidChecksumType(ck.Algorithm, ck.Type) {
			return s3err.InvalidRequest("The " + ck.Type + " checksum type cannot be used with " + ck.Algorithm + ".")
		}
	}
	id, err := h.store.CreateUpload(req.bucket, req.key, metadataFromHeaders(req.r.Header), ck)
	if err != nil {
		return err
	}
	if ck.Algorithm != "" {
		req.w.Header().Set("X-Amz-Checksum-Algorithm", ck.Algorithm)
		req.w.Header().Set("X-Amz-Checksum-Type", ck.Type)
	}
	writeXML(req.w, http.StatusOK, initiateMultipartUploadResult{NS: s3NS, Bucket: req.bucket, Key: req.key, UploadID: id})
	return nil
}

func (h *Handler) uploadPart(req *request) error {
	n, err := strconv.Atoi(req.q.Get("partNumber"))
	if err != nil || !req.q.Has("uploadId") {
		return s3err.InvalidArgument("UploadPart requires partNumber and uploadId.")
	}
	if req.r.Header.Get("X-Amz-Copy-Source") != "" {
		return h.uploadPartCopy(req, n)
	}
	p, err := newPayload(req, h.opts.BodyTimeout, false)
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
	h.metrics.receivedBytes.Add(p.size)
	req.w.Header().Set("ETag", part.ETag)
	setChecksumHeaders(req.w.Header(), part.Checksums)
	req.w.WriteHeader(http.StatusOK)
	return nil
}

func (h *Handler) uploadPartCopy(req *request, n int) error {
	srcBucket, srcKey, err := parseCopySource(req.r.Header.Get("X-Amz-Copy-Source"))
	if err != nil {
		return err
	}
	if !req.mayRead(srcBucket) {
		return s3err.AccessDenied("This access key may not access the source bucket.")
	}
	rng, err := parseCopyRange(req.r.Header.Get("X-Amz-Copy-Source-Range"))
	if err != nil {
		return err
	}
	if rng != nil {
		if err := h.checkSize(rng.End - rng.Start + 1); err != nil {
			return err
		}
	} else if h.opts.MaxObjectSize > 0 {
		src, err := h.store.HeadObject(srcBucket, srcKey)
		if err != nil {
			return err
		}
		if err := h.checkSize(src.Size); err != nil {
			return err
		}
	}
	opts := storage.CopyOptions{
		SrcIfMatch:     req.r.Header.Get("X-Amz-Copy-Source-If-Match"),
		SrcIfNoneMatch: req.r.Header.Get("X-Amz-Copy-Source-If-None-Match"),
	}
	part, err := h.store.PutPartCopy(req.r.Context(), srcBucket, srcKey, rng, req.bucket, req.key, req.q.Get("uploadId"), n, opts)
	if err != nil {
		return err
	}
	writeXML(req.w, http.StatusOK, copyPartResult{NS: s3NS, LastModified: isoTime(part.ModTime), ETag: part.ETag})
	return nil
}

// parseCopyRange parses x-amz-copy-source-range, which S3 accepts only as bytes=first-last.
func parseCopyRange(v string) (*storage.ByteRange, error) {
	if v == "" {
		return nil, nil
	}
	bad := s3err.InvalidArgument("The x-amz-copy-source-range value must be of the form bytes=first-last where first and last are the zero-based offsets of the first and last bytes to copy")
	spec, ok := strings.CutPrefix(v, "bytes=")
	if !ok {
		return nil, bad
	}
	a, b, ok := strings.Cut(spec, "-")
	start, err1 := strconv.ParseInt(a, 10, 64)
	end, err2 := strconv.ParseInt(b, 10, 64)
	if !ok || err1 != nil || err2 != nil || start < 0 || end < start {
		return nil, bad
	}
	return &storage.ByteRange{Start: start, End: end}, nil
}

func (h *Handler) completeMultipartUpload(req *request) error {
	// Here x-amz-checksum-* declare the object's checksum, not the document's.
	body, err := readXMLBody(req, h.opts.BodyTimeout, false)
	if err != nil {
		return err
	}
	var doc completeMultipartUpload
	if xml.Unmarshal(body, &doc) != nil {
		return s3err.MalformedXML()
	}
	ck := storage.CompleteChecksums{Object: map[string]string{}, Parts: map[int]map[string]string{}}
	for name := range req.r.Header {
		if alg, ok := checksum.FromHeader(name); ok {
			ck.Object[alg] = req.r.Header.Get(name)
		}
	}
	parts := make([]storage.CompletePart, len(doc.Parts))
	for i, p := range doc.Parts {
		parts[i] = storage.CompletePart{Number: p.PartNumber, ETag: p.ETag}
		ck.Parts[p.PartNumber] = p.values()
	}
	cond := writeConditions(req.r.Header)
	info, err := h.store.CompleteUpload(req.r.Context(), req.bucket, req.key, req.q.Get("uploadId"), parts, cond, h.opts.MaxObjectSize, ck)
	if err != nil {
		return err
	}
	res := completeMultipartUploadResult{NS: s3NS, Location: req.url(req.key), Bucket: req.bucket, Key: req.key, ETag: info.ETag}
	if len(info.Checksums) > 0 {
		res.checksumFields, res.ChecksumType = newChecksumFields(info.Checksums), info.ChecksumType
	}
	writeXML(req.w, http.StatusOK, res)
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
		item.checksumFields = newChecksumFields(p.Checksums)
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
	enc, err := encodingType(req.q)
	if err != nil {
		return err
	}
	prefix, keyMarker, idMarker := req.q.Get("prefix"), req.q.Get("key-marker"), req.q.Get("upload-id-marker")
	ups, truncated, err := h.store.ListUploads(req.bucket, prefix, keyMarker, idMarker, maxUploads)
	if err != nil {
		return err
	}
	res := listMultipartUploadsResult{
		NS: s3NS, Bucket: req.bucket, KeyMarker: enc(keyMarker), UploadIDMarker: idMarker, Prefix: enc(prefix),
		MaxUploads: maxUploads, IsTruncated: truncated, EncodingType: req.q.Get("encoding-type"),
	}
	for _, u := range ups {
		res.Uploads = append(res.Uploads, uploadItem{
			Key: enc(u.Key), UploadID: u.UploadID, Initiator: serverOwner, Owner: serverOwner,
			StorageClass: "STANDARD", Initiated: isoTime(u.Initiated),
		})
	}
	if truncated && len(ups) > 0 {
		res.NextKeyMarker, res.NextUploadIDMarker = enc(ups[len(ups)-1].Key), ups[len(ups)-1].UploadID
	}
	writeXML(req.w, http.StatusOK, res)
	return nil
}
