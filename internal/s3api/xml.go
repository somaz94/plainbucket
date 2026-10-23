package s3api

import (
	"encoding/xml"
	"net/http"
	"time"
)

const (
	s3NS       = "http://s3.amazonaws.com/doc/2006-03-01/"
	timeLayout = "2006-01-02T15:04:05.000Z"
)

func isoTime(t time.Time) string { return t.UTC().Format(timeLayout) }

type owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

var serverOwner = owner{ID: "plainbucket", DisplayName: "plainbucket"}

type listAllMyBucketsResult struct {
	XMLName xml.Name     `xml:"ListAllMyBucketsResult"`
	NS      string       `xml:"xmlns,attr"`
	Owner   owner        `xml:"Owner"`
	Buckets []bucketItem `xml:"Buckets>Bucket"`
	// ContinuationToken asks for the next page; absent on the last one.
	ContinuationToken string `xml:"ContinuationToken,omitempty"`
	Prefix            string `xml:"Prefix,omitempty"`
}

type bucketItem struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

type locationConstraint struct {
	XMLName  xml.Name `xml:"LocationConstraint"`
	NS       string   `xml:"xmlns,attr"`
	Location string   `xml:",chardata"`
}

type versioningConfiguration struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	NS      string   `xml:"xmlns,attr"`
}

type createBucketConfiguration struct {
	XMLName            xml.Name `xml:"CreateBucketConfiguration"`
	LocationConstraint string   `xml:"LocationConstraint"`
}

type listObject struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
	Owner        *owner `xml:"Owner,omitempty"`
}

type commonPrefix struct {
	Prefix string `xml:"Prefix"`
}

type listBucketResult struct {
	XMLName               xml.Name       `xml:"ListBucketResult"`
	NS                    string         `xml:"xmlns,attr"`
	Name                  string         `xml:"Name"`
	Prefix                string         `xml:"Prefix"`
	Marker                *string        `xml:"Marker,omitempty"`
	NextMarker            string         `xml:"NextMarker,omitempty"`
	ContinuationToken     *string        `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string         `xml:"NextContinuationToken,omitempty"`
	StartAfter            string         `xml:"StartAfter,omitempty"`
	KeyCount              *int           `xml:"KeyCount,omitempty"`
	MaxKeys               int            `xml:"MaxKeys"`
	Delimiter             string         `xml:"Delimiter,omitempty"`
	EncodingType          string         `xml:"EncodingType,omitempty"`
	IsTruncated           bool           `xml:"IsTruncated"`
	Contents              []listObject   `xml:"Contents"`
	CommonPrefixes        []commonPrefix `xml:"CommonPrefixes"`
}

type objectVersion struct {
	Key          string `xml:"Key"`
	VersionID    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
	Owner        *owner `xml:"Owner,omitempty"`
}

type listVersionsResult struct {
	XMLName             xml.Name        `xml:"ListVersionsResult"`
	NS                  string          `xml:"xmlns,attr"`
	Name                string          `xml:"Name"`
	Prefix              string          `xml:"Prefix"`
	KeyMarker           string          `xml:"KeyMarker"`
	VersionIDMarker     string          `xml:"VersionIdMarker"`
	NextKeyMarker       string          `xml:"NextKeyMarker,omitempty"`
	NextVersionIDMarker string          `xml:"NextVersionIdMarker,omitempty"`
	MaxKeys             int             `xml:"MaxKeys"`
	Delimiter           string          `xml:"Delimiter,omitempty"`
	EncodingType        string          `xml:"EncodingType,omitempty"`
	IsTruncated         bool            `xml:"IsTruncated"`
	Versions            []objectVersion `xml:"Version"`
	CommonPrefixes      []commonPrefix  `xml:"CommonPrefixes"`
}

type attributesChecksum struct {
	checksumFields
	ChecksumType string `xml:"ChecksumType"`
}

type attributesPart struct {
	PartNumber int   `xml:"PartNumber"`
	Size       int64 `xml:"Size"`
	checksumFields
}

type attributesParts struct {
	TotalPartsCount      int              `xml:"PartsCount"`
	PartNumberMarker     int              `xml:"PartNumberMarker"`
	NextPartNumberMarker int              `xml:"NextPartNumberMarker"`
	MaxParts             int              `xml:"MaxParts"`
	IsTruncated          bool             `xml:"IsTruncated"`
	Parts                []attributesPart `xml:"Part"`
}

type objectAttributesResult struct {
	XMLName      xml.Name            `xml:"GetObjectAttributesResponse"`
	NS           string              `xml:"xmlns,attr"`
	ETag         string              `xml:"ETag,omitempty"`
	Checksum     *attributesChecksum `xml:"Checksum,omitempty"`
	ObjectParts  *attributesParts    `xml:"ObjectParts,omitempty"`
	StorageClass string              `xml:"StorageClass,omitempty"`
	ObjectSize   *int64              `xml:"ObjectSize,omitempty"`
}

type copyPartResult struct {
	XMLName      xml.Name `xml:"CopyPartResult"`
	NS           string   `xml:"xmlns,attr"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
}

