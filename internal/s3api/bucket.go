package s3api

import (
	"encoding/base64"
	"encoding/xml"
	"net/http"
	"strconv"

	"github.com/somaz94/plainbucket/internal/s3err"
	"github.com/somaz94/plainbucket/internal/sigv4"
	"github.com/somaz94/plainbucket/internal/storage"
)

const maxListKeys = 1000

func (h *Handler) listBuckets(req *request) error {
	bs, err := h.store.ListBuckets()
	if err != nil {
		return err
	}
	res := listAllMyBucketsResult{NS: s3NS, Owner: serverOwner, Buckets: []bucketItem{}}
	for _, b := range bs {
		res.Buckets = append(res.Buckets, bucketItem{Name: b.Name, CreationDate: isoTime(b.Created)})
	}
	writeXML(req.w, http.StatusOK, res)
	return nil
}

func (h *Handler) createBucket(req *request) error {
	body, err := readXMLBody(req, h.opts.BodyTimeout)
	if err != nil {
		return err
	}
	if len(body) > 0 {
		var cfg createBucketConfiguration
		if xml.Unmarshal(body, &cfg) != nil {
			return s3err.MalformedXML()
		}
	}
	if err := h.store.CreateBucket(req.bucket); err != nil {
		return err
	}
	req.w.Header().Set("Location", "/"+req.bucket)
	req.w.WriteHeader(http.StatusOK)
	return nil
}

func (h *Handler) headBucket(req *request) error {
	if err := h.store.HeadBucket(req.bucket); err != nil {
		return err
	}
	req.w.Header().Set("X-Amz-Bucket-Region", h.opts.Region)
	req.w.WriteHeader(http.StatusOK)
	return nil
}

func (h *Handler) deleteBucket(req *request) error {
	if err := h.store.DeleteBucket(req.bucket); err != nil {
		return err
	}
	req.w.WriteHeader(http.StatusNoContent)
	return nil
}

func (h *Handler) getBucketLocation(req *request) error {
	if err := h.store.HeadBucket(req.bucket); err != nil {
		return err
	}
	loc := h.opts.Region
	if loc == "us-east-1" {
		loc = "" // S3 reports the default region as an empty constraint.
	}
	writeXML(req.w, http.StatusOK, locationConstraint{NS: s3NS, Location: loc})
	return nil
}

func (h *Handler) getBucketVersioning(req *request) error {
	if err := h.store.HeadBucket(req.bucket); err != nil {
		return err
	}
	writeXML(req.w, http.StatusOK, versioningConfiguration{NS: s3NS})
	return nil
}

// listObjects serves ListObjectsV2 (v2) and the original ListObjects.
func (h *Handler) listObjects(req *request, v2 bool) error {
	q := req.q
	maxKeys := maxListKeys
	if v := q.Get("max-keys"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return s3err.InvalidArgument("max-keys must be a non-negative integer.")
		}
		maxKeys = min(n, maxListKeys)
	}
	encodeURL := false
	switch q.Get("encoding-type") {
	case "":
	case "url":
		encodeURL = true
	default:
		return s3err.InvalidArgument("Invalid Encoding Method specified in Request.")
	}
	enc := func(s string) string {
		if encodeURL {
			return sigv4.URIEncodePath(s)
		}
		return s
	}

	p := storage.ListParams{Prefix: q.Get("prefix"), Delimiter: q.Get("delimiter"), MaxKeys: maxKeys}
	res := listBucketResult{
		NS: s3NS, Name: req.bucket, Prefix: enc(p.Prefix), Delimiter: enc(p.Delimiter), MaxKeys: maxKeys,
		Contents: []listObject{},
	}
	if encodeURL {
		res.EncodingType = "url"
	}
	if v2 {
		p.After = q.Get("start-after")
		res.StartAfter = enc(p.After)
		if tok := q.Get("continuation-token"); tok != "" {
			raw, err := base64.RawURLEncoding.DecodeString(tok)
			if err != nil {
				return s3err.InvalidArgument("The continuation token provided is incorrect.")
			}
			p.After = string(raw)
			res.ContinuationToken = tok
		}
	} else {
		p.After = q.Get("marker")
		marker := enc(p.After)
		res.Marker = &marker
	}

	out, err := h.store.List(req.bucket, p)
	if err != nil {
		return err
	}
	withOwner := !v2 || q.Get("fetch-owner") == "true"
	for _, o := range out.Objects {
		item := listObject{Key: enc(o.Key), LastModified: isoTime(o.ModTime), ETag: o.ETag, Size: o.Size, StorageClass: "STANDARD"}
		if withOwner {
			item.Owner = &serverOwner
		}
		res.Contents = append(res.Contents, item)
	}
	for _, cp := range out.CommonPrefixes {
		res.CommonPrefixes = append(res.CommonPrefixes, commonPrefix{Prefix: enc(cp)})
	}
	res.IsTruncated = out.Truncated
	if v2 {
		n := len(out.Objects) + len(out.CommonPrefixes)
		res.KeyCount = &n
		if out.Truncated {
			res.NextContinuationToken = base64.RawURLEncoding.EncodeToString([]byte(out.Next))
		}
	} else if out.Truncated {
		res.NextMarker = enc(out.Next)
	}
	writeXML(req.w, http.StatusOK, res)
	return nil
}
