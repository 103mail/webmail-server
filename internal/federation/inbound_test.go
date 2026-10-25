package federation

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func inboundRequest(body string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, InboundPath, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	for k, v := range headers {
		if v == "" {
			r.Header.Del(k)
			continue
		}
		r.Header.Set(k, v)
	}
	return r
}

// Given a request whose media type or encoding the protocol does not allow,
// when it is read, then it is refused before the body is trusted.
func TestRequestFromRefusesWhatItCannotAuthenticate(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		status  int
		code    string
	}{
		{"a form post", map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
			415, ErrUnsupportedMedia},
		{"no content type", map[string]string{"Content-Type": ""}, 415, ErrUnsupportedMedia},
		// The signature covers the transmitted bytes, so a compressed body
		// would verify against bytes the peer never signed (RFC 5.1).
		{"a gzipped body", map[string]string{"Content-Encoding": "gzip"}, 415, ErrUnsupportedMedia},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, r := RequestFrom(inboundRequest(`{}`, tt.headers), 1<<20)
			if r == nil {
				t.Fatalf("%s was accepted", tt.name)
			}
			if r.Status != tt.status || r.Code != tt.code {
				t.Errorf("got %d %s, want %d %s", r.Status, r.Code, tt.status, tt.code)
			}
		})
	}
}

// A body over the cap is refused, and a lying Content-Length does not get past
// it either: the read is bounded regardless of what the header claims.
func TestRequestFromCapsTheBody(t *testing.T) {
	big := strings.Repeat("x", 100)

	t.Run("an honest Content-Length", func(t *testing.T) {
		_, r := RequestFrom(inboundRequest(big, nil), 10)
		if r == nil || r.Status != 413 || r.Code != ErrTooLarge {
			t.Fatalf("got %v, want 413 too_large", r)
		}
	})

	t.Run("a Content-Length that lies", func(t *testing.T) {
		req := inboundRequest(big, nil)
		req.ContentLength = 5 // claims small, sends 100
		_, r := RequestFrom(req, 10)
		if r == nil || r.Status != 413 || r.Code != ErrTooLarge {
			t.Fatalf("got %v, want 413 too_large", r)
		}
	})

	t.Run("a body within the cap", func(t *testing.T) {
		parsed, r := RequestFrom(inboundRequest(`{"a":1}`, nil), 1<<20)
		if r != nil {
			t.Fatalf("a small body was rejected: %v", r)
		}
		if string(parsed.Body) != `{"a":1}` {
			t.Errorf("body = %q", parsed.Body)
		}
	})
}

// The headers are lowercased and trimmed on the way in, because the sender is
// compared against the envelope and the domain part of the sender address.
func TestRequestFromNormalisesTheSender(t *testing.T) {
	parsed, r := RequestFrom(inboundRequest(`{}`, map[string]string{
		"X-Federation-Sender": "  103Mail.COM ",
	}), 1<<20)
	if r != nil {
		t.Fatalf("rejected: %v", r)
	}
	if parsed.Sender != "103mail.com" {
		t.Errorf("sender = %q, want 103mail.com", parsed.Sender)
	}
}

// The Destination is the domain whose Federation hostname the request was
// sent to, which is what recipients are checked against (RFC 5.3).
func TestRequestFromReadsTheDestinationFromHost(t *testing.T) {
	req := inboundRequest(`{}`, nil)
	req.Host = "Webmail.Mail.Acme.test:443"
	parsed, r := RequestFrom(req, 1<<20)
	if r != nil {
		t.Fatalf("rejected: %v", r)
	}
	if parsed.Destination != "mail.acme.test" {
		t.Errorf("destination = %q, want mail.acme.test", parsed.Destination)
	}
}

// Given a Date outside the skew window in either direction, when it is checked,
// then the request is stale. A request from the future is as suspicious as one
// from the past.
func TestCheckDateEnforcesTheSkewWindowBothWays(t *testing.T) {
	s := &Service{}
	tests := []struct {
		name string
		date string
		ok   bool
	}{
		{"now", HTTPDate(time.Now()), true},
		{"a minute ago", HTTPDate(time.Now().Add(-time.Minute)), true},
		{"a minute ahead", HTTPDate(time.Now().Add(time.Minute)), true},
		{"an hour ago", HTTPDate(time.Now().Add(-time.Hour)), false},
		{"an hour ahead", HTTPDate(time.Now().Add(time.Hour)), false},
		{"missing", "", false},
		{"unparseable", "some time last week", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := s.checkDate(tt.date)
			if tt.ok && r != nil {
				t.Errorf("%s was refused: %v", tt.name, r)
			}
			if !tt.ok {
				if r == nil {
					t.Fatalf("%s was accepted", tt.name)
				}
				if r.Status != 401 || r.Code != ErrStaleRequest {
					t.Errorf("got %d %s, want 401 stale_request", r.Status, r.Code)
				}
			}
		})
	}
}

