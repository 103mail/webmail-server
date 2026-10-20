package federation

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"slices"
	"strings"
	"testing"
	"time"
)

func validEnvelope() Envelope {
	return Envelope{
		Version:      Version,
		EnvelopeID:   "018f4d2b-9f21-7890-abcd-ef0123456789",
		OriginDomain: "103mail.com",
		OriginSentAt: FormatTimestamp(time.Now()),
		Thread: Thread{
			FederationID: "018f4d2b-9f21-7890-abcd-ef0123456790",
			Subject:      "Hello",
		},
		Message: Message{
			FederationID: "018f4d2b-9f21-7890-abcd-ef0123456791",
			SenderEmail:  "alice@103mail.com",
			SenderName:   "Alice",
			Body:         "text",
			SentAt:       FormatTimestamp(time.Now()),
		},
		Attachments: []Attachment{},
		Recipients:  []string{"bob@example.com"},
	}
}

func attachmentOf(data []byte) Attachment {
	sum := sha256.Sum256(data)
	return Attachment{
		FederationID: "018f4d2b-9f21-7890-abcd-ef0123456792",
		FileName:     "report.pdf",
		MimeType:     "application/pdf",
		FileSize:     int64(len(data)),
		HashSHA256:   hex.EncodeToString(sum[:]),
		Data:         base64.StdEncoding.EncodeToString(data),
	}
}

// Given a well-formed envelope, when it is validated, then it is accepted.
func TestValidateAcceptsAWellFormedEnvelope(t *testing.T) {
	e := validEnvelope()
	if r := e.Validate(DefaultMaxAttachmentBytes); r != nil {
		t.Fatalf("a valid envelope was rejected: %v", r)
	}
}

// Every field rule in RFC 5.3 has to actually reject, or a peer could send us
// something the rest of the code does not expect.
func TestValidateEnforcesFieldRules(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Envelope)
		status int
		code   string
	}{
		{"an unknown version", func(e *Envelope) { e.Version = "0.9" }, 400, ErrUnsupportedVersion},
		{"an envelope id that is not a uuid", func(e *Envelope) { e.EnvelopeID = "nope" }, 400, ErrSchemaViolation},
		{"an uppercased origin domain", func(e *Envelope) { e.OriginDomain = "103Mail.com" }, 400, ErrSchemaViolation},
		{"an origin timestamp that is not RFC 3339", func(e *Envelope) { e.OriginSentAt = "yesterday" }, 400, ErrSchemaViolation},
		{"a local origin timestamp", func(e *Envelope) { e.OriginSentAt = "2026-04-23T10:15:00+02:00" }, 400, ErrSchemaViolation},
		{"a thread id that is not a uuid", func(e *Envelope) { e.Thread.FederationID = "x" }, 400, ErrSchemaViolation},
		{"an oversized subject", func(e *Envelope) { e.Thread.Subject = strings.Repeat("x", 999) }, 400, ErrSchemaViolation},
		{"an in_reply_to that is not a uuid", func(e *Envelope) { v := "x"; e.Thread.InReplyTo = &v }, 400, ErrSchemaViolation},
		{"a sender that is not an address", func(e *Envelope) { e.Message.SenderEmail = "alice" }, 400, ErrSchemaViolation},
		{"an oversized sender name", func(e *Envelope) { e.Message.SenderName = strings.Repeat("x", 256) }, 400, ErrSchemaViolation},
		{"an oversized body", func(e *Envelope) { e.Message.Body = strings.Repeat("x", maxBodyBytes+1) }, 400, ErrSchemaViolation},
		{"no recipients", func(e *Envelope) { e.Recipients = nil }, 400, ErrSchemaViolation},
		{"too many recipients", func(e *Envelope) {
			for i := 0; i <= maxRecipients; i++ {
				e.Recipients = append(e.Recipients, "bob@example.com")
			}
		}, 400, ErrSchemaViolation},
		{"a recipient that is not an address", func(e *Envelope) { e.Recipients = []string{"bob"} }, 400, ErrSchemaViolation},
		{"too many members", func(e *Envelope) {
			e.Thread.Members = []string{"alice@103mail.com", "bob@example.com"}
			for i := 0; i < maxMembers; i++ {
				e.Thread.Members = append(e.Thread.Members, "carol@example.org")
			}
		}, 400, ErrSchemaViolation},
		{"a member that is not an address", func(e *Envelope) {
			e.Thread.Members = []string{"alice@103mail.com", "bob@example.com", "erin"}
		}, 400, ErrSchemaViolation},
		{"members without the sender", func(e *Envelope) {
			e.Thread.Members = []string{"bob@example.com"}
		}, 400, ErrSchemaViolation},
		{"members without a recipient", func(e *Envelope) {
			e.Thread.Members = []string{"alice@103mail.com", "erin@mail.acme.test"}
		}, 400, ErrSchemaViolation},
		{"a refused address that is not an address", func(e *Envelope) {
			e.Thread.Refused = []string{"nobody"}
		}, 400, ErrSchemaViolation},
		{"a refused address that is also a member", func(e *Envelope) {
			e.Thread.Members = []string{"alice@103mail.com", "bob@example.com", "nobody@example.com"}
			e.Thread.Refused = []string{"nobody@example.com"}
		}, 400, ErrSchemaViolation},
		{"too many refused addresses", func(e *Envelope) {
			for i := 0; i <= maxRecipients; i++ {
				e.Thread.Refused = append(e.Thread.Refused, "nobody@example.com")
			}
		}, 400, ErrSchemaViolation},
		{"too many attachments", func(e *Envelope) {
			for i := 0; i <= maxAttachments; i++ {
				e.Attachments = append(e.Attachments, attachmentOf([]byte("x")))
			}
		}, 400, ErrSchemaViolation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := validEnvelope()
			tt.mutate(&e)
			r := e.Validate(DefaultMaxAttachmentBytes)
			if r == nil {
				t.Fatalf("%s was accepted", tt.name)
			}
			if r.Status != tt.status || r.Code != tt.code {
				t.Errorf("got %d %s, want %d %s", r.Status, r.Code, tt.status, tt.code)
			}
		})
	}
}

