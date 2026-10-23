package s3api

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/somaz94/plainbucket/internal/s3err"
	"github.com/somaz94/plainbucket/internal/storage"
)

// S3's limits on object tags; key and value lengths are in characters.
const (
	maxTags        = 10
	maxTagKeyLen   = 128
	maxTagValueLen = 256
)

// sortedTags checks tags and sorts them by key, the order they are returned in.
func sortedTags(tags []storage.Tag) ([]storage.Tag, error) {
	if err := checkTags(tags); err != nil {
		return nil, err
	}
	slices.SortFunc(tags, func(a, b storage.Tag) int { return strings.Compare(a.Key, b.Key) })
	return tags, nil
}

func checkTags(tags []storage.Tag) error {
	if len(tags) > maxTags {
		return s3err.InvalidTag(fmt.Sprintf("Object tags cannot be greater than %d.", maxTags))
	}
	seen := map[string]bool{}
	for _, t := range tags {
		switch {
		case t.Key == "" || utf8.RuneCountInString(t.Key) > maxTagKeyLen:
			return s3err.InvalidTag(fmt.Sprintf("The TagKey you have provided is invalid; it must be 1 to %d characters.", maxTagKeyLen))
		case utf8.RuneCountInString(t.Value) > maxTagValueLen:
			return s3err.InvalidTag(fmt.Sprintf("The TagValue you have provided is too long, max length is %d.", maxTagValueLen))
		case strings.HasPrefix(strings.ToLower(t.Key), "aws:"):
			return s3err.InvalidTag("Your TagKey cannot be prefixed with aws:")
		case !utf8.ValidString(t.Key) || !utf8.ValidString(t.Value):
			return s3err.InvalidTag("Tags must be valid UTF-8.")
		case seen[t.Key]:
			return s3err.InvalidTag("Cannot provide multiple Tags with the same key.")
		}
		seen[t.Key] = true
	}
	return nil
}

// parseTaggingHeader reads x-amz-tagging, a URL query string.
func parseTaggingHeader(v string) ([]storage.Tag, error) {
	var tags []storage.Tag
	for pair := range strings.SplitSeq(v, "&") {
		if pair == "" {
			continue
		}
		k, val, _ := strings.Cut(pair, "=")
		key, err1 := url.QueryUnescape(k)
		value, err2 := url.QueryUnescape(val)
		if err1 != nil || err2 != nil {
			return nil, s3err.InvalidArgument("The header 'x-amz-tagging' shall be encoded as UTF-8 then URLEncoded URL query parameters without tag name duplicates.")
		}
		tags = append(tags, storage.Tag{Key: key, Value: value})
	}
	return sortedTags(tags)
}

// writeMetadata is metadataFromHeaders plus the tags in x-amz-tagging.
func writeMetadata(h http.Header) (storage.Metadata, error) {
	md := metadataFromHeaders(h)
	tags, err := parseTaggingHeader(h.Get("X-Amz-Tagging"))
	md.Tags = tags
	return md, err
}

// parseTagging reads a Tagging document, as PutObjectTagging and POST uploads send it.
func parseTagging(body []byte) ([]storage.Tag, error) {
	var doc struct {
		XMLName xml.Name `xml:"Tagging"`
		TagSet  *struct {
			Tags []xmlTag `xml:"Tag"`
		} `xml:"TagSet"`
	}
	if xml.Unmarshal(body, &doc) != nil || doc.TagSet == nil {
		return nil, s3err.MalformedXML()
	}
	tags := make([]storage.Tag, len(doc.TagSet.Tags))
	for i, t := range doc.TagSet.Tags {
		tags[i] = storage.Tag{Key: t.Key, Value: t.Value}
	}
	return sortedTags(tags)
}

func setTagCount(hdr http.Header, tags []storage.Tag) {
	if len(tags) > 0 {
		hdr.Set("X-Amz-Tagging-Count", strconv.Itoa(len(tags)))
	}
}

func (h *Handler) getObjectTagging(req *request) error {
	info, err := h.store.HeadObject(req.bucket, req.key)
	if err != nil {
		return err
	}
	res := tagging{NS: s3NS}
	for _, t := range info.Tags {
		res.TagSet.Tags = append(res.TagSet.Tags, xmlTag(t))
	}
	writeXML(req.w, http.StatusOK, res)
	return nil
}

func (h *Handler) putObjectTagging(req *request) error {
	body, err := readXMLBody(req, h.opts.BodyTimeout, true)
	if err != nil {
		return err
	}
	tags, err := parseTagging(body)
	if err != nil {
		return err
	}
	if err := h.store.SetTags(req.bucket, req.key, tags); err != nil {
		return err
	}
	req.w.WriteHeader(http.StatusOK)
	return nil
}

func (h *Handler) deleteObjectTagging(req *request) error {
	if err := h.store.SetTags(req.bucket, req.key, nil); err != nil {
		return err
	}
	req.w.WriteHeader(http.StatusNoContent)
	return nil
}
