package mailer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeObjects is an ObjectGetter backed by a map, so the receive path can be
// tested end to end without S3.
type fakeObjects struct {
	objects map[string][]byte
	err     error
	gets    []string
}

func (f *fakeObjects) GetObject(ctx context.Context, bucket, key string) ([]byte, error) {
	f.gets = append(f.gets, bucket+"/"+key)
	if f.err != nil {
		return nil, f.err
	}
	data, ok := f.objects[bucket+"/"+key]
	if !ok {
		return nil, fmt.Errorf("no such object %s/%s", bucket, key)
	}
	return data, nil
}

const inboundRaw = "From: Ada Lovelace <ada@example.org>\r\n" +
	"To: support@example.com\r\n" +
	"Subject: Re: your invoice\r\n" +
	"Message-Id: <reply@example.org>\r\n" +
	"In-Reply-To: <original@example.com>\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Thanks, received.\r\n"

// sesMessage renders the JSON SES puts in the SNS Message field.
func sesMessage(t *testing.T, action map[string]any, content string) string {
	t.Helper()
	event := map[string]any{
		"notificationType": "Received",
		"receipt": map[string]any{
			"recipients":   []string{"support@example.com"},
			"spfVerdict":   map[string]string{"status": "PASS"},
			"dkimVerdict":  map[string]string{"status": "PASS"},
			"dmarcVerdict": map[string]string{"status": "FAIL"},
			"spamVerdict":  map[string]string{"status": "PASS"},
			"virusVerdict": map[string]string{"status": "PASS"},
			"action":       action,
		},
		"mail": map[string]any{
			"messageId":   "ses-message-id",
			"source":      "ada@example.org",
			"destination": []string{"support@example.com"},
		},
	}
	if content != "" {
		event["content"] = content
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal ses event: %v", err)
	}
	return string(encoded)
}

func notificationBody(t *testing.T, n *Notification) []byte {
	t.Helper()
	body, err := json.Marshal(n)
	if err != nil {
		t.Fatalf("marshal notification: %v", err)
	}
	return body
}

// Given mail SES stored in S3 and announced over SNS, when the notification is
// received, then the message is fetched, parsed, and carries the envelope
// recipients and the checks SES ran.
func TestReceiverReceivesFromS3(t *testing.T) {
	harness := newSNSHarness(t)
	objects := &fakeObjects{objects: map[string][]byte{
		"webmail-inbound/mail/abc123": []byte(inboundRaw),
	}}
	message := sesMessage(t, map[string]any{
		"type":       "S3",
		"bucketName": "webmail-inbound",
		"objectKey":  "mail/abc123",
	}, "")
	n := harness.notification(t, message)

	receiver := NewReceiver(harness.verifier(n.TopicArn), objects)
	got, err := receiver.Receive(context.Background(), notificationBody(t, n))
	if err != nil {
		t.Fatalf("Receive() error = %v", err)
	}

	if got.ProviderID != "ses-message-id" {
		t.Errorf("ProviderID = %q, want the SES message id", got.ProviderID)
	}
	if got.From.Email != "ada@example.org" || got.From.Name != "Ada Lovelace" {
		t.Errorf("From = %+v, want Ada", got.From)
	}
	if got.Subject != "Re: your invoice" {
		t.Errorf("Subject = %q, want %q", got.Subject, "Re: your invoice")
	}
	if strings.TrimSpace(got.Text) != "Thanks, received." {
		t.Errorf("Text = %q, want the body", got.Text)
	}
	if got.InReplyTo != "original@example.com" {
		t.Errorf("InReplyTo = %q, want the parent id", got.InReplyTo)
	}
	if strings.Join(got.Recipients, ",") != "support@example.com" {
		t.Errorf("Recipients = %v, want the envelope recipient", got.Recipients)
	}
	if failed := got.Verdicts.Failed(); strings.Join(failed, ",") != "DMARC" {
		t.Errorf("Verdicts.Failed() = %v, want DMARC", failed)
	}
	if strings.Join(objects.gets, ",") != "webmail-inbound/mail/abc123" {
		t.Errorf("fetched %v, want the announced object", objects.gets)
	}
}

// Given a receipt rule using the SNS action, when the notification is
// received, then the inline copy of the message is used and no object store is
// needed.
func TestReceiverReceivesInlineContent(t *testing.T) {
	harness := newSNSHarness(t)
	tests := []struct {
		name     string
		encoding string
		content  string
	}{
		{"base64", "BASE64", base64.StdEncoding.EncodeToString([]byte(inboundRaw))},
		{"utf-8", "UTF-8", inboundRaw},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message := sesMessage(t, map[string]any{"type": "SNS", "encoding": test.encoding}, test.content)
			n := harness.notification(t, message)

			receiver := NewReceiver(harness.verifier(n.TopicArn), nil)
			got, err := receiver.Receive(context.Background(), notificationBody(t, n))
			if err != nil {
				t.Fatalf("Receive() error = %v", err)
			}
			if got.Subject != "Re: your invoice" {
				t.Fatalf("Subject = %q, want the parsed subject", got.Subject)
			}
		})
	}
}

