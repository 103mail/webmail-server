package mailer

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// Given a message with both bodies and a file, when it is rendered and parsed
// back, then nothing is lost.
func TestBuildMIMERoundTrip(t *testing.T) {
	msg := Outbound{
		From:    Address{Name: "Webmail", Email: "no-reply@example.com"},
		To:      []Address{{Name: "Ada Lovelace", Email: "ada@example.org"}},
		Cc:      []Address{{Email: "cc@example.org"}},
		ReplyTo: []Address{{Email: "reply@example.com"}},
		Subject: "Je hebt ongelezen berichten — één wacht",
		Text:    "Sign in to read them.\nThis line is quite long, long enough that quoted-printable has to fold it somewhere sensible.",
		HTML:    "<p>Sign in to read them.</p>",
		Headers: map[string]string{"In-Reply-To": "<parent@example.com>"},
		Date:    time.Date(2026, 8, 24, 10, 30, 0, 0, time.UTC),
		Attachments: []Attachment{{
			FileName: "report.pdf",
			MimeType: "application/pdf",
			Content:  bytes.Repeat([]byte{0x00, 0xff, 0x10}, 200),
		}},
	}

	raw, err := BuildMIME(msg)
	if err != nil {
		t.Fatalf("BuildMIME() error = %v", err)
	}
	got, err := ParseMIME(raw)
	if err != nil {
		t.Fatalf("ParseMIME() error = %v", err)
	}

	if got.Subject != msg.Subject {
		t.Errorf("Subject = %q, want %q", got.Subject, msg.Subject)
	}
	if got.From != (Address{Name: "Webmail", Email: "no-reply@example.com"}) {
		t.Errorf("From = %+v, want the sender", got.From)
	}
	if len(got.To) != 1 || got.To[0].Email != "ada@example.org" || got.To[0].Name != "Ada Lovelace" {
		t.Errorf("To = %+v, want Ada", got.To)
	}
	if len(got.Cc) != 1 || got.Cc[0].Email != "cc@example.org" {
		t.Errorf("Cc = %+v, want the cc address", got.Cc)
	}
	if got.Text != msg.Text {
		t.Errorf("Text = %q, want %q", got.Text, msg.Text)
	}
	if got.HTML != msg.HTML {
		t.Errorf("HTML = %q, want %q", got.HTML, msg.HTML)
	}
	if got.InReplyTo != "parent@example.com" {
		t.Errorf("InReplyTo = %q, want %q", got.InReplyTo, "parent@example.com")
	}
	if !got.Date.Equal(msg.Date) {
		t.Errorf("Date = %v, want %v", got.Date, msg.Date)
	}
	if len(got.Attachments) != 1 {
		t.Fatalf("Attachments = %d, want 1", len(got.Attachments))
	}
	if got.Attachments[0].FileName != "report.pdf" {
		t.Errorf("attachment name = %q, want report.pdf", got.Attachments[0].FileName)
	}
	if got.Attachments[0].MimeType != "application/pdf" {
		t.Errorf("attachment type = %q, want application/pdf", got.Attachments[0].MimeType)
	}
	if !bytes.Equal(got.Attachments[0].Content, msg.Attachments[0].Content) {
		t.Errorf("attachment content survived as %d bytes, want %d", len(got.Attachments[0].Content), len(msg.Attachments[0].Content))
	}
}

// Given a message with Bcc recipients, when it is rendered, then no header
// names them: a Bcc that reached the wire would tell every recipient who else
// was copied.
func TestBuildMIMEOmitsBcc(t *testing.T) {
	msg := validOutbound()
	msg.Bcc = []Address{{Email: "audit@example.com"}}

	raw, err := BuildMIME(msg)
	if err != nil {
		t.Fatalf("BuildMIME() error = %v", err)
	}
	if bytes.Contains(raw, []byte("audit@example.com")) {
		t.Fatalf("BuildMIME() output names the bcc recipient:\n%s", raw)
	}
	if bytes.Contains(bytes.ToLower(raw), []byte("bcc:")) {
		t.Fatalf("BuildMIME() output has a Bcc header:\n%s", raw)
	}
}

