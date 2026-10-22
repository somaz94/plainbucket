// Package checksum implements the S3 additional checksum algorithms with the standard library.
package checksum

import (
	"crypto/sha1" //nolint:gosec // S3 checksum algorithm, not a security boundary
	"crypto/sha256"
	"encoding/base64"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"strings"
)

// Algorithms lists the supported algorithm names as S3 spells them.
var Algorithms = []string{"CRC32", "CRC32C", "CRC64NVME", "SHA1", "SHA256"}

var (
	castagnoli = crc32.MakeTable(crc32.Castagnoli)
	// The reflected form of the NVMe polynomial 0xad93d23594c93659.
	nvme = crc64.MakeTable(0x9a6c9329ac4bc9b5)
)

// New returns a hash for alg (case-insensitive), or false when alg is unsupported.
func New(alg string) (hash.Hash, bool) {
	switch strings.ToUpper(alg) {
	case "CRC32":
		return crc32.NewIEEE(), true
	case "CRC32C":
		return crc32.New(castagnoli), true
	case "CRC64NVME":
		return crc64.New(nvme), true
	case "SHA1":
		return sha1.New(), true //nolint:gosec // S3 checksum algorithm
	case "SHA256":
		return sha256.New(), true
	}
	return nil, false
}

// Header returns the request/response header carrying alg's value, e.g. x-amz-checksum-crc32.
func Header(alg string) string {
	return "x-amz-checksum-" + strings.ToLower(alg)
}

// FromHeader maps a header name such as x-amz-checksum-crc32c back to its algorithm.
func FromHeader(name string) (string, bool) {
	suffix, ok := strings.CutPrefix(strings.ToLower(name), "x-amz-checksum-")
	if !ok {
		return "", false
	}
	for _, a := range Algorithms {
		if strings.ToLower(a) == suffix {
			return a, true
		}
	}
	return "", false
}

// Encode renders a digest the way S3 transmits checksums: standard base64.
func Encode(sum []byte) string {
	return base64.StdEncoding.EncodeToString(sum)
}
