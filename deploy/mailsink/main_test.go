package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"

	"github.com/mevdschee/webmail-server/internal/mailer"
)

// TestPublishThroughTheSDK publishes with the same Topic and AWS SDK client
// the server uses, pointed at the sink, and reads the result back. That is
// the whole claim the sink makes: the server cannot tell it from SNS.
func TestPublishThroughTheSDK(t *testing.T) {
	server := httptest.NewServer(newSink(slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer server.Close()

	client := sns.New(sns.Options{
		Region:       "eu-west-1",
		BaseEndpoint: aws.String(server.URL),
		Credentials:  aws.AnonymousCredentials{},
	})
	topic := mailer.NewTopic(client, mailer.TopicConfig{TopicARN: "arn:aws:sns:eu-west-1:000000000000:example"})
	id, err := topic.Send(context.Background(), mailer.Outbound{
		From:    mailer.Address{Email: "noreply@example.test"},
		To:      []mailer.Address{{Name: "Bob", Email: "bob@example.test"}},
		Subject: "You were added to a thread",
		Text:    "Open it here.",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if id != "mailsink-1" {
		t.Errorf("message id %q, want mailsink-1", id)
	}

	var got []struct {
		ID           int    `json:"id"`
		Topic        string `json:"topic"`
		Notification mailer.OutboundNotification
	}
	get(t, server.URL+"/messages", &got)
	if len(got) != 1 || got[0].Topic != "arn:aws:sns:eu-west-1:000000000000:example" ||
		got[0].Notification.Subject != "You were added to a thread" ||
		got[0].Notification.To[0].Email != "bob@example.test" || len(got[0].Notification.Raw) == 0 {
		t.Fatalf("messages = %+v", got)
	}

	get(t, server.URL+"/messages?after=1", &got)
	if len(got) != 0 {
		t.Errorf("%d messages after the last one, want none", len(got))
	}
}

func TestOtherActionsAreRefused(t *testing.T) {
	server := httptest.NewServer(newSink(slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer server.Close()

	client := sns.New(sns.Options{
		Region:       "eu-west-1",
		BaseEndpoint: aws.String(server.URL),
		Credentials:  aws.AnonymousCredentials{},
	})
	if _, err := client.ListTopics(context.Background(), &sns.ListTopicsInput{}); err == nil {
		t.Error("ListTopics succeeded against the sink")
	}
}

func get(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}
