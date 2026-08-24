package mailer

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"

	"golang.org/x/net/html/charset"
)

// ParseMIME turns a raw RFC 5322 message into an Inbound.
//
// It is deliberately forgiving. Mail from the open internet is routinely
// malformed, and losing a whole message because one part has a broken
// boundary or a truncated base64 body would be worse than keeping the parts
// that did decode. Only a message with no parsable header at all is an error.
func ParseMIME(raw []byte) (*Inbound, error) {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse message: %w", err)
	}
	header := m.Header

	in := &Inbound{
		Raw:        raw,
		Subject:    decodeWord(header.Get("Subject")),
		MessageID:  trimAngles(header.Get("Message-Id")),
		InReplyTo:  trimAngles(header.Get("In-Reply-To")),
		References: parseReferences(header.Get("References")),
		To:         parseAddressList(header.Get("To")),
		Cc:         parseAddressList(header.Get("Cc")),
	}
	if from := parseAddressList(header.Get("From")); len(from) > 0 {
		in.From = from[0]
	}
	if date, err := header.Date(); err == nil {
		in.Date = date.UTC()
	}

	parts := 0
	walk(in, textproto.MIMEHeader(header), m.Body, 0, &parts)
	return in, nil
}

// walk descends one MIME entity, collecting bodies and attachments into in.
// depth and parts bound the work a hostile message can cause.
func walk(in *Inbound, header textproto.MIMEHeader, body io.Reader, depth int, parts *int) {
	if depth > maxMIMEDepth || *parts >= maxMIMEParts {
		return
	}
	*parts++

	mediaType, params := contentType(header)
	decoded := decodeTransfer(header.Get("Content-Transfer-Encoding"), body)

	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return
		}
		reader := multipart.NewReader(decoded, boundary)
		for {
			// NextRawPart, not NextPart: this walk decodes the transfer
			// encoding itself so that the top level message and its parts
			// take exactly the same path.
			next, err := reader.NextRawPart()
			if err != nil {
				return
			}
			walk(in, next.Header, next, depth+1, parts)
		}
	}

	disposition, dispositionParams := contentDisposition(header)
	isBodyType := mediaType == "text/plain" || mediaType == "text/html"
	if disposition == "attachment" || !isBodyType {
		name := dispositionParams["filename"]
		if name == "" {
			name = params["name"]
		}
		in.Attachments = append(in.Attachments, Attachment{
			FileName:  name,
			MimeType:  mediaType,
			Content:   readLimited(decoded),
			ContentID: trimAngles(header.Get("Content-Id")),
		})
		return
	}

	text := normalizeNewlines(decodeCharset(params["charset"], decoded))
	if mediaType == "text/html" {
		in.HTML = appendBody(in.HTML, text)
		return
	}
	in.Text = appendBody(in.Text, text)
}

// contentType reads the media type of an entity, defaulting to the plain text
// RFC 2045 prescribes for an entity that declares nothing.
func contentType(header textproto.MIMEHeader) (string, map[string]string) {
	value := header.Get("Content-Type")
	if strings.TrimSpace(value) == "" {
		return "text/plain", map[string]string{}
	}
	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil {
		// A broken parameter list still leaves a usable media type in front
		// of the first semicolon.
		mediaType = strings.ToLower(strings.TrimSpace(strings.Split(value, ";")[0]))
		if mediaType == "" {
			mediaType = "text/plain"
		}
		return mediaType, map[string]string{}
	}
	return mediaType, params
}

func contentDisposition(header textproto.MIMEHeader) (string, map[string]string) {
	value := header.Get("Content-Disposition")
	if strings.TrimSpace(value) == "" {
		return "", map[string]string{}
	}
	disposition, params, err := mime.ParseMediaType(value)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(strings.Split(value, ";")[0])), map[string]string{}
	}
	return disposition, params
}

func decodeTransfer(encoding string, r io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, r)
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	default:
		return r
	}
}

// decodeCharset converts a text body to UTF-8. An unknown or broken charset
// falls back to the bytes as they arrived, which is right more often than it
// is wrong, since the label is usually the only thing that was wrong.
func decodeCharset(label string, r io.Reader) string {
	data := readLimited(r)
	if label == "" || strings.EqualFold(label, "utf-8") || strings.EqualFold(label, "us-ascii") {
		return string(data)
	}
	converted, err := charset.NewReaderLabel(label, bytes.NewReader(data))
	if err != nil {
		return string(data)
	}
	out, err := io.ReadAll(io.LimitReader(converted, maxPartBytes))
	if err != nil {
		return string(data)
	}
	return string(out)
}

// readLimited reads at most maxPartBytes. A decode error keeps whatever came
// out before it, because a truncated attachment beats no message.
func readLimited(r io.Reader) []byte {
	data, _ := io.ReadAll(io.LimitReader(r, maxPartBytes))
	return data
}

// normalizeNewlines turns the CRLF that MIME requires on the wire back into
// the plain LF the rest of the system stores.
func normalizeNewlines(text string) string {
	if !strings.ContainsRune(text, '\r') {
		return text
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n")
}

func appendBody(existing, addition string) string {
	if existing == "" {
		return addition
	}
	if addition == "" {
		return existing
	}
	return existing + "\n" + addition
}

func decodeWord(value string) string {
	decoder := &mime.WordDecoder{CharsetReader: charset.NewReaderLabel}
	decoded, err := decoder.DecodeHeader(value)
	if err != nil {
		return value
	}
	return decoded
}

// parseAddressList reads an address header. A header too malformed to parse is
// kept verbatim rather than dropped, so the sender of a broken message is
// still visible to whoever has to deal with it.
func parseAddressList(value string) []Address {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	list, err := mail.ParseAddressList(value)
	if err != nil {
		return []Address{{Email: strings.TrimSpace(decodeWord(value))}}
	}
	addresses := make([]Address, 0, len(list))
	for _, a := range list {
		addresses = append(addresses, Address{Name: a.Name, Email: a.Address})
	}
	return addresses
}

func parseReferences(value string) []string {
	var references []string
	for _, field := range strings.Fields(value) {
		if id := trimAngles(field); id != "" {
			references = append(references, id)
		}
	}
	return references
}

func trimAngles(value string) string {
	return strings.Trim(strings.TrimSpace(value), "<>")
}