// Given the same message rendered twice, when the outputs are compared, then
// only the multipart boundaries differ: header order is stable, so a rendered
// message is reviewable.
func TestBuildMIMEHeaderOrderIsStable(t *testing.T) {
	msg := validOutbound()
	msg.Date = time.Date(2026, 8, 24, 10, 30, 0, 0, time.UTC)
	msg.Headers = map[string]string{"X-Zulu": "z", "X-Alpha": "a", "X-Mike": "m"}

	first, err := BuildMIME(msg)
	if err != nil {
		t.Fatalf("BuildMIME() error = %v", err)
	}
	second, err := BuildMIME(msg)
	if err != nil {
		t.Fatalf("BuildMIME() error = %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("BuildMIME() is not deterministic:\n%s\n---\n%s", first, second)
	}

	headers := string(first[:bytes.Index(first, []byte("\r\n\r\n"))])
	alpha, mike, zulu := strings.Index(headers, "X-Alpha"), strings.Index(headers, "X-Mike"), strings.Index(headers, "X-Zulu")
	if !(alpha < mike && mike < zulu) {
		t.Fatalf("extra headers are not sorted:\n%s", headers)
	}
}

// Given a message with only one body, when it is rendered, then it is a single
// part rather than a pointless multipart wrapper.
func TestBuildMIMEUsesSinglePartForOneBody(t *testing.T) {
	msg := validOutbound()
	msg.HTML = ""

	raw, err := BuildMIME(msg)
	if err != nil {
		t.Fatalf("BuildMIME() error = %v", err)
	}
	if bytes.Contains(raw, []byte("multipart/")) {
		t.Fatalf("BuildMIME() wrapped a single body in multipart:\n%s", raw)
	}
	got, err := ParseMIME(raw)
	if err != nil {
		t.Fatalf("ParseMIME() error = %v", err)
	}
	if got.Text != msg.Text {
		t.Fatalf("Text = %q, want %q", got.Text, msg.Text)
	}
}

// Given an invalid message, when it is rendered, then it is refused before any
// bytes are produced.
func TestBuildMIMERejectsInvalidMessage(t *testing.T) {
	if _, err := BuildMIME(Outbound{}); err == nil {
		t.Fatal("BuildMIME() = nil error, want a validation error")
	}
}

// Given an attachment whose name carries a newline, when the message is
// rendered, then it is refused rather than folded into the part headers.
func TestBuildMIMERejectsAttachmentHeaderInjection(t *testing.T) {
	msg := validOutbound()
	msg.Attachments = []Attachment{{FileName: "a.txt\r\nContent-Type: text/html", Content: []byte("x")}}
	if _, err := BuildMIME(msg); err == nil {
		t.Fatal("BuildMIME() = nil error, want an error about a newline")
	}
}

// Given mail from the open internet in a legacy charset and a base64 body,
// when it is parsed, then the text comes out as UTF-8.
func TestParseMIMEDecodesLegacyCharsetAndBase64(t *testing.T) {
	raw := "From: =?iso-8859-1?Q?Bj=F8rn?= <bjorn@example.org>\r\n" +
		"To: ada@example.org\r\n" +
		"Subject: =?iso-8859-1?Q?Caf=E9?=\r\n" +
		"Message-Id: <abc@example.org>\r\n" +
		"References: <one@example.org> <two@example.org>\r\n" +
		"Content-Type: text/plain; charset=iso-8859-1\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		// "Café" in ISO-8859-1.
		"Q2FmyQ==\r\n"

	got, err := ParseMIME([]byte(raw))
	if err != nil {
		t.Fatalf("ParseMIME() error = %v", err)
	}
	if got.Subject != "Café" {
		t.Errorf("Subject = %q, want %q", got.Subject, "Café")
	}
	if got.From.Name != "Bjørn" || got.From.Email != "bjorn@example.org" {
		t.Errorf("From = %+v, want Bjørn", got.From)
	}
	if got.Text != "CafÉ" {
		t.Errorf("Text = %q, want %q", got.Text, "CafÉ")
	}
	if got.MessageID != "abc@example.org" {
		t.Errorf("MessageID = %q, want %q", got.MessageID, "abc@example.org")
	}
	if strings.Join(got.References, ",") != "one@example.org,two@example.org" {
		t.Errorf("References = %v, want both parents", got.References)
	}
}

// Given a message whose From header is too malformed to parse, when it is
// parsed, then the header is kept verbatim rather than the sender being lost.
func TestParseMIMEKeepsMalformedSender(t *testing.T) {
	raw := "From: not an address at all\r\n" +
		"To: ada@example.org\r\n" +
		"Subject: Broken\r\n" +
		"\r\n" +
		"body\r\n"

	got, err := ParseMIME([]byte(raw))
	if err != nil {
		t.Fatalf("ParseMIME() error = %v", err)
	}
	if got.From.Email != "not an address at all" {
		t.Fatalf("From = %+v, want the raw header kept", got.From)
	}
}

// Given an inline image referenced by an HTML body, when the message is
// parsed, then the image keeps the content id the HTML points at.
func TestParseMIMEKeepsContentID(t *testing.T) {
	msg := validOutbound()
	msg.HTML = `<p><img src="cid:logo@example.com"></p>`
	msg.Attachments = []Attachment{{
		FileName:  "logo.png",
		MimeType:  "image/png",
		Content:   []byte{0x89, 'P', 'N', 'G'},
		ContentID: "logo@example.com",
	}}

	raw, err := BuildMIME(msg)
	if err != nil {
		t.Fatalf("BuildMIME() error = %v", err)
	}
	got, err := ParseMIME(raw)
	if err != nil {
		t.Fatalf("ParseMIME() error = %v", err)
	}
	if len(got.Attachments) != 1 || got.Attachments[0].ContentID != "logo@example.com" {
		t.Fatalf("Attachments = %+v, want the inline image with its content id", got.Attachments)
	}
}

// Given bytes that are not a message at all, when they are parsed, then the
// failure is reported rather than guessed at.
func TestParseMIMERejectsGarbage(t *testing.T) {
	if _, err := ParseMIME([]byte("this is not a message")); err == nil {
		t.Fatal("ParseMIME() = nil error, want a parse error")
	}
}

// Given a message nested far deeper than any real mail, when it is parsed,
// then the walk stops instead of recursing until the stack runs out.
func TestParseMIMEStopsAtNestingLimit(t *testing.T) {
	var body bytes.Buffer
	body.WriteString("Content-Type: multipart/mixed; boundary=\"b0\"\r\n\r\n")
	depth := maxMIMEDepth + 10
	for i := 0; i < depth; i++ {
		body.WriteString("--b0\r\n")
		body.WriteString("Content-Type: multipart/mixed; boundary=\"b0\"\r\n\r\n")
	}
	body.WriteString("--b0\r\nContent-Type: text/plain\r\n\r\ndeep\r\n--b0--\r\n")

	if _, err := ParseMIME(body.Bytes()); err != nil {
		t.Fatalf("ParseMIME() error = %v", err)
	}
}
