package mailer

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sns/types"
)

// PublishAPI is the part of the SNS client the publisher uses. Depending on an
// interface rather than the concrete client is what lets tests run the whole
// publish path without AWS.
type PublishAPI interface {
	Publish(ctx context.Context, params *sns.PublishInput, optFns ...func(*sns.Options)) (*sns.PublishOutput, error)
}

// TopicConfig configures the SNS publisher.
type TopicConfig struct {
	// Region is the SNS region, for example "eu-west-1".
	Region string
	// TopicARN is the topic notifications are published to.
	TopicARN string
	// MaxMessageBytes rejects an oversized payload before it reaches AWS.
	// Zero means DefaultMaxPublishBytes.
	MaxMessageBytes int
}

// DefaultMaxPublishBytes is the size at which a notification is refused. SNS
// itself accepts 256 KB; a notification carries no message content (ADR 4), so
// anything near this means something has gone wrong upstream.
const DefaultMaxPublishBytes = 200 << 10

// OutboundNotification is the payload published for one outbound notification.
//
// It is JSON rather than MIME because the subscriber decides how to deliver it.
// Nothing here carries thread content: the fields are the rendered template,
// which is metadata and a link by construction.
type OutboundNotification struct {
	From    Address   `json:"from"`
	To      []Address `json:"to"`
	Bcc     []Address `json:"bcc,omitempty"`
	ReplyTo []Address `json:"replyTo,omitempty"`
	Subject string    `json:"subject"`
	Text    string    `json:"text,omitempty"`
	HTML    string    `json:"html,omitempty"`
	// Raw is the complete RFC 5322 message. A subscriber that only relays
	// bytes can hand this to any sender without knowing how to build MIME;
	// one that wants to reformat has the structured fields above instead.
	Raw []byte `json:"raw"`
}

// Topic publishes outbound notifications to an SNS topic. It is safe for
// concurrent use.
//
// Delivery is the subscriber's job. This system's responsibility ends when the
// topic accepts the notification, which is what keeps a user-facing action from
// blocking on a mail provider.
type Topic struct {
	api PublishAPI
	cfg TopicConfig
}

// NewTopic returns a Sender that publishes to an SNS topic.
func NewTopic(api PublishAPI, cfg TopicConfig) *Topic {
	if cfg.MaxMessageBytes <= 0 {
		cfg.MaxMessageBytes = DefaultMaxPublishBytes
	}
	return &Topic{api: api, cfg: cfg}
}

// LoadTopic builds a publisher from the ambient AWS configuration: the
// environment, the shared credentials files, or the instance role. It does not
// contact AWS, so a bad configuration surfaces on the first Send.
func LoadTopic(ctx context.Context, cfg TopicConfig) (*Topic, error) {
	if cfg.TopicARN == "" {
		return nil, fmt.Errorf("mailer: no sns topic arn configured")
	}
	options := []func(*awsconfig.LoadOptions) error{}
	if cfg.Region != "" {
		options = append(options, awsconfig.WithRegion(cfg.Region))
	}
	loaded, err := awsconfig.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("load aws configuration: %w", err)
	}
	return NewTopic(sns.NewFromConfig(loaded), cfg), nil
}

// Send publishes msg and returns the SNS message id.
//
// Recipients travel in the payload rather than in SNS subscriptions, so a
// notification can be addressed to someone who has never subscribed to
// anything. That is what an onboarding invitation needs.
func (t *Topic) Send(ctx context.Context, msg Outbound) (string, error) {
	if err := msg.Validate(); err != nil {
		return "", err
	}
	raw, err := BuildMIME(msg)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(OutboundNotification{
		From:    msg.From,
		To:      msg.To,
		Bcc:     msg.Bcc,
		ReplyTo: msg.ReplyTo,
		Subject: msg.Subject,
		Text:    msg.Text,
		HTML:    msg.HTML,
		Raw:     raw,
	})
	if err != nil {
		return "", fmt.Errorf("encode notification: %w", err)
	}
	if len(payload) > t.cfg.MaxMessageBytes {
		return "", fmt.Errorf("mailer: notification is %d bytes, over the %d byte limit",
			len(payload), t.cfg.MaxMessageBytes)
	}

	// Message attributes let a subscription filter without parsing the body.
	// The SNS Subject field is not used: it is limited to 100 ASCII characters
	// with no newlines, which a rendered subject line cannot be relied on to
	// respect.
	attributes := map[string]types.MessageAttributeValue{
		"recipient": {DataType: aws.String("String"), StringValue: aws.String(msg.To[0].Email)},
	}
	out, err := t.api.Publish(ctx, &sns.PublishInput{
		TopicArn:          aws.String(t.cfg.TopicARN),
		Message:           aws.String(string(payload)),
		MessageAttributes: attributes,
	})
	if err != nil {
		return "", fmt.Errorf("publish notification: %w", err)
	}
	return aws.ToString(out.MessageId), nil
}
