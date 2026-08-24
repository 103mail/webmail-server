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
	"sort"
	"strings"
	"time"
)

// Limits on what ParseMIME will read out of an untrusted message. A message
// that exceeds them is truncated rather than rejected, so a malformed part
// never costs us the rest of the mail.
const (
	maxMIMEDepth = 16
	maxMIMEParts = 512
	maxPartBytes = 32 << 20
)

// BuildMIME renders msg as an RFC 5322 message.
//
// Bcc recipients are deliberately absent from the output. They travel in the
// OutboundNotification instead, so a subscriber knows to deliver a copy without
// the other recipients learning of it.
func BuildMIME(msg Outbound) ([]byte, error) {
	if err := msg.Validate(); err != nil {
		return nil, err
	}
	body, err := buildBody(msg)
	if err != nil {
		return nil, err
	}

	date := msg.Date
	if date.IsZero() {
		date = time.Now()
	}

	var buf bytes.Buffer
	writeHeader(&buf, "MIME-Version", "1.0")
	writeHeader(&buf, "Date", date.Format(time.RFC1123Z))
	writeHeader(&buf, "From", formatAddress(msg.From))
	if len(msg.To) > 0 {
		writeHeader(&buf, "To", formatAddresses(msg.To))
	}
	if len(msg.Cc) > 0 {
		writeHeader(&buf, "Cc", formatAddresses(msg.Cc))
	}
	if len(msg.ReplyTo) > 0 {
		writeHeader(&buf, "Reply-To", formatAddresses(msg.ReplyTo))
	}
	writeHeader(&buf, "Subject", mime.QEncoding.Encode("utf-8", msg.Subject))

	// Sorted so that the same message always renders the same bytes, which is
	// what makes the output testable.
	names := make([]string, 0, len(msg.Headers))
	for name := range msg.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		writeHeader(&buf, textproto.CanonicalMIMEHeaderKey(name), msg.Headers[name])
	}

	for _, name := range sortedKeys(body.header) {
		for _, value := range body.header[name] {
			writeHeader(&buf, name, value)
		}
	}
	buf.WriteString("\r\n")
	buf.Write(body.body)
	return buf.Bytes(), nil
}

// part is one MIME entity: the headers that describe its content, and the
// already encoded bytes of it.
type part struct {
	header textproto.MIMEHeader
	body   []byte
}

// buildBody picks the simplest structure that carries everything msg holds: a
// single part when there is one body and no files, multipart/alternative for
// text plus HTML, and multipart/mixed once files are involved.
func buildBody(msg Outbound) (part, error) {
	var bodies []part
	if msg.Text != "" {
		p, err := textPart("text/plain", msg.Text)
		if err != nil {
			return part{}, err
		}
		bodies = append(bodies, p)
	}
	if msg.HTML != "" {
		p, err := textPart("text/html", msg.HTML)
		if err != nil {
			return part{}, err
		}
		bodies = append(bodies, p)
	}

	content := bodies[0]
	if len(bodies) > 1 {
		var err error
		content, err = multipartPart("alternative", bodies)
		if err != nil {
			return part{}, err
		}
	}
	if len(msg.Attachments) == 0 {
		return content, nil
	}

	parts := []part{content}
	for _, a := range msg.Attachments {
		p, err := attachmentPart(a)
		if err != nil {
			return part{}, err
		}
		parts = append(parts, p)
	}
	return multipartPart("mixed", parts)
}

// textPart encodes a body as quoted-printable, which keeps non-ASCII text and
// long lines intact without inflating the message the way base64 would.
func textPart(mimeType, body string) (part, error) {
	var buf bytes.Buffer
	w := quotedprintable.NewWriter(&buf)
	if _, err := io.WriteString(w, body); err != nil {
		return part{}, fmt.Errorf("encode %s part: %w", mimeType, err)
	}
	if err := w.Close(); err != nil {
		return part{}, fmt.Errorf("encode %s part: %w", mimeType, err)
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Type", mimeType+`; charset="utf-8"`)
	header.Set("Content-Transfer-Encoding", "quoted-printable")
	return part{header, buf.Bytes()}, nil
}

func attachmentPart(a Attachment) (part, error) {
	if hasCRLF(a.FileName) || hasCRLF(a.MimeType) || hasCRLF(a.ContentID) {
		return part{}, fmt.Errorf("mailer: attachment %q contains a newline", a.FileName)
	}
	mimeType := a.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Type", mime.FormatMediaType(mimeType, map[string]string{"name": a.FileName}))
	header.Set("Content-Transfer-Encoding", "base64")
	disposition := "attachment"
	if a.ContentID != "" {
		disposition = "inline"
		header.Set("Content-ID", "<"+a.ContentID+">")
	}
	header.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": a.FileName}))
	return part{header, wrapBase64(a.Content)}, nil
}

func multipartPart(subtype string, parts []part) (part, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		pw, err := w.CreatePart(p.header)
		if err != nil {
			return part{}, fmt.Errorf("create %s part: %w", subtype, err)
		}
		if _, err := pw.Write(p.body); err != nil {
			return part{}, fmt.Errorf("write %s part: %w", subtype, err)
		}
	}
	if err := w.Close(); err != nil {
		return part{}, fmt.Errorf("close %s part: %w", subtype, err)
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Type", mime.FormatMediaType("multipart/"+subtype, map[string]string{"boundary": w.Boundary()}))
	return part{header, buf.Bytes()}, nil
}

// wrapBase64 encodes data and breaks it into the 76 character lines RFC 2045
// asks for.
func wrapBase64(data []byte) []byte {
	const lineLen = 76
	encoded := base64.StdEncoding.EncodeToString(data)
	var buf bytes.Buffer
	for len(encoded) > lineLen {
		buf.WriteString(encoded[:lineLen])
		buf.WriteString("\r\n")
		encoded = encoded[lineLen:]
	}
	buf.WriteString(encoded)
	buf.WriteString("\r\n")
	return buf.Bytes()
}

func writeHeader(buf *bytes.Buffer, name, value string) {
	buf.WriteString(name)
	buf.WriteString(": ")
	buf.WriteString(value)
	buf.WriteString("\r\n")
}

func sortedKeys(header textproto.MIMEHeader) []string {
	names := make([]string, 0, len(header))
	for name := range header {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// formatAddress renders one address. A bare address is left bare rather than
// wrapped in angle brackets, because that is what a delivery agent expects for
// the envelope sender and it is equally valid in a header.
func formatAddress(a Address) string {
	if a.Name == "" {
		return a.Email
	}
	return (&mail.Address{Name: a.Name, Address: a.Email}).String()
}

func formatAddresses(addresses []Address) string {
	parts := make([]string, 0, len(addresses))
	for _, a := range addresses {
		parts = append(parts, formatAddress(a))
	}
	return strings.Join(parts, ", ")
}