// Members name everyone on the thread, on any Domain, and fall back to the
// sender and the recipients when a peer leaves them out (RFC 5.3).
func TestMembersOfAThread(t *testing.T) {
	e := validEnvelope()
	if got := e.Members(); strings.Join(got, ",") != "alice@103mail.com,bob@example.com" {
		t.Errorf("Members without thread.members = %v", got)
	}
	e.Thread.Members = []string{"Alice@103mail.com", "bob@example.com", "erin@mail.acme.test"}
	if r := e.Validate(DefaultMaxAttachmentBytes); r != nil {
		t.Fatalf("members on three Domains were rejected: %v", r)
	}
	if got := e.Members(); strings.Join(got, ",") != "alice@103mail.com,bob@example.com,erin@mail.acme.test" {
		t.Errorf("Members = %v", got)
	}
}

// A refusal notice comes from mailer-daemon, which is never a member: its
// members leave it out, and it is not added back (RFC 6.5).
func TestMailerDaemonIsNeverAMember(t *testing.T) {
	e := validEnvelope()
	e.Message.SenderEmail = "mailer-daemon@103mail.com"
	e.Thread.Members = []string{"alice@103mail.com", "bob@example.com"}
	e.Thread.Refused = []string{"nobody@103mail.com"}
	if r := e.Validate(DefaultMaxAttachmentBytes); r != nil {
		t.Fatalf("a notice without mailer-daemon among its members was rejected: %v", r)
	}
	e.Thread.Members = nil
	if got := e.Members(); slices.Contains(got, "mailer-daemon@103mail.com") {
		t.Errorf("Members = %v, includes mailer-daemon", got)
	}
}

// Recipients name only the Domain the envelope was sent to (RFC 5.3).
func TestRecipientsMustBeOnTheDestination(t *testing.T) {
	e := validEnvelope()
	if r := e.RecipientsOn("example.com"); r != nil {
		t.Errorf("recipients on the Destination were refused: %v", r)
	}
	e.Recipients = []string{"bob@example.com", "erin@mail.acme.test"}
	if r := e.RecipientsOn("example.com"); r == nil || r.Status != 400 || r.Code != ErrNotRegistrar {
		t.Errorf("a recipient on another Domain was accepted: %v", r)
	}
	// A parent is not its subdomain (RFC 4.1.1).
	e.Recipients = []string{"pete@acme.test"}
	if r := e.RecipientsOn("mail.acme.test"); r == nil {
		t.Error("a recipient on the parent domain was accepted")
	}
}

// Given an attachment whose declared size or hash disagrees with its bytes,
// when it is validated, then it is rejected as a mismatch rather than stored.
func TestAttachmentBytesAreCheckedAgainstTheirClaims(t *testing.T) {
	data := []byte("the actual bytes")

	t.Run("a lying file_size", func(t *testing.T) {
		a := attachmentOf(data)
		a.FileSize = 999
		if _, r := a.Bytes(); r == nil || r.Code != ErrAttachmentMismatch {
			t.Fatalf("got %v, want attachment_mismatch", r)
		}
	})

	t.Run("a lying hash", func(t *testing.T) {
		a := attachmentOf(data)
		a.HashSHA256 = strings.Repeat("0", 64)
		if _, r := a.Bytes(); r == nil || r.Code != ErrAttachmentMismatch {
			t.Fatalf("got %v, want attachment_mismatch", r)
		}
	})

	t.Run("data that is not base64", func(t *testing.T) {
		a := attachmentOf(data)
		a.Data = "!!!not base64!!!"
		if _, r := a.Bytes(); r == nil || r.Code != ErrSchemaViolation {
			t.Fatalf("got %v, want schema_violation", r)
		}
	})

	t.Run("honest bytes", func(t *testing.T) {
		a := attachmentOf(data)
		got, r := a.Bytes()
		if r != nil {
			t.Fatalf("a matching attachment was rejected: %v", r)
		}
		if string(got) != string(data) {
			t.Errorf("bytes = %q, want %q", got, data)
		}
	})
}