// The configuration is where a deployment could drift away from the one-hostname
// rule, so it is checked at load rather than at first use (RFC 9.2).
func TestCheckDomainNameRefusesNonDomains(t *testing.T) {
	bad := []string{
		"",                    // nothing
		"https://example.com", // a URL
		"example.com/path",    // a path
		"example.com:8443",    // a port
		"127.0.0.1",           // an address
		"localhost",           // no dot
		"webmail.example.com", // the Federation hostname, not the domain
	}
	for _, name := range bad {
		if err := checkDomainName(name); err == nil {
			t.Errorf("checkDomainName(%q) accepted", name)
		}
	}
	for _, name := range []string{"example.com", "mail.example.com", "eu.tenant.corp.example"} {
		if err := checkDomainName(name); err != nil {
			t.Errorf("checkDomainName(%q) refused: %v", name, err)
		}
	}
}

// With no configuration the service reports every domain as local, which is the
// single-domain behaviour federation replaces. Callers rely on this so they do
// not have to nil-check.
func TestDisabledServiceTreatsEveryDomainAsLocal(t *testing.T) {
	s, err := New(nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.Enabled() {
		t.Error("a service with no config reported itself enabled")
	}
	if !s.IsLocal("anywhere.example") {
		t.Error("a disabled service should treat every domain as local")
	}
	if _, ok := s.DescriptorFor("anywhere.example"); ok {
		t.Error("a disabled service served a descriptor")
	}
}

func TestLocalRecipientsFiltersToHostedDomains(t *testing.T) {
	s := &Service{local: map[string]*LocalDomain{"103mail.com": {Domain: "103mail.com"}}}
	got := s.LocalRecipients([]string{"a@103mail.com", "b@example.com", "c@103mail.com"})
	if len(got) != 2 || got[0] != "a@103mail.com" || got[1] != "c@103mail.com" {
		t.Errorf("LocalRecipients = %v", got)
	}
}

func TestPeerAllowlist(t *testing.T) {
	open := &Service{cfg: &Config{}}
	if !open.peerAllowed("anyone.example") {
		t.Error("an empty allowlist should be open federation")
	}
	closed := &Service{cfg: &Config{AllowedPeers: []string{"Example.com"}}}
	if !closed.peerAllowed("example.com") {
		t.Error("the allowlist should match case-insensitively")
	}
	if closed.peerAllowed("evil.com") {
		t.Error("a peer not on the allowlist was permitted")
	}
}

func TestBackoffStaysWithinItsCap(t *testing.T) {
	for _, schedule := range []struct {
		name        string
		base, limit time.Duration
	}{{"mail", RetryBase, RetryCap}, {"chat", ChatRetryBase, ChatRetryCap}} {
		for attempt := 1; attempt <= 20; attempt++ {
			delay := backoff(attempt, schedule.base, schedule.limit)
			if delay < schedule.base {
				t.Errorf("%s attempt %d waits %v, less than the base delay", schedule.name, attempt, delay)
			}
			if delay > schedule.limit+schedule.base {
				t.Errorf("%s attempt %d waits %v, past the cap", schedule.name, attempt, delay)
			}
		}
	}
}

func TestPermanentStatusesAreNotRetried(t *testing.T) {
	permanent := []int{400, 401, 403, 404, 413, 415}
	for _, status := range permanent {
		if !isPermanent(status) {
			t.Errorf("%d should be permanent", status)
		}
	}
	// 0 means no response was ever seen, which is a transport error.
	for _, status := range []int{0, 408, 425, 429, 500, 502, 503, 504} {
		if isPermanent(status) {
			t.Errorf("%d should be transient", status)
		}
	}
}

func TestRefusedRecipientsAreReadFromTheAcceptance(t *testing.T) {
	refused := refusalsIn([]byte(`{"envelope_id":"e","status":"accepted",` +
		`"refused_recipients":[{"address":"Nobody@Peer.test","error":"unknown_recipient"}]}`))
	if len(refused) != 1 || refused[0].Address != "nobody@peer.test" || refused[0].Error != ErrUnknownRecipient {
		t.Errorf("refusalsIn = %+v", refused)
	}
	// A 0.2 peer sends no list, and a body that is not JSON is no reason to
	// doubt a 202.
	for _, body := range []string{`{"envelope_id":"e","status":"accepted"}`, ``, `not json`} {
		if refused := refusalsIn([]byte(body)); len(refused) != 0 {
			t.Errorf("refusalsIn(%q) = %+v, want none", body, refused)
		}
	}
}

func TestOnlyRefusalsForAddressesSentAreBounced(t *testing.T) {
	envelope := Envelope{Recipients: []string{"bob@peer.test", "nobody@peer.test", "ghost@peer.test"}}
	got := bounceable(envelope, []Refusal{
		{Address: "nobody@peer.test", Error: ErrUnknownRecipient},
		{Address: "ghost@peer.test", Error: "something_new"},
		{Address: "carol@elsewhere.test", Error: ErrUnknownRecipient}, // never sent
		{Address: "nobody@peer.test", Error: ErrUnknownRecipient},     // listed twice
	})
	want := map[string][]string{
		ErrUnknownRecipient: {"nobody@peer.test"},
		"something_new":     {"ghost@peer.test"},
	}
	if len(got) != len(want) {
		t.Fatalf("bounceable = %v, want %v", got, want)
	}
	for code, addresses := range want {
		if strings.Join(got[code], ",") != strings.Join(addresses, ",") {
			t.Errorf("bounceable[%s] = %v, want %v", code, got[code], addresses)
		}
	}
}
