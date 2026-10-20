// Command mailsink stands in for Amazon SNS in the local stack, so that the
// notification email every Domain sends is captured instead of delivered.
//
// The API servers publish to it exactly as they publish to SNS in production
// (ADR 15): AWS_ENDPOINT_URL_SNS points the SDK here, and the SNS Publish call
// arrives as the form-encoded query protocol it always uses. Nothing in the
// server knows it is talking to a sink.
//
// What arrives is kept in memory and listed at GET /messages, which is how the
// end-to-end stories read invitations and prove that no message content left
// any Domain by email. GET /messages?after=N lists only what came after
// message N. A restart forgets everything.
package main

import (
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

func main() {
	addr := flag.String("addr", ":4566", "listen address")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	log.Info("mail sink listening", "addr", *addr)
	if err := http.ListenAndServe(*addr, newSink(log)); err != nil {
		log.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// message is one published notification. Notification is the JSON the server
// published, kept as it arrived.
type message struct {
	ID           int             `json:"id"`
	Topic        string          `json:"topic"`
	ReceivedAt   time.Time       `json:"receivedAt"`
	Notification json.RawMessage `json:"notification"`
}

type sink struct {
	log      *slog.Logger
	mu       sync.Mutex
	messages []message
}

func newSink(log *slog.Logger) http.Handler {
	s := &sink{log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /", s.publish)
	mux.HandleFunc("GET /messages", s.list)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"status":"ok"}`)
	})
	return mux
}

// publish answers the SNS Publish action. Any other action is refused the way
// SNS refuses one, so a server that starts calling something new fails loudly
// here rather than appearing to work.
func (s *sink) publish(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		snsError(w, "InvalidParameter", "the request body is not a form")
		return
	}
	if action := r.PostForm.Get("Action"); action != "Publish" {
		snsError(w, "InvalidAction", fmt.Sprintf("the mail sink does not implement %q", action))
		return
	}
	body := r.PostForm.Get("Message")
	if !json.Valid([]byte(body)) {
		snsError(w, "InvalidParameter", "the message is not the JSON the server publishes")
		return
	}

	s.mu.Lock()
	m := message{
		ID:           len(s.messages) + 1,
		Topic:        r.PostForm.Get("TopicArn"),
		ReceivedAt:   time.Now().UTC(),
		Notification: json.RawMessage(body),
	}
	s.messages = append(s.messages, m)
	s.mu.Unlock()
	s.log.Info("captured", "id", m.ID, "topic", m.Topic)

	type result struct {
		MessageID string `xml:"PublishResult>MessageId"`
		RequestID string `xml:"ResponseMetadata>RequestId"`
	}
	writeXML(w, http.StatusOK, struct {
		XMLName xml.Name `xml:"PublishResponse"`
		result
	}{result: result{MessageID: fmt.Sprintf("mailsink-%d", m.ID), RequestID: strconv.Itoa(m.ID)}})
}

func (s *sink) list(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.Atoi(r.URL.Query().Get("after"))
	s.mu.Lock()
	out := []message{}
	for _, m := range s.messages {
		if m.ID > after {
			out = append(out, m)
		}
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// snsError writes the error shape of the SNS query protocol, which the SDK
// turns back into a typed error.
func snsError(w http.ResponseWriter, code, message string) {
	writeXML(w, http.StatusBadRequest, struct {
		XMLName xml.Name `xml:"ErrorResponse"`
		Type    string   `xml:"Error>Type"`
		Code    string   `xml:"Error>Code"`
		Message string   `xml:"Error>Message"`
	}{Type: "Sender", Code: code, Message: message})
}

func writeXML(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(status)
	w.Write([]byte(xml.Header))
	xml.NewEncoder(w).Encode(v)
}
