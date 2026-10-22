package s3api

import (
	"bytes"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/somaz94/plainbucket/internal/s3err"
	"github.com/somaz94/plainbucket/internal/storage"
)

const defaultContentType = "binary/octet-stream"

// storedHeaders are kept with the object and returned on GET and HEAD.
var storedHeaders = []string{"Cache-Control", "Content-Disposition", "Content-Encoding", "Content-Language", "Expires"}

// responseOverrides maps GET query parameters to the headers they replace.
var responseOverrides = map[string]string{
	"response-cache-control":       "Cache-Control",
	"response-content-disposition": "Content-Disposition",
	"response-content-encoding":    "Content-Encoding",
	"response-content-language":    "Content-Language",
	"response-content-type":        "Content-Type",
	"response-expires":             "Expires",
}

// metadataFromHeaders collects the content type, x-amz-meta-* and stored headers of a write.
func metadataFromHeaders(h http.Header) storage.Metadata {
	md := storage.Metadata{ContentType: h.Get("Content-Type")}
	for k := range h {
		if name, ok := strings.CutPrefix(strings.ToLower(k), "x-amz-meta-"); ok && name != "" {
			if md.UserMeta == nil {
				md.UserMeta = map[string]string{}
			}
			md.UserMeta[name] = h.Get(k)
		}
	}
	for _, name := range storedHeaders {
		v := h.Get(name)
		if name == "Content-Encoding" {
			v = stripAWSChunked(v)
		}
		if v != "" {
			if md.Headers == nil {
				md.Headers = map[string]string{}
			}
			md.Headers[name] = v
		}
	}
	return md
}

