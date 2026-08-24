package mailer

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func validOutbound() Outbound {
	return Outbound{
		From:    Address{Name: "Webmail", Email: "no-reply@example.com"},
		To:      []Address{{Name: "Ada", Email: "ada@example.org"}},
		Subject: "You have unread messages",
		Text:    "Sign in to read them.",
	}
}

// Given a message with a sender, a recipient and a body, when it is validated,
// then it is accepted.
func TestOutboundValidateAcceptsCompleteMessage(t *testing.T) {
	if err := validOutbound().Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

// Given a message missing something it cannot be sent without, when it is
// validated, then it is rejected.
func TestOutboundValidateRejectsIncompleteMessage(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Outbound)
		wantErr string
	}{
		{"no sender", func(m *Outbound) { m.From = Address{} }, "from address is empty"},
		{"sender without domain", func(m *Outbound) { m.From.Email = "no-reply" }, "has no domain"},
		{"no recipients", func(m *Outbound) { m.To = nil }, "no recipients"},
		{"recipient without domain", func(m *Outbound) { m.To[0].Email = "ada" }, "has no domain"},
		{"no body", func(m *Outbound) { m.Text, m.HTML = "", "" }, "no body"},
		{"empty header name", func(m *Outbound) { m.Headers = map[string]string{"": "x"} }, "empty header name"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			msg := validOutbound()
			test.mutate(&msg)
			err := msg.Validate()
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, test.wantErr)
			}
		})
	}
}

// Given a message carrying a newline in a header, an address or a name, when
// it is validated, then it is rejected: a newline that reached a header would
// let the caller inject headers of its own.
func TestOutboundValidateRejectsHeaderInjection(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Outbound)
	}{
		{"in the sender address", func(m *Outbound) { m.From.Email = "a@b.com\r\nBcc: eve@evil.com" }},
		{"in the sender name", func(m *Outbound) { m.From.Name = "Webmail\r\nBcc: eve@evil.com" }},
		{"in a recipient address", func(m *Outbound) { m.To[0].Email = "ada@b.com\nBcc: eve@evil.com" }},
		{"in a header name", func(m *Outbound) { m.Headers = map[string]string{"X-A\r\nBcc": "v"} }},
		{"in a header value", func(m *Outbound) { m.Headers = map[string]string{"X-A": "v\r\nBcc: eve@evil.com"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			msg := validOutbound()
			test.mutate(&msg)
			if err := msg.Validate(); err == nil {
				t.Fatal("Validate() = nil, want an error about a newline")
			}
		})
	}
}

// Given SES verdicts with some checks failed, when they are inspected, then
// only the failures are reported, in a fixed order.
func TestVerdictsFailed(t *testing.T) {
	tests := []struct {
		name     string
		verdicts Verdicts
		want     []string
	}{
		{"all clean", Verdicts{VerdictPass, VerdictPass, VerdictPass, VerdictPass, VerdictPass}, nil},
		{"unchecked", Verdicts{}, nil},
		{"gray is not a failure", Verdicts{Spam: VerdictGray}, nil},
		{"one failure", Verdicts{DMARC: VerdictFail}, []string{"DMARC"}},
		{
			"several failures keep their order",
			Verdicts{SPF: VerdictFail, DMARC: VerdictFail, Virus: VerdictFail},
			[]string{"SPF", "DMARC", "virus"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := test.verdicts.Failed()
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("Failed() = %v, want %v", got, test.want)
			}
		})
	}
}

// Given a Recorder standing in for SES, when messages are sent, then they are
// kept in order and never leave the process.
func TestRecorderRecordsSentMessages(t *testing.T) {
	recorder := NewRecorder()
	if got := recorder.Sent(); len(got) != 0 {
		t.Fatalf("Sent() = %v, want empty", got)
	}

	first := validOutbound()
	id, err := recorder.Send(context.Background(), first)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if id != "recorded-1" {
		t.Fatalf("Send() id = %q, want %q", id, "recorded-1")
	}
	second := validOutbound()
	second.Subject = "Second"
	if _, err := recorder.Send(context.Background(), second); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	sent := recorder.Sent()
	if len(sent) != 2 || sent[0].Subject != first.Subject || sent[1].Subject != "Second" {
		t.Fatalf("Sent() = %v, want the two messages in order", sent)
	}
}

// Given an invalid message, when it is recorded, then it is rejected exactly
// as SES would reject it, so a test that passes here would also send.
func TestRecorderRejectsInvalidMessage(t *testing.T) {
	recorder := NewRecorder()
	if _, err := recorder.Send(context.Background(), Outbound{}); err == nil {
		t.Fatal("Send() = nil, want a validation error")
	}
	if got := recorder.Sent(); len(got) != 0 {
		t.Fatalf("Sent() = %v, want the rejected message not to be recorded", got)
	}
}

// Given a Recorder told to fail, when a message is sent, then the failure is
// returned; and when it is reset, then it accepts messages again.
func TestRecorderFailWithAndReset(t *testing.T) {
	recorder := NewRecorder()
	wantErr := errors.New("ses is down")
	recorder.FailWith(wantErr)

	if _, err := recorder.Send(context.Background(), validOutbound()); !errors.Is(err, wantErr) {
		t.Fatalf("Send() error = %v, want %v", err, wantErr)
	}
	if got := recorder.Sent(); len(got) != 0 {
		t.Fatalf("Sent() = %v, want nothing recorded while failing", got)
	}

	recorder.FailWith(nil)
	if _, err := recorder.Send(context.Background(), validOutbound()); err != nil {
		t.Fatalf("Send() after clearing the failure = %v, want nil", err)
	}

	recorder.Reset()
	if got := recorder.Sent(); len(got) != 0 {
		t.Fatalf("Sent() after Reset() = %v, want empty", got)
	}
}