type tagging struct {
	XMLName xml.Name `xml:"Tagging"`
	NS      string   `xml:"xmlns,attr,omitempty"`
	// A struct, so an empty set is still written: the SDKs read a missing TagSet as absent.
	TagSet struct {
		Tags []xmlTag `xml:"Tag"`
	} `xml:"TagSet"`
}

type xmlTag struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

type copyObjectResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	NS           string   `xml:"xmlns,attr"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
}

type deleteRequest struct {
	XMLName xml.Name `xml:"Delete"`
	Quiet   bool     `xml:"Quiet"`
	Objects []struct {
		Key              string `xml:"Key"`
		VersionID        string `xml:"VersionId"`
		ETag             string `xml:"ETag"`
		Size             *int64 `xml:"Size"`
		LastModifiedTime string `xml:"LastModifiedTime"`
	} `xml:"Object"`
}

type deletedItem struct {
	Key string `xml:"Key"`
}

type deleteError struct {
	Key     string `xml:"Key"`
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

type deleteResult struct {
	XMLName xml.Name      `xml:"DeleteResult"`
	NS      string        `xml:"xmlns,attr"`
	Deleted []deletedItem `xml:"Deleted"`
	Errors  []deleteError `xml:"Error"`
}

type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	NS       string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

type completeMultipartUpload struct {
	XMLName xml.Name `xml:"CompleteMultipartUpload"`
	Parts   []struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
		checksumFields
	} `xml:"Part"`
}

type completeMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	NS       string   `xml:"xmlns,attr"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
	checksumFields
	ChecksumType string `xml:"ChecksumType,omitempty"`
}

// checksumFields carries one value per checksum algorithm in S3 documents.
type checksumFields struct {
	ChecksumCRC32     string `xml:"ChecksumCRC32,omitempty"`
	ChecksumCRC32C    string `xml:"ChecksumCRC32C,omitempty"`
	ChecksumCRC64NVME string `xml:"ChecksumCRC64NVME,omitempty"`
	ChecksumSHA1      string `xml:"ChecksumSHA1,omitempty"`
	ChecksumSHA256    string `xml:"ChecksumSHA256,omitempty"`
}

func (c *checksumFields) fields() map[string]*string {
	return map[string]*string{
		"CRC32": &c.ChecksumCRC32, "CRC32C": &c.ChecksumCRC32C, "CRC64NVME": &c.ChecksumCRC64NVME,
		"SHA1": &c.ChecksumSHA1, "SHA256": &c.ChecksumSHA256,
	}
}

func newChecksumFields(sums map[string]string) checksumFields {
	var c checksumFields
	for alg, f := range c.fields() {
		*f = sums[alg]
	}
	return c
}

// values returns the algorithms that carry a value.
func (c checksumFields) values() map[string]string {
	out := map[string]string{}
	for alg, f := range c.fields() {
		if *f != "" {
			out[alg] = *f
		}
	}
	return out
}

type partItem struct {
	PartNumber   int    `xml:"PartNumber"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	checksumFields
}

type listPartsResult struct {
	XMLName              xml.Name   `xml:"ListPartsResult"`
	NS                   string     `xml:"xmlns,attr"`
	Bucket               string     `xml:"Bucket"`
	Key                  string     `xml:"Key"`
	UploadID             string     `xml:"UploadId"`
	Initiator            owner      `xml:"Initiator"`
	Owner                owner      `xml:"Owner"`
	StorageClass         string     `xml:"StorageClass"`
	PartNumberMarker     int        `xml:"PartNumberMarker"`
	NextPartNumberMarker int        `xml:"NextPartNumberMarker"`
	MaxParts             int        `xml:"MaxParts"`
	IsTruncated          bool       `xml:"IsTruncated"`
	Parts                []partItem `xml:"Part"`
}

type uploadItem struct {
	Key          string `xml:"Key"`
	UploadID     string `xml:"UploadId"`
	Initiator    owner  `xml:"Initiator"`
	Owner        owner  `xml:"Owner"`
	StorageClass string `xml:"StorageClass"`
	Initiated    string `xml:"Initiated"`
}

type listMultipartUploadsResult struct {
	XMLName            xml.Name       `xml:"ListMultipartUploadsResult"`
	NS                 string         `xml:"xmlns,attr"`
	Bucket             string         `xml:"Bucket"`
	KeyMarker          string         `xml:"KeyMarker"`
	UploadIDMarker     string         `xml:"UploadIdMarker"`
	NextKeyMarker      string         `xml:"NextKeyMarker,omitempty"`
	NextUploadIDMarker string         `xml:"NextUploadIdMarker,omitempty"`
	Prefix             string         `xml:"Prefix"`
	Delimiter          string         `xml:"Delimiter,omitempty"`
	EncodingType       string         `xml:"EncodingType,omitempty"`
	MaxUploads         int            `xml:"MaxUploads"`
	IsTruncated        bool           `xml:"IsTruncated"`
	Uploads            []uploadItem   `xml:"Upload"`
	CommonPrefixes     []commonPrefix `xml:"CommonPrefixes"`
}

func writeXML(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(v)
}