// stripAWSChunked drops the transfer-only aws-chunked token from Content-Encoding.
func stripAWSChunked(v string) string {
	var keep []string
	for part := range strings.SplitSeq(v, ",") {
		if p := strings.TrimSpace(part); p != "" && !strings.EqualFold(p, "aws-chunked") {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, ",")
}

func writeConditions(h http.Header) (storage.Conditions, error) {
	c := storage.Conditions{IfMatch: h.Get("If-Match"), IfNoneMatch: h.Get("If-None-Match")}
	if c.IfNoneMatch != "" && c.IfNoneMatch != "*" {
		return c, s3err.NotImplemented()
	}
	return c, nil
}

func (h *Handler) checkSize(n int64) error {
	if h.opts.MaxObjectSize > 0 && n > h.opts.MaxObjectSize {
		return s3err.EntityTooLarge()
	}
	return nil
}

func (h *Handler) putObject(req *request) error {
	p, err := newPayload(req, h.opts.BodyTimeout, true)
	if err != nil {
		return err
	}
	if err := h.checkSize(p.size); err != nil {
		return err
	}
	cond, err := writeConditions(req.r.Header)
	if err != nil {
		return err
	}
	info, err := h.store.PutObject(req.bucket, req.key, p.reader, p.size, storage.PutOptions{
		Metadata: metadataFromHeaders(req.r.Header),
		Cond:     cond,
		Verify:   p.verify,
	})
	if err != nil {
		return err
	}
	h.metrics.receivedBytes.Add(p.size)
	req.w.Header().Set("ETag", info.ETag)
	setChecksumHeaders(req.w.Header(), info.Checksums)
	req.w.WriteHeader(http.StatusOK)
	return nil
}

func (h *Handler) getObject(req *request) error {
	f, info, err := h.store.GetObject(req.bucket, req.key)
	if err != nil {
		return err
	}
	var content io.ReadSeeker = bytes.NewReader(nil)
	if f != nil {
		defer func() { _ = f.Close() }()
		content = f
	}

	hdr := req.w.Header()
	ct := info.ContentType
	if ct == "" {
		ct = defaultContentType
	}
	hdr.Set("Content-Type", ct)
	hdr.Set("ETag", info.ETag)
	hdr.Set("Accept-Ranges", "bytes")
	for k, v := range info.UserMeta {
		hdr.Set("X-Amz-Meta-"+k, v)
	}
	for k, v := range info.Headers {
		hdr.Set(k, v)
	}
	if strings.EqualFold(req.r.Header.Get("X-Amz-Checksum-Mode"), "ENABLED") &&
		req.r.Header.Get("Range") == "" && len(info.Checksums) > 0 {
		setChecksumHeaders(hdr, info.Checksums)
		hdr.Set("X-Amz-Checksum-Type", "FULL_OBJECT")
	}
	if req.r.Method == http.MethodGet {
		for param, header := range responseOverrides {
			if v := req.q.Get(param); v != "" {
				hdr.Set(header, v)
			}
		}
	}
	http.ServeContent(req.w, req.r, "", info.ModTime, content)
	return nil
}

func (h *Handler) deleteObject(req *request) error {
	if err := h.store.DeleteObject(req.bucket, req.key); err != nil {
		return err
	}
	req.w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) deleteObjects(req *request) error {
	body, err := readXMLBody(req, h.opts.BodyTimeout)
	if err != nil {
		return err
	}
	var del deleteRequest
	if xml.Unmarshal(body, &del) != nil || len(del.Objects) == 0 || len(del.Objects) > maxListKeys {
		return s3err.MalformedXML()
	}
	if err := h.store.HeadBucket(req.bucket); err != nil {
		return err
	}
	res := deleteResult{NS: s3NS}
	for _, o := range del.Objects {
		var err error
		if o.VersionID != "" && o.VersionID != "null" {
			err = s3err.NotImplemented()
		} else {
			err = h.store.DeleteObject(req.bucket, o.Key)
		}
		if err != nil {
			e := toS3(err)
			if e == nil {
				h.log.Error("delete failed", "bucket", req.bucket, "key", o.Key, "err", err)
				e = s3err.Internal()
			}
			res.Errors = append(res.Errors, deleteError{Key: o.Key, Code: e.Code, Message: e.Message})
			continue
		}
		if !del.Quiet {
			res.Deleted = append(res.Deleted, deletedItem{Key: o.Key})
		}
	}
	writeXML(req.w, http.StatusOK, res)
	return nil
}

func (h *Handler) copyObject(req *request) error {
	srcBucket, srcKey, err := parseCopySource(req.r.Header.Get("X-Amz-Copy-Source"))
	if err != nil {
		return err
	}
	if !req.mayRead(srcBucket) {
		return s3err.AccessDenied("This access key may not access the source bucket.")
	}
	opts := storage.CopyOptions{
		SrcIfMatch:     req.r.Header.Get("X-Amz-Copy-Source-If-Match"),
		SrcIfNoneMatch: req.r.Header.Get("X-Amz-Copy-Source-If-None-Match"),
	}
	switch strings.ToUpper(req.r.Header.Get("X-Amz-Metadata-Directive")) {
	case "", "COPY":
		if srcBucket == req.bucket && srcKey == req.key {
			return s3err.InvalidRequest("This copy request is illegal because it is trying to copy an object to itself without changing the object's metadata.")
		}
	case "REPLACE":
		opts.ReplaceMetadata = true
		opts.Metadata = metadataFromHeaders(req.r.Header)
	default:
		return s3err.InvalidArgument("Unknown metadata directive.")
	}
	if opts.Cond, err = writeConditions(req.r.Header); err != nil {
		return err
	}
	if h.opts.MaxObjectSize > 0 {
		src, err := h.store.HeadObject(srcBucket, srcKey)
		if err != nil {
			return err
		}
		if err := h.checkSize(src.Size); err != nil {
			return err
		}
	}
	info, err := h.store.CopyObject(req.r.Context(), srcBucket, srcKey, req.bucket, req.key, opts)
	if err != nil {
		return err
	}
	writeXML(req.w, http.StatusOK, copyObjectResult{NS: s3NS, LastModified: isoTime(info.ModTime), ETag: info.ETag})
	return nil
}

// parseCopySource splits x-amz-copy-source, "[/]bucket/key[?versionId=null]", URL-encoded.
func parseCopySource(v string) (string, string, error) {
	path, query, _ := strings.Cut(v, "?")
	if query != "" {
		q, err := url.ParseQuery(query)
		if err != nil || len(q) != 1 || q.Get("versionId") != "null" {
			return "", "", s3err.NotImplemented()
		}
	}
	decoded, err := url.PathUnescape(path)
	if err != nil {
		return "", "", s3err.InvalidArgument("Invalid copy source encoding.")
	}
	bucket, key, ok := strings.Cut(strings.TrimPrefix(decoded, "/"), "/")
	if !ok || bucket == "" || key == "" {
		return "", "", s3err.InvalidArgument("Copy Source must mention the source bucket and key: sourcebucket/sourcekey.")
	}
	return bucket, key, nil
}

// partChecksums fills a part listing entry from the part's stored checksums.
func partChecksums(item *partItem, sums map[string]string) {
	for alg, v := range sums {
		switch alg {
		case "CRC32":
			item.ChecksumCRC32 = v
		case "CRC32C":
			item.ChecksumCRC32C = v
		case "CRC64NVME":
			item.ChecksumCRC64NVME = v
		case "SHA1":
			item.ChecksumSHA1 = v
		case "SHA256":
			item.ChecksumSHA256 = v
		}
	}
}

// getObjectTagging reports an empty tag set: tags are not stored.
func (h *Handler) getObjectTagging(req *request) error {
	if _, err := h.store.HeadObject(req.bucket, req.key); err != nil {
		return err
	}
	writeXML(req.w, http.StatusOK, tagging{NS: s3NS})
	return nil
}