// Given a notification that was not signed by AWS, when it is received, then
// nothing is fetched and nothing is parsed.
func TestReceiverRejectsUnsignedNotification(t *testing.T) {
	harness := newSNSHarness(t)
	objects := &fakeObjects{objects: map[string][]byte{}}
	message := sesMessage(t, map[string]any{
		"type":       "S3",
		"bucketName": "webmail-inbound",
		"objectKey":  "mail/abc123",
	}, "")
	n := harness.notification(t, message)
	n.Message = sesMessage(t, map[string]any{
		"type":       "S3",
		"bucketName": "attacker-bucket",
		"objectKey":  "mail/forged",
	}, "")

	receiver := NewReceiver(harness.verifier(n.TopicArn), objects)
	if _, err := receiver.Receive(context.Background(), notificationBody(t, n)); err == nil {
		t.Fatal("Receive() = nil, want a signature failure")
	}
	if len(objects.gets) != 0 {
		t.Fatalf("fetched %v, want nothing fetched for an unverified notification", objects.gets)
	}
}

// Given an SNS message that is not inbound mail, when it is received, then it
// is reported as such so the caller can confirm a subscription instead.
func TestReceiverReportsNonMail(t *testing.T) {
	harness := newSNSHarness(t)

	subscription := &Notification{
		Type:           TypeSubscriptionConfirmation,
		MessageID:      "sub-1",
		TopicArn:       "arn:aws:sns:eu-west-1:123456789012:webmail-inbound",
		Message:        "You have chosen to subscribe",
		Token:          "token-value",
		SubscribeURL:   harness.server.URL + "/confirm",
		Timestamp:      "2026-08-24T10:30:00.000Z",
		SigningCertURL: harness.certURL,
	}
	harness.sign(t, subscription, "2")

	bounce := harness.notification(t, `{"notificationType":"Bounce"}`)

	for _, test := range []struct {
		name string
		n    *Notification
	}{
		{"subscription confirmation", subscription},
		{"bounce notification", bounce},
	} {
		t.Run(test.name, func(t *testing.T) {
			receiver := NewReceiver(harness.verifier(test.n.TopicArn), nil)
			_, err := receiver.Receive(context.Background(), notificationBody(t, test.n))
			if !errors.Is(err, ErrNotMail) {
				t.Fatalf("Receive() error = %v, want ErrNotMail", err)
			}
		})
	}
}

// Given a receipt action this system is not set up for, when a notification
// arrives, then it fails loudly rather than dropping the mail silently.
func TestReceiverRejectsUnusableAction(t *testing.T) {
	harness := newSNSHarness(t)
	tests := []struct {
		name    string
		action  map[string]any
		objects ObjectGetter
		wantErr string
	}{
		{
			"lambda action",
			map[string]any{"type": "Lambda"},
			&fakeObjects{},
			"unsupported receipt action",
		},
		{
			"s3 action without an object store",
			map[string]any{"type": "S3", "bucketName": "b", "objectKey": "k"},
			nil,
			"no object store is configured",
		},
		{
			"s3 action without a key",
			map[string]any{"type": "S3", "bucketName": "b"},
			&fakeObjects{},
			"no bucket or key",
		},
		{
			"sns action without content",
			map[string]any{"type": "SNS", "encoding": "UTF-8"},
			nil,
			"carried no content",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			n := harness.notification(t, sesMessage(t, test.action, ""))
			receiver := NewReceiver(harness.verifier(n.TopicArn), test.objects)
			_, err := receiver.Receive(context.Background(), notificationBody(t, n))
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Receive() error = %v, want one containing %q", err, test.wantErr)
			}
		})
	}
}

// Given S3 refusing to hand the message over, when a notification arrives,
// then the failure is reported so the delivery can be retried.
func TestReceiverReportsFetchFailure(t *testing.T) {
	harness := newSNSHarness(t)
	objects := &fakeObjects{err: errors.New("access denied")}
	n := harness.notification(t, sesMessage(t, map[string]any{
		"type":       "S3",
		"bucketName": "webmail-inbound",
		"objectKey":  "mail/abc123",
	}, ""))

	receiver := NewReceiver(harness.verifier(n.TopicArn), objects)
	_, err := receiver.Receive(context.Background(), notificationBody(t, n))
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("Receive() error = %v, want the underlying failure", err)
	}
}
