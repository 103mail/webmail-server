package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// api stands in for a Domain's API: it counts what reaches it and answers 202.
type api struct{ hosts []string }

func (a *api) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.hosts = append(a.hosts, r.Host)
	w.WriteHeader(http.StatusAccepted)
	io.WriteString(w, `{"status":"accepted"}`)
}

func setup(t *testing.T) (*api, http.Handler, http.Handler) {
	t.Helper()
	upstream := &api{}
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	p, err := newProxy("example.com="+strings.TrimPrefix(server.URL, "http://"),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return upstream, p, p.controls()
}

func deliver(h http.Handler, from string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/federation/inbound", strings.NewReader("{}"))
	req.Host = "webmail.example.com"
	req.Header.Set("X-Faults-Domain", "example.com")
	req.Header.Set("X-Federation-Sender", from)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func arm(t *testing.T, controls http.Handler, rule string) {
	t.Helper()
	rec := httptest.NewRecorder()
	controls.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/faults", strings.NewReader(rule)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("arming %s answered %d: %s", rule, rec.Code, rec.Body)
	}
}

// Unarmed, a request reaches the API with its Host intact.
func TestPassesThrough(t *testing.T) {
	upstream, p, _ := setup(t)
	if rec := deliver(p, "103mail.com"); rec.Code != http.StatusAccepted {
		t.Fatalf("answered %d", rec.Code)
	}
	if len(upstream.hosts) != 1 || upstream.hosts[0] != "webmail.example.com" {
		t.Errorf("the API saw %v", upstream.hosts)
	}
}

// A lost answer still delivers, and is used up after one request.
func TestLoseOnce(t *testing.T) {
	upstream, p, controls := setup(t)
	arm(t, controls, `{"domain":"example.com","from":"103mail.com","action":"lose","count":1}`)
	if rec := deliver(p, "acme.test"); rec.Code != http.StatusAccepted {
		t.Errorf("another sender answered %d, want it untouched", rec.Code)
	}
	if rec := deliver(p, "103mail.com"); rec.Code != http.StatusBadGateway {
		t.Errorf("the lost answer is %d, want 502", rec.Code)
	}
	if rec := deliver(p, "103mail.com"); rec.Code != http.StatusAccepted {
		t.Errorf("after the fault: %d, want 202", rec.Code)
	}
	if len(upstream.hosts) != 3 {
		t.Errorf("the API saw %d requests, want all 3", len(upstream.hosts))
	}
}

// A refusal answers without forwarding, until cleared.
func TestAnswerUntilCleared(t *testing.T) {
	upstream, p, controls := setup(t)
	arm(t, controls, `{"domain":"example.com","action":"answer","status":429,"error":"rate_limited","retryAfter":60}`)
	for range 2 {
		rec := deliver(p, "103mail.com")
		var body struct {
			Error      string `json:"error"`
			RetryAfter int    `json:"retry_after_seconds"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != 429 || body.Error != "rate_limited" || body.RetryAfter != 60 || rec.Header().Get("Retry-After") != "60" {
			t.Errorf("answered %d %+v Retry-After %q", rec.Code, body, rec.Header().Get("Retry-After"))
		}
	}
	if len(upstream.hosts) != 0 {
		t.Errorf("a refused request reached the API")
	}
	rec := httptest.NewRecorder()
	controls.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/faults?domain=example.com", nil))
	if rec := deliver(p, "103mail.com"); rec.Code != http.StatusAccepted {
		t.Errorf("after clearing: %d, want 202", rec.Code)
	}
}
