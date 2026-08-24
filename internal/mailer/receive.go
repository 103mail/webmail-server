package mailer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// sesEvent is the payload SES puts in the SNS Message field for a receipt
// rule. Only the fields we act on are modelled.
type sesEvent struct {
	NotificationType string `json:"notificationType"`
	// Content is the whole message, and is only present for the SNS receipt
	// action. The S3 action puts it in a bucket instead.
	Content string `json:"content"`
	Receipt struct {
		Recipients   []string      `json:"recipients"`
		SPFVerdict   verdictStatus `json:"spfVerdict"`
		DKIMVerdict  verdictStatus `json:"dkimVerdict"`
		DMARCVerdict verdictStatus `json:"dmarcVerdict"`
		SpamVerdict  verdictStatus `json:"spamVerdict"`
		VirusVerdict verdictStatus `json:"virusVerdict"`
		Action       struct {
			Type       string `json:"type"`
			BucketName string `json:"bucketName"`
			ObjectKey  string `json:"objectKey"`
			Encoding   string `json:"encoding"`
		} `json:"action"`
	} `json:"receipt"`
	Mail struct {
		MessageID   string   `json:"messageId"`
		Source      string   `json:"source"`
		Destination []string `json:"destination"`
	} `json:"mail"`
}

type verdictStatus struct {
	Status Verdict `json:"status"`
}

// Receiver turns an SNS post body into a parsed message.
//
// It verifies the SNS signature before touching anything else, so an
// unauthenticated post cannot inject mail. Objects is only consulted for the
// S3 receipt action, which is the one to use in production: the SNS action
// caps a message at 150 KB, which no message with attachments respects.
type Receiver struct {
	verifier *Verifier
	objects  ObjectGetter
}

// NewReceiver returns a Receiver. objects may be nil if the receipt rule uses
// the SNS action rather than the S3 action.
func NewReceiver(verifier *Verifier, objects ObjectGetter) *Receiver {
	return &Receiver{verifier: verifier, objects: objects}
}

// Receive verifies an SNS post body, retrieves the message SES stored for it
// and parses it.
//
// It returns ErrNotMail for anything that is not inbound mail, including
// subscription confirmations. Those are still verified first, so a caller can
// safely pass the notification to Verifier.Confirm afterwards.
func (r *Receiver) Receive(ctx context.Context, body []byte) (*Inbound, error) {
	notification, err := ParseNotification(body)
	if err != nil {
		return nil, err
	}
	if err := r.verifier.Verify(ctx, notification); err != nil {
		return nil, err
	}
	if notification.Type != TypeNotification {
		return nil, fmt.Errorf("%w: sns type %s", ErrNotMail, notification.Type)
	}

	var event sesEvent
	if err := json.Unmarshal([]byte(notification.Message), &event); err != nil {
		return nil, fmt.Errorf("parse ses notification: %w", err)
	}
	if event.NotificationType != "Received" {
		return nil, fmt.Errorf("%w: ses notification type %s", ErrNotMail, event.NotificationType)
	}

	raw, err := r.rawMessage(ctx, &event)
	if err != nil {
		return nil, err
	}
	inbound, err := ParseMIME(raw)
	if err != nil {
		return nil, err
	}
	inbound.ProviderID = event.Mail.MessageID
	inbound.Recipients = event.Receipt.Recipients
	inbound.Verdicts = Verdicts{
		SPF:   event.Receipt.SPFVerdict.Status,
		DKIM:  event.Receipt.DKIMVerdict.Status,
		DMARC: event.Receipt.DMARCVerdict.Status,
		Spam:  event.Receipt.SpamVerdict.Status,
		Virus: event.Receipt.VirusVerdict.Status,
	}
	return inbound, nil
}

// rawMessage retrieves the message bytes for an event, from wherever the
// receipt rule put them.
func (r *Receiver) rawMessage(ctx context.Context, event *sesEvent) ([]byte, error) {
	action := event.Receipt.Action
	switch action.Type {
	case "S3":
		if r.objects == nil {
			return nil, errors.New("mailer: receipt rule uses the S3 action but no object store is configured")
		}
		if action.BucketName == "" || action.ObjectKey == "" {
			return nil, errors.New("mailer: S3 receipt action has no bucket or key")
		}
		raw, err := r.objects.GetObject(ctx, action.BucketName, action.ObjectKey)
		if err != nil {
			return nil, fmt.Errorf("fetch stored message: %w", err)
		}
		return raw, nil
	case "SNS":
		if event.Content == "" {
			return nil, errors.New("mailer: SNS receipt action carried no content")
		}
		if action.Encoding == "BASE64" {
			raw, err := base64.StdEncoding.DecodeString(event.Content)
			if err != nil {
				return nil, fmt.Errorf("decode inline message: %w", err)
			}
			return raw, nil
		}
		return []byte(event.Content), nil
	default:
		return nil, fmt.Errorf("mailer: unsupported receipt action %q", action.Type)
	}
}
