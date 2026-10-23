package s3api

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

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
	return strings.Join(keep, ", ")
}

func writeConditions(h http.Header) storage.Conditions {
	return storage.Conditions{IfMatch: h.Get("If-Match"), IfNoneMatch: h.Get("If-None-Match")}
}

// capReader fails a body of unknown length once it passes the maximum object size.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	if c.left -= int64(n); c.left < 0 {
		return n, s3err.EntityTooLarge()
	}
	return n, err
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
	if p.size < 0 && h.opts.MaxObjectSize > 0 {
		p.reader = &capReader{r: p.reader, left: h.opts.MaxObjectSize}
	}
	md, err := writeMetadata(req.r.Header)
	if err != nil {
		return err
	}
	cond := writeConditions(req.r.Header)
	info, err := h.store.PutObject(req.bucket, req.key, p.reader, p.size, storage.PutOptions{
		Metadata: md,
		Cond:     cond,
		Verify:   p.verify,
	})
	if err != nil {
		return err
	}
	h.metrics.receivedBytes.Add(info.Size)
	req.w.Header().Set("ETag", info.ETag)
	setChecksumHeaders(req.w.Header(), info.Checksums)
	req.w.WriteHeader(http.StatusOK)
	return nil
}

func (h *Handler) getObject(req *request) error {
	part, err := partNumber(req)
	if err != nil {
		return err
	}
	f, info, err := h.store.GetObject(req.bucket, req.key)
	if err != nil {
		return err
	}
	if f != nil {
		defer func() { _ = f.Close() }()
	}
	// S3 serves the whole object for a Range it cannot parse, where ServeContent says 416.
	if h := req.r.Header.Get("Range"); h != "" && !strings.HasPrefix(h, "bytes=") {
		req.r.Header.Del("Range")
	}
	if err := checkRange(req.r.Header.Get("Range"), info.Size); err != nil {
		return err
	}
	emptyPart := false
	if part > 0 {
		if emptyPart, err = servePart(req, info, part); err != nil {
			return err
		}
	}
	var content io.ReadSeeker = bytes.NewReader(nil)
	if f != nil && !emptyPart {
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
	if req.auth != nil {
		// Tags need a key, as GetObjectTagging does; S3 shows the count only to those who may read them.
		setTagCount(hdr, info.Tags)
	}
	for k, v := range info.UserMeta {
		// Lowercase as S3 sends them: clients take the metadata key from the header name.
		hdr["x-amz-meta-"+k] = []string{v}
	}
	for k, v := range info.Headers {
		hdr.Set(k, v)
	}
	checksumMode := strings.EqualFold(req.r.Header.Get("X-Amz-Checksum-Mode"), "ENABLED")
	if part > 0 && checksumMode && len(info.Parts) > 0 && len(info.Parts[part-1].Checksums) > 0 {
		setChecksumHeaders(hdr, info.Parts[part-1].Checksums)
		hdr.Set("X-Amz-Checksum-Type", info.ChecksumType)
	} else if part == 0 && checksumMode && req.r.Header.Get("Range") == "" && len(info.Checksums) > 0 {
		setChecksumHeaders(hdr, info.Checksums)
		typ := info.ChecksumType
		if typ == "" {
			typ = storage.ChecksumFullObject
		}
		hdr.Set("X-Amz-Checksum-Type", typ)
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

// deleteConditions reads the headers of a conditional DeleteObject.
func deleteConditions(hdr http.Header) (storage.DeleteConditions, error) {
	c := storage.DeleteConditions{IfMatch: hdr.Get("If-Match")}
	if v := hdr.Get("X-Amz-If-Match-Size"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return c, s3err.InvalidArgument("x-amz-if-match-size must be a non-negative integer.")
		}
		c.Size, c.HasSize = n, true
	}
	if v := hdr.Get("X-Amz-If-Match-Last-Modified-Time"); v != "" {
		t, err := parseDate(v)
		if err != nil {
			return c, s3err.InvalidArgument("x-amz-if-match-last-modified-time must be a date.")
		}
		c.LastModified = t
	}
	return c, nil
}

// parseDate reads an HTTP date or an ISO 8601 one; the SDKs send the former even inside
// XML documents.
func parseDate(v string) (time.Time, error) {
	if t, err := http.ParseTime(v); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, v)
}

func (h *Handler) deleteObject(req *request) error {
	cond, err := deleteConditions(req.r.Header)
	if err != nil {
		return err
	}
	if err := h.store.DeleteObjectIf(req.bucket, req.key, cond); err != nil {
		return err
	}
	req.w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) deleteObjects(req *request) error {
	body, err := readXMLBody(req, h.opts.BodyTimeout, true)
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
			cond := storage.DeleteConditions{IfMatch: o.ETag}
			if o.Size != nil {
				if *o.Size < 0 {
					err = s3err.InvalidArgument("Size must be a non-negative integer.")
				}
				cond.Size, cond.HasSize = *o.Size, true
			}
			if o.LastModifiedTime != "" && err == nil {
				cond.LastModified, err = parseDate(o.LastModifiedTime)
				if err != nil {
					err = s3err.InvalidArgument("LastModifiedTime must be a date.")
				}
			}
			if err == nil {
				err = h.store.DeleteObjectIf(req.bucket, o.Key, cond)
			}
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
	switch strings.ToUpper(req.r.Header.Get("X-Amz-Tagging-Directive")) {
	case "", "COPY":
	case "REPLACE":
		tags, err := parseTaggingHeader(req.r.Header.Get("X-Amz-Tagging"))
		if err != nil {
			return err
		}
		opts.ReplaceTags, opts.Tags = true, tags
	default:
		return s3err.InvalidArgument("Unknown tagging directive.")
	}
	opts.Cond = writeConditions(req.r.Header)
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

var objectAttributes = map[string]bool{"ETag": true, "Checksum": true, "ObjectParts": true, "StorageClass": true, "ObjectSize": true}

func (h *Handler) getObjectAttributes(req *request) error {
	want := map[string]bool{}
	for _, v := range req.r.Header.Values("X-Amz-Object-Attributes") {
		for a := range strings.SplitSeq(v, ",") {
			a = strings.TrimSpace(a)
			if !objectAttributes[a] {
				return s3err.InvalidArgument("Invalid attribute name specified: " + a + ".")
			}
			want[a] = true
		}
	}
	if len(want) == 0 {
		return s3err.InvalidArgument("The x-amz-object-attributes header specifying the attributes to be retrieved is either missing or empty.")
	}
	maxParts, err := intParam(req.r.Header.Get("X-Amz-Max-Parts"), maxListParts, maxListParts)
	if err != nil {
		return err
	}
	if maxParts == 0 {
		maxParts = maxListParts
	}
	marker, err := intParam(req.r.Header.Get("X-Amz-Part-Number-Marker"), 0, maxPartNumber)
	if err != nil {
		return err
	}
	info, err := h.store.HeadObject(req.bucket, req.key)
	if err != nil {
		return err
	}

	res := objectAttributesResult{NS: s3NS}
	if want["ETag"] {
		res.ETag = strings.Trim(info.ETag, `"`)
	}
	if want["Checksum"] && len(info.Checksums) > 0 {
		typ := info.ChecksumType
		if typ == "" {
			typ = storage.ChecksumFullObject
		}
		res.Checksum = &attributesChecksum{checksumFields: newChecksumFields(info.Checksums), ChecksumType: typ}
	}
	if want["ObjectParts"] && len(info.Parts) > 0 {
		op := &attributesParts{TotalPartsCount: len(info.Parts), PartNumberMarker: marker, MaxParts: maxParts}
		for i := marker; i < len(info.Parts); i++ {
			if len(op.Parts) == maxParts {
				op.IsTruncated = true
				break
			}
			p := info.Parts[i]
			op.Parts = append(op.Parts, attributesPart{PartNumber: i + 1, Size: p.Size, checksumFields: newChecksumFields(p.Checksums)})
			op.NextPartNumberMarker = i + 1
		}
		res.ObjectParts = op
	}
	if want["StorageClass"] {
		res.StorageClass = "STANDARD"
	}
	if want["ObjectSize"] {
		res.ObjectSize = &info.Size
	}
	req.w.Header().Set("Last-Modified", info.ModTime.UTC().Format(http.TimeFormat))
	writeXML(req.w, http.StatusOK, res)
	return nil
}

// partNumber reads the partNumber parameter of a GET or HEAD, 0 when there is none.
func partNumber(req *request) (int, error) {
	v := req.q.Get("partNumber")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > maxPartNumber {
		return 0, s3err.InvalidArgument("Part number must be an integer between 1 and 10000, inclusive.")
	}
	if req.r.Header.Get("Range") != "" {
		return 0, s3err.InvalidRequest("Cannot specify both Range header and partNumber query parameter.")
	}
	return n, nil
}

// servePart sets the Range of part n for http.ServeContent to serve as 206; an object
// not uploaded in parts is one part. It reports whether the part is empty, which no
// Range can express.
func servePart(req *request, info storage.ObjectInfo, n int) (empty bool, err error) {
	sizes := []int64{info.Size}
	if len(info.Parts) > 0 {
		sizes = sizes[:0]
		for _, p := range info.Parts {
			sizes = append(sizes, p.Size)
		}
	}
	if n > len(sizes) {
		return false, s3err.InvalidPartNumber()
	}
	if len(info.Parts) > 0 {
		req.w.Header().Set("X-Amz-Mp-Parts-Count", strconv.Itoa(len(sizes)))
	}
	var start int64
	for _, s := range sizes[:n-1] {
		start += s
	}
	size := sizes[n-1]
	if size == 0 {
		return true, nil
	}
	req.r.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, start+size-1))
	return false, nil
}

// checkRange answers an unsatisfiable single byte range with S3's error document:
// http.ServeContent would reply in plain text, and serve an empty object whole.
func checkRange(h string, size int64) error {
	spec, ok := strings.CutPrefix(h, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return nil
	}
	first, last, ok := strings.Cut(strings.TrimSpace(spec), "-")
	if !ok {
		return nil
	}
	if first == "" {
		n, err := strconv.ParseInt(last, 10, 64)
		if err == nil && (n == 0 || size == 0) {
			return s3err.InvalidRange()
		}
		return nil
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err == nil && start >= size {
		return s3err.InvalidRange()
	}
	return nil
}
