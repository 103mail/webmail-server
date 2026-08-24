package mailer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
)

// fakeSNS captures what would have been published.
type fakeSNS struct {
	inputs []*sns.PublishInput
	err    error
}

func (f *fakeSNS) Publish(ctx context.Context, params *sns.PublishInput, optFns ...func(*sns.Options)) (*sns.PublishOutput, error) {
	f.inputs = append(f.inputs, params)
	if f.err != nil {
		return nil, f.err
	}
	return &sns.PublishOutput{MessageId: aws.String("sns-0001")}, nil
}

// Given a notification, when it is sent, then the topic receives the rendered
// message as JSON, with the recipient as a filterable attribute and no SNS
// Subject, which is limited to 100 ASCII characters a subject line need not
// respect.
func TestTopicPublishesNotification(t *testing.T) {
	api := &fakeSNS{}
	topic := NewTopic(api, TopicConfig{Region: "eu-west-1", TopicARN: "arn:aws:sns:eu-west-1:1:mail"})

	msg := validOutbound()
	msg.Bcc = []Address{{Email: "archive@example.com"}}
	id, err := topic.Send(context.Background(), msg)
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if id != "sns-0001" {
		t.Errorf("Send() = %q, want the SNS message id", id)
	}
	if len(api.inputs) != 1 {
		t.Fatalf("published %d times, want 1", len(api.inputs))
	}
	input := api.inputs[0]
	if got := aws.ToString(input.TopicArn); got != "arn:aws:sns:eu-west-1:1:mail" {
		t.Errorf("TopicArn = %q, want the configured topic", got)
	}
	if input.Subject != nil {
		t.Errorf("Subject = %q, want it left unset", aws.ToString(input.Subject))
	}
	if got := aws.ToString(input.MessageAttributes["recipient"].StringValue); got != "ada@example.org" {
		t.Errorf("recipient attribute = %q, want the first recipient", got)
	}

	var published OutboundNotification
	if err := json.Unmarshal([]byte(aws.ToString(input.Message)), &published); err != nil {
		t.Fatalf("published message is not an OutboundNotification: %v", err)
	}
	if published.Subject != msg.Subject || published.Text != msg.Text {
		t.Errorf("published %+v, want the rendered template", published)
	}
	if len(published.To) != 1 || published.To[0].Email != "ada@example.org" {
		t.Errorf("published To = %+v, want the recipient", published.To)
	}
	if len(published.Bcc) != 1 || published.Bcc[0].Email != "archive@example.com" {
		t.Errorf("published Bcc = %+v, want the blind recipient", published.Bcc)
	}

	// The raw message is what a subscriber that only relays bytes needs, and a
	// Bcc header in it would tell every recipient who else received the copy.
	if strings.Contains(string(published.Raw), "Bcc:") {
		t.Error("raw message carries a Bcc header")
	}
	parsed, err := ParseMIME(published.Raw)
	if err != nil {
		t.Fatalf("raw message does not parse: %v", err)
	}
	if parsed.Subject != msg.Subject {
		t.Errorf("raw subject = %q, want %q", parsed.Subject, msg.Subject)
	}
}

// Given a message with no recipient, when it is sent, then it is refused
// before it reaches AWS.
func TestTopicRejectsInvalidMessage(t *testing.T) {
	api := &fakeSNS{}
	msg := validOutbound()
	msg.To = nil

	if _, err := NewTopic(api, TopicConfig{TopicARN: "arn"}).Send(context.Background(), msg); err == nil {
		t.Fatal("Send() error = nil, want a validation failure")
	}
	if len(api.inputs) != 0 {
		t.Errorf("published %d messages, want none", len(api.inputs))
	}
}

// Given a notification over the size limit, when it is sent, then it is
// refused here rather than by SNS.
func TestTopicRejectsOversizedNotification(t *testing.T) {
	api := &fakeSNS{}
	topic := NewTopic(api, TopicConfig{TopicARN: "arn", MaxMessageBytes: 100})

	_, err := topic.Send(context.Background(), validOutbound())
	if err == nil || !strings.Contains(err.Error(), "over the 100 byte limit") {
		t.Fatalf("Send() error = %v, want the size limit", err)
	}
	if len(api.inputs) != 0 {
		t.Errorf("published %d messages, want none", len(api.inputs))
	}
}

// Given SNS refusing the publish, when a notification is sent, then the
// failure is wrapped rather than swallowed.
func TestTopicReportsFailure(t *testing.T) {
	wantErr := errors.New("topic does not exist")
	topic := NewTopic(&fakeSNS{err: wantErr}, TopicConfig{TopicARN: "arn"})

	_, err := topic.Send(context.Background(), validOutbound())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Send() error = %v, want it to wrap %v", err, wantErr)
	}
}

// Given no explicit size limit, when a publisher is built, then the default
// applies.
func TestNewTopicAppliesDefaultLimit(t *testing.T) {
	if got := NewTopic(&fakeSNS{}, TopicConfig{TopicARN: "arn"}).cfg.MaxMessageBytes; got != DefaultMaxPublishBytes {
		t.Fatalf("MaxMessageBytes = %d, want %d", got, DefaultMaxPublishBytes)
	}
}

// Given the ambient AWS configuration, when a publisher is loaded, then it is
// built without contacting AWS.
func TestLoadTopic(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	topic, err := LoadTopic(context.Background(), TopicConfig{
		Region:   "eu-west-1",
		TopicARN: "arn:aws:sns:eu-west-1:1:mail",
	})
	if err != nil {
		t.Fatalf("LoadTopic() error = %v", err)
	}
	if topic.api == nil {
		t.Fatal("LoadTopic() returned a publisher with no client")
	}
}

// Given no topic ARN, when a publisher is loaded, then it reports that rather
// than publishing into nowhere.
func TestLoadTopicRequiresARN(t *testing.T) {
	if _, err := LoadTopic(context.Background(), TopicConfig{Region: "eu-west-1"}); err == nil {
		t.Fatal("LoadTopic() error = nil, want a missing topic ARN")
	}
}
