package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/mevdschee/webmail-server/internal/config"
	"github.com/mevdschee/webmail-server/internal/mailer"
)

func TestRecipientsOfPrefersEnvelopeAndDeduplicates(t *testing.T) {
	in := &mailer.Inbound{
		Recipients: []string{"a@example.test"},
		To:         []mailer.Address{{Email: "A@example.test"}, {Email: "b@example.test"}},
		Cc:         []mailer.Address{{Email: "c@example.test"}, {Email: "b@example.test"}},
	}
	got := recipientsOf(in)
	want := []string{"a@example.test", "b@example.test", "c@example.test"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("recipientsOf = %v, want %v", got, want)
	}
}

func TestRecipientsOfDecodesObfuscatedAddresses(t *testing.T) {
	in := &mailer.Inbound{Recipients: []string{"sales&#64;example.test"}}
	got := recipientsOf(in)
	if len(got) != 1 || got[0] != "sales_example.test" {
		t.Errorf("recipientsOf = %v", got)
	}
}

func TestRecipientsOfIgnoresBlanks(t *testing.T) {
	in := &mailer.Inbound{Recipients: []string{"", "  "}, To: []mailer.Address{{Email: ""}}}
	if got := recipientsOf(in); len(got) != 0 {
		t.Errorf("recipientsOf = %v, want none", got)
	}
}

func TestHTMLEscape(t *testing.T) {
	if got, want := htmlEscape(`a <b> & "c"`), `a &lt;b&gt; &amp; "c"`; got != want {
		t.Errorf("htmlEscape = %q, want %q", got, want)
	}
}

// newInboundServer returns a server whose inbound webhook is wired to a
// verifier that accepts nothing but a properly signed AWS post.
func newInboundServer(t *testing.T) *Server {
	t.Helper()
	verifier := mailer.NewVerifier(nil, "arn:aws:sns:eu-west-1:1:inbound")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(&config.Config{MaxBodyBytes: 1 << 20}, log, nil, nil, nil, nil,
		mailer.NewReceiver(verifier, nil), verifier, nil)
}

func postInbound(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleInboundMail(w, httptest.NewRequest(http.MethodPost, "/api/v1/mail/inbound", strings.NewReader(body)))
	return w
}

// Given a request that is not a POST, when it reaches the inbound webhook,
// then it is refused: SNS only ever posts.
func TestInboundMailRejectsNonPost(t *testing.T) {
	w := httptest.NewRecorder()
	newInboundServer(t).handleInboundMail(w, httptest.NewRequest(http.MethodGet, "/api/v1/mail/inbound", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

// Given a server without SES configured, when mail is posted, then it reports
// that it is unavailable rather than accepting a post it cannot authenticate.
func TestInboundMailReportsUnconfigured(t *testing.T) {
	s := New(&config.Config{MaxBodyBytes: 1 << 20}, slog.New(slog.NewTextHandler(io.Discard, nil)),
		nil, nil, nil, nil, nil, nil, nil)
	if w := postInbound(t, s, `{"Type":"Notification"}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

// Given a body that is not an SNS envelope, when it is posted, then it is
// refused with 400: retrying will not make it one.
func TestInboundMailRejectsUnparseableBody(t *testing.T) {
	if w := postInbound(t, newInboundServer(t), "not json"); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// Given a post that does not verify, when it is received, then it is answered
// with 403 rather than a 5xx, so SNS does not retry a forgery.
func TestInboundMailRejectsUnverifiedPost(t *testing.T) {
	body := `{"Type":"Notification","MessageId":"1","TopicArn":"arn:aws:sns:eu-west-1:1:inbound",` +
		`"Message":"{}","Timestamp":"2026-08-24T00:00:00.000Z","SignatureVersion":"1",` +
		`"Signature":"AAAA","SigningCertURL":"https://example.test/cert.pem"}`
	if w := postInbound(t, newInboundServer(t), body); w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

// Given a verified post from a topic we do not accept, when it is received,
// then it is refused for the same reason.
func TestInboundMailRejectsUnknownTopic(t *testing.T) {
	body := `{"Type":"Notification","MessageId":"1","TopicArn":"arn:aws:sns:eu-west-1:1:other",` +
		`"Message":"{}","Timestamp":"2026-08-24T00:00:00.000Z","SignatureVersion":"1",` +
		`"Signature":"AAAA","SigningCertURL":"https://sns.eu-west-1.amazonaws.com/cert.pem"}`
	if w := postInbound(t, newInboundServer(t), body); w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}