// An attachment over the advertised cap is 413, not a schema violation: the
// peer sent something well formed that this Domain will not hold.
func TestOversizedAttachmentIsTooLarge(t *testing.T) {
	e := validEnvelope()
	e.Attachments = []Attachment{attachmentOf([]byte("0123456789"))}
	r := e.Validate(4)
	if r == nil || r.Status != 413 || r.Code != ErrTooLarge {
		t.Fatalf("got %v, want 413 too_large", r)
	}
}

func TestTimestampFormatIsMillisecondUTC(t *testing.T) {
	v := FormatTimestamp(time.Date(2026, 4, 23, 10, 15, 0, 123456789, time.UTC))
	if v != "2026-04-23T10:15:00.123Z" {
		t.Errorf("FormatTimestamp = %q", v)
	}
	parsed, err := ParseTimestamp(v)
	if err != nil {
		t.Fatalf("ParseTimestamp: %v", err)
	}
	if parsed.Year() != 2026 {
		t.Errorf("round trip = %v", parsed)
	}
}

func TestDomainOf(t *testing.T) {
	tests := map[string]string{
		"alice@103mail.com":     "103mail.com",
		"alice@103MAIL.com":     "103mail.com",
		"a.b+c@mail.example.co": "mail.example.co",
		"nodomain":              "",
	}
	for address, want := range tests {
		if got := DomainOf(address); got != want {
			t.Errorf("DomainOf(%q) = %q, want %q", address, got, want)
		}
	}
}

func TestHostIsOneLabelDeeperThanTheDomain(t *testing.T) {
	// RFC 4.1.1: the label is prepended to the full DNS domain, never hoisted
	// to the registrable parent.
	if got := Host("mail.example.com"); got != "webmail.mail.example.com" {
		t.Errorf("Host = %q", got)
	}
	if got := BaseURL("example.com"); got != "https://webmail.example.com" {
		t.Errorf("BaseURL = %q", got)
	}
}

func TestDescriptorValidation(t *testing.T) {
	key := "ed25519:" + base64.StdEncoding.EncodeToString(make([]byte, 32))

	t.Run("a descriptor for another domain", func(t *testing.T) {
		d := Descriptor{Domain: "evil.com", PublicKey: key, Versions: []string{Version}}
		if err := d.Validate("example.com"); err == nil {
			t.Error("a descriptor claiming another domain was accepted")
		}
	})

	t.Run("no versions", func(t *testing.T) {
		d := Descriptor{Domain: "example.com", PublicKey: key}
		if err := d.Validate("example.com"); err == nil {
			t.Error("a descriptor advertising no version was accepted")
		}
	})

	t.Run("more than four rotation keys", func(t *testing.T) {
		d := Descriptor{Domain: "example.com", PublicKey: key, Versions: []string{Version},
			PreviousPublicKeys: []string{key, key, key, key, key}}
		if err := d.Validate("example.com"); err == nil {
			t.Error("a descriptor with five previous keys was accepted")
		}
	})

	t.Run("a valid descriptor", func(t *testing.T) {
		d := Descriptor{Domain: "example.com", PublicKey: key, Versions: []string{Version}}
		if err := d.Validate("EXAMPLE.com"); err != nil {
			t.Errorf("a valid descriptor was rejected: %v", err)
		}
		if !d.Speaks(Version) || d.Speaks("9.9") {
			t.Error("Speaks disagrees with the advertised versions")
		}
		if d.EnvelopeCap() != DefaultMaxEnvelopeBytes || d.AttachmentCap() != DefaultMaxAttachmentBytes {
			t.Error("absent caps did not fall back to the RFC defaults")
		}
	})
}

func TestUUIDShape(t *testing.T) {
	id, err := NewUUID()
	if err != nil {
		t.Fatalf("NewUUID: %v", err)
	}
	if !validUUID(id) {
		t.Fatalf("NewUUID produced %q, which does not validate", id)
	}
	// Version 7 in the high nibble of the third group.
	if id[14] != '7' {
		t.Errorf("NewUUID produced version %c, want 7", id[14])
	}
	for _, bad := range []string{"", "not-a-uuid", strings.ToUpper(id), id + "x"} {
		if validUUID(bad) {
			t.Errorf("validUUID(%q) accepted", bad)
		}
	}
}

// UUIDv7 exists so that identifiers sort by creation time; if they did not, the
// indexes they sit in would fragment.
func TestUUIDsSortByTime(t *testing.T) {
	first, _ := NewUUID()
	time.Sleep(2 * time.Millisecond)
	second, _ := NewUUID()
	if !(first < second) {
		t.Errorf("%q should sort before %q", first, second)
	}
}
