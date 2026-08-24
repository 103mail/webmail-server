package api

import (
	"encoding/base64"
	"strings"
)

// decodeBase64 decodes an attachment payload. Both standard and URL alphabets
// are accepted, with or without padding.
//
// The PHP decodes leniently and substitutes an empty string on failure, which
// silently stores a zero-byte attachment. Reporting the error instead lets the
// API answer 400 rather than accepting a corrupt upload.
func decodeBase64(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	s = strings.TrimSpace(s)
	encodings := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}
	for _, enc := range encodings {
		if data, err := enc.DecodeString(s); err == nil {
			return data, nil
		}
	}
	return nil, errBadBase64
}
