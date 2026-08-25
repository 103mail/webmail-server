package federation

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"time"
)

// NewUUID returns a UUIDv7 (RFC 9562) in canonical lowercase form.
//
// Version 7 rather than 4 because the first 48 bits are a millisecond
// timestamp, which makes these identifiers sort by creation time. That matters
// for the indexes they sit in: envelope ids, thread ids and message ids are all
// inserted in roughly ascending order, so a B-tree appends rather than splits.
func NewUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate uuid: %w", err)
	}
	millis := uint64(time.Now().UnixMilli())
	// 48-bit big-endian timestamp in the first six bytes.
	binary.BigEndian.PutUint16(b[0:2], uint16(millis>>32))
	binary.BigEndian.PutUint32(b[2:6], uint32(millis))
	// Version 7 in the high nibble of byte 6, RFC 4122 variant in byte 8.
	b[6] = (b[6] & 0x0f) | 0x70
	b[8] = (b[8] & 0x3f) | 0x80

	dst := make([]byte, 36)
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst), nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func base64Std(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// readCapped reads at most n bytes, for response bodies that only ever go into
// a log line.
func readCapped(r io.Reader, n int64) (string, error) {
	buf, err := io.ReadAll(io.LimitReader(r, n))
	return string(buf), err
}
