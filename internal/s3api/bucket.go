package s3api

import (
	"encoding/base64"
	"encoding/xml"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/somaz94/plainbucket/internal/s3err"
	"github.com/somaz94/plainbucket/internal/sigv4"
	"github.com/somaz94/plainbucket/internal/storage"
)

const (
	maxListKeys    = 1000
	maxListBuckets = 10000
)

// listBuckets serves ListBuckets, paginated when max-buckets or continuation-token is
// given; the token is the last bucket name returned, encoded.
func (h *Handler) listBuckets(req *request) error {
	limit := -1
	if v := req.q.Get("max-buckets"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxListBuckets {
			return s3err.InvalidArgument("max-buckets must be an integer from 1 to 10000.")
		}
		limit = n
	}
	after := ""
	if t := req.q.Get("continuation-token"); t != "" {
		name, err := base64.RawURLEncoding.DecodeString(t)
		if err != nil || !storage.ValidBucketName(string(name)) {
			return s3err.InvalidArgument("The continuation token provided is incorrect.")
		}
		after = string(name)
		if limit < 0 {
			limit = maxListBuckets
		}
	}
	prefix := req.q.Get("prefix")
	res := listAllMyBucketsResult{NS: s3NS, Owner: serverOwner, Buckets: []bucketItem{}, Prefix: prefix}
	if r := req.q.Get("bucket-region"); r != "" && r != h.opts.Region {
		writeXML(req.w, http.StatusOK, res)
		return nil
	}
	bs, err := h.store.ListBuckets()
	if err != nil {
		return err
	}
	for _, b := range bs {
		if b.Name <= after || !strings.HasPrefix(b.Name, prefix) || !req.mayRead(b.Name) {
			continue
		}
		if len(res.Buckets) == limit {
			res.ContinuationToken = base64.RawURLEncoding.EncodeToString([]byte(res.Buckets[limit-1].Name))
			break
		}
		res.Buckets = append(res.Buckets, bucketItem{Name: b.Name, CreationDate: isoTime(b.Created)})
	}
	writeXML(req.w, http.StatusOK, res)
	return nil
}

func (h *Handler) createBucket(req *request) error {
	body, err := readXMLBody(req, h.opts.BodyTimeout, true)
	if err != nil {
		return err
	}
	var tags []storage.Tag
	if len(body) > 0 {
		var cfg createBucketConfiguration
		if xml.Unmarshal(body, &cfg) != nil {
			return s3err.MalformedXML()
		}
		for _, t := range cfg.Tags {
			tags = append(tags, storage.Tag(t))
		}
		if tags, err = sortedBucketTags(tags); err != nil {
			return err
		}
	}
	// us-east-1 answers re-creating a bucket the caller owns with 200, for legacy
	// compatibility, leaving it as it is; every other region refuses.
	err = h.store.CreateBucketWithTags(req.bucket, tags)
	if errors.Is(err, storage.ErrBucketExists) && h.opts.Region == "us-east-1" && h.store.HeadBucket(req.bucket) == nil {
		err = nil // a file of that name at the root is not a bucket
	}
	if err != nil {
		return err
	}
	loc := "/" + req.bucket
	if req.vhost {
		loc = req.url("")
	}
	req.w.Header().Set("Location", loc)
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

// listQuery reads max-keys and encoding-type, shared by every object listing.
func listQuery(q url.Values) (maxKeys int, enc func(string) string, err error) {
	maxKeys = maxListKeys
	if v := q.Get("max-keys"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, nil, s3err.InvalidArgument("max-keys must be a non-negative integer.")
		}
		maxKeys = min(n, maxListKeys)
	}
	enc, err = encodingType(q)
	return maxKeys, enc, err
}

// encodingType maps encoding-type to the key encoder of a listing.
func encodingType(q url.Values) (func(string) string, error) {
	switch q.Get("encoding-type") {
	case "":
		return func(s string) string { return s }, nil
	case "url":
		return sigv4.URIEncodePath, nil
	}
	return nil, s3err.InvalidArgument("Invalid Encoding Method specified in Request.")
}

// listObjects serves ListObjectsV2 (v2) and the original ListObjects.
func (h *Handler) listObjects(req *request, v2 bool) error {
	q := req.q
	maxKeys, enc, err := listQuery(q)
	if err != nil {
		return err
	}

	p := storage.ListParams{Prefix: q.Get("prefix"), Delimiter: q.Get("delimiter"), MaxKeys: maxKeys}
	// Only V2 encodes the prefix it echoes; SDKs decode it there and not in V1.
	prefix := p.Prefix
	if v2 {
		prefix = enc(prefix)
	}
	res := listBucketResult{
		NS: s3NS, Name: req.bucket, Prefix: prefix, Delimiter: enc(p.Delimiter), MaxKeys: maxKeys,
		Contents: []listObject{},
	}
	res.EncodingType = q.Get("encoding-type")
	if v2 {
		p.After = q.Get("start-after")
		res.StartAfter = enc(p.After)
		if q.Has("continuation-token") {
			tok := q.Get("continuation-token")
			raw, err := base64.RawURLEncoding.DecodeString(tok)
			if err != nil {
				return s3err.InvalidArgument("The continuation token provided is incorrect.")
			}
			if len(raw) > 0 {
				p.After = string(raw)
			}
			res.ContinuationToken = &tok
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

// listObjectVersions answers ListObjectVersions for an unversioned bucket, as S3 does:
// every object is its only version, "null", and there are no delete markers.
func (h *Handler) listObjectVersions(req *request) error {
	q := req.q
	maxKeys, enc, err := listQuery(q)
	if err != nil {
		return err
	}
	keyMarker, versionMarker := q.Get("key-marker"), q.Get("version-id-marker")
	if versionMarker != "" && keyMarker == "" {
		return s3err.InvalidArgument("A version-id marker cannot be specified without a key marker.")
	}
	if versionMarker != "" && versionMarker != "null" {
		return s3err.InvalidArgument("Invalid version id specified")
	}

	p := storage.ListParams{Prefix: q.Get("prefix"), Delimiter: q.Get("delimiter"), After: keyMarker, MaxKeys: maxKeys}
	out, err := h.store.List(req.bucket, p)
	if err != nil {
		return err
	}
	res := listVersionsResult{
		NS: s3NS, Name: req.bucket, Prefix: enc(p.Prefix), Delimiter: enc(p.Delimiter), MaxKeys: maxKeys,
		KeyMarker: enc(keyMarker), VersionIDMarker: versionMarker, IsTruncated: out.Truncated,
		Versions: []objectVersion{},
	}
	res.EncodingType = q.Get("encoding-type")
	for _, o := range out.Objects {
		res.Versions = append(res.Versions, objectVersion{
			Key: enc(o.Key), VersionID: "null", IsLatest: true, LastModified: isoTime(o.ModTime),
			ETag: o.ETag, Size: o.Size, StorageClass: "STANDARD", Owner: &serverOwner,
		})
	}
	for _, cp := range out.CommonPrefixes {
		res.CommonPrefixes = append(res.CommonPrefixes, commonPrefix{Prefix: enc(cp)})
	}
	if out.Truncated {
		res.NextKeyMarker = enc(out.Next)
		if n := len(out.Objects); n > 0 && out.Objects[n-1].Key == out.Next {
			res.NextVersionIDMarker = "null"
		}
	}
	writeXML(req.w, http.StatusOK, res)
	return nil
}
