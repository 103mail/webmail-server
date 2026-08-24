// Package mailer moves email between this system and the outside world. It
// owns transport only: what a message says is decided by the domain packages,
// and per ADR 4 outbound mail never carries thread content.
//
// The two directions use different services (ADR 15). Outbound is published to
// an SNS topic as a notification this package renders; delivering it is the
// subscriber's job. Inbound comes from Amazon SES: a receipt rule writes the
// raw message to S3 and publishes an SNS notification, and Receiver turns that
// notification back into an Inbound.
//
// Nothing here talks to the database or to HTTP request handling. The Sender
// and ObjectGetter interfaces exist so that tests, and a dry-run deployment,
// can run the whole path without an AWS account.
package mailer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Address is one mailbox on an email envelope. Name may be empty.
type Address struct {
	Name  string
	Email string
}

// Attachment is one file carried by an email.
type Attachment struct {
	FileName string
	MimeType string
	Content  []byte
	// ContentID is the value an HTML body references with cid:. It is empty
	// for an ordinary attachment.
	ContentID string
}

// Outbound is an email this system sends.
//
// Bcc is carried as its own field rather than as a header, and BuildMIME never
// writes a Bcc header, so no recipient learns who else received the message.
type Outbound struct {
	From        Address
	To          []Address
	Cc          []Address
	Bcc         []Address
	ReplyTo     []Address
	Subject     string
	Text        string
	HTML        string
	Attachments []Attachment
	// Headers are extra RFC 5322 headers, for example In-Reply-To. Names and
	// values may not contain CR or LF.
	Headers map[string]string
	// Date is the Date header. The zero value means the time of sending.
	Date time.Time
}

// Inbound is an email this system received.
type Inbound struct {
	// ProviderID is the id SES assigned the message. It is the idempotency
	// key: SNS delivers at least once, so the same message can arrive twice.
	ProviderID string
	// MessageID is the RFC 5322 Message-ID header without its angle brackets.
	MessageID  string
	InReplyTo  string
	References []string
	From       Address
	To         []Address
	Cc         []Address
	Subject    string
	Date       time.Time
	// Recipients are the envelope addresses SES accepted the message for,
	// which is what decides the mailbox it belongs in. They are not always
	// the same as the To header.
	Recipients  []string
	Text        string
	HTML        string
	Attachments []Attachment
	Verdicts    Verdicts
	Raw         []byte
}

// Verdict is the outcome of one check SES ran on an inbound message.
type Verdict string

// The statuses SES reports. An unrun check is the empty string.
const (
	VerdictPass             Verdict = "PASS"
	VerdictFail             Verdict = "FAIL"
	VerdictGray             Verdict = "GRAY"
	VerdictProcessingFailed Verdict = "PROCESSING_FAILED"
	VerdictDisabled         Verdict = "DISABLED"
)

// Verdicts are the checks SES ran before handing a message over.
type Verdicts struct {
	SPF   Verdict
	DKIM  Verdict
	DMARC Verdict
	Spam  Verdict
	Virus Verdict
}

// Failed lists the checks that came back FAIL, in a fixed order. What to do
// about them is the caller's decision: a DMARC failure is a reason to
// quarantine, a virus hit is a reason to drop the message entirely.
func (v Verdicts) Failed() []string {
	checks := []struct {
		name    string
		verdict Verdict
	}{
		{"SPF", v.SPF},
		{"DKIM", v.DKIM},
		{"DMARC", v.DMARC},
		{"spam", v.Spam},
		{"virus", v.Virus},
	}
	var failed []string
	for _, c := range checks {
		if c.verdict == VerdictFail {
			failed = append(failed, c.name)
		}
	}
	return failed
}

// Sender hands one message to the outside world and returns the provider's id
// for it. Topic publishes to SNS, Recorder keeps it in memory.
// Implementations must be safe for concurrent use.
type Sender interface {
	Send(ctx context.Context, msg Outbound) (string, error)
}

// ObjectGetter reads the object an SES receipt rule wrote. S3Objects
// implements it against S3; tests use a map.
type ObjectGetter interface {
	GetObject(ctx context.Context, bucket, key string) ([]byte, error)
}

// ErrNotMail reports an SNS message that is not inbound mail, such as a
// subscription confirmation. It is not a failure: the caller handles those
// separately, see Verifier.Confirm.
var ErrNotMail = errors.New("mailer: not an inbound mail notification")

// Validate reports whether msg can be rendered and sent. It is deliberately
// strict about CR and LF, because a newline that reaches a header is a header
// injection.
func (msg Outbound) Validate() error {
	if err := validateAddress("from", msg.From); err != nil {
		return err
	}
	if len(msg.To)+len(msg.Cc)+len(msg.Bcc) == 0 {
		return errors.New("mailer: message has no recipients")
	}
	for _, group := range []struct {
		field     string
		addresses []Address
	}{
		{"to", msg.To}, {"cc", msg.Cc}, {"bcc", msg.Bcc}, {"reply-to", msg.ReplyTo},
	} {
		for _, a := range group.addresses {
			if err := validateAddress(group.field, a); err != nil {
				return err
			}
		}
	}
	if msg.Text == "" && msg.HTML == "" {
		return errors.New("mailer: message has no body")
	}
	for name, value := range msg.Headers {
		if name == "" {
			return errors.New("mailer: message has an empty header name")
		}
		if hasCRLF(name) || hasCRLF(value) {
			return fmt.Errorf("mailer: header %q contains a newline", name)
		}
	}
	return nil
}

func validateAddress(field string, a Address) error {
	if a.Email == "" {
		return fmt.Errorf("mailer: %s address is empty", field)
	}
	if hasCRLF(a.Email) || hasCRLF(a.Name) {
		return fmt.Errorf("mailer: %s address %q contains a newline", field, a.Email)
	}
	if !strings.Contains(a.Email, "@") {
		return fmt.Errorf("mailer: %s address %q has no domain", field, a.Email)
	}
	return nil
}

func hasCRLF(s string) bool {
	return strings.ContainsAny(s, "\r\n")
}

// Recorder is a Sender that keeps messages in memory instead of publishing
// them. Tests use it in place of the SNS topic, and so does a deployment
// configured without a topic ARN, which is how a staging environment avoids
// mailing real people.
type Recorder struct {
	mu   sync.Mutex
	sent []Outbound
	err  error
}

// NewRecorder returns an empty Recorder.
func NewRecorder() *Recorder {
	return &Recorder{}
}

// Send validates msg and stores it. The returned id counts from one.
func (r *Recorder) Send(ctx context.Context, msg Outbound) (string, error) {
	if err := msg.Validate(); err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return "", r.err
	}
	r.sent = append(r.sent, msg)
	return fmt.Sprintf("recorded-%d", len(r.sent)), nil
}

// Sent returns a copy of the messages recorded so far, oldest first.
func (r *Recorder) Sent() []Outbound {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Outbound(nil), r.sent...)
}

// FailWith makes every later Send return err. Passing nil restores normal
// behaviour. Validation still runs first, so a malformed message reports its
// own error rather than err.
func (r *Recorder) FailWith(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

// Reset discards the recorded messages and clears any failure.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = nil
	r.err = nil
}
