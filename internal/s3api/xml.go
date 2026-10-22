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
	ContinuationToken     string         `xml:"ContinuationToken,omitempty"`
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

type copyPartResult struct {
	XMLName      xml.Name `xml:"CopyPartResult"`
	NS           string   `xml:"xmlns,attr"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
}

type tagging struct {
	XMLName xml.Name `xml:"Tagging"`
	NS      string   `xml:"xmlns,attr"`
	TagSet  struct{} `xml:"TagSet"`
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
		Key       string `xml:"Key"`
		VersionID string `xml:"VersionId"`
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
	} `xml:"Part"`
}

type completeMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	NS       string   `xml:"xmlns,attr"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

type partItem struct {
	PartNumber        int    `xml:"PartNumber"`
	LastModified      string `xml:"LastModified"`
	ETag              string `xml:"ETag"`
	Size              int64  `xml:"Size"`
	ChecksumCRC32     string `xml:"ChecksumCRC32,omitempty"`
	ChecksumCRC32C    string `xml:"ChecksumCRC32C,omitempty"`
	ChecksumCRC64NVME string `xml:"ChecksumCRC64NVME,omitempty"`
	ChecksumSHA1      string `xml:"ChecksumSHA1,omitempty"`
	ChecksumSHA256    string `xml:"ChecksumSHA256,omitempty"`
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
	XMLName            xml.Name     `xml:"ListMultipartUploadsResult"`
	NS                 string       `xml:"xmlns,attr"`
	Bucket             string       `xml:"Bucket"`
	KeyMarker          string       `xml:"KeyMarker"`
	UploadIDMarker     string       `xml:"UploadIdMarker"`
	NextKeyMarker      string       `xml:"NextKeyMarker,omitempty"`
	NextUploadIDMarker string       `xml:"NextUploadIdMarker,omitempty"`
	Prefix             string       `xml:"Prefix"`
	MaxUploads         int          `xml:"MaxUploads"`
	IsTruncated        bool         `xml:"IsTruncated"`
	Uploads            []uploadItem `xml:"Upload"`
}

func writeXML(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(v)
}
