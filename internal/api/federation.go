package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/mevdschee/webmail-server/internal/federation"
)

// The federation endpoints. They are the one part of the API that is not
// addressed by bearer token: a peer authenticates with an Ed25519 signature over
// the request, and the descriptor is public by definition.
//
// These handlers translate and nothing else. Every protocol rule, including
// which failures are permanent, lives in internal/federation, so that the wire
// behaviour is testable without an HTTP server. See ADR 17.

// handleWellKnown serves this Domain's peer descriptor (RFC 4.1).
//
// The Host header decides which Domain answers, because a Server may host
// several and each has its own descriptor and its own key. A descriptor is
// never shared across DNS domains.
func (s *Server) handleWellKnown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		s.respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	if s.federation == nil || !s.federation.Enabled() {
		s.respondJSON(w, http.StatusNotFound, map[string]string{"error": "not_federated"})
		return
	}

	domain := domainFromHost(r.Host)
	descriptor, ok := s.federation.DescriptorFor(domain)
	if !ok {
		// Answering for a hostname we are not authoritative for would let a
		// misrouted request look like a valid discovery.
		s.respondJSON(w, http.StatusNotFound, map[string]string{"error": "unknown_domain"})
		return
	}

	body, err := json.Marshal(descriptor)
	if err != nil {
		s.respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// handleFederationInbound accepts an envelope from a peer (RFC 6).
//
// No bearer token is read and nothing in the body is trusted until the
// signature verifies, which is the same discipline the SES webhook follows.
func (s *Server) handleFederationInbound(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		s.respondJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
		return
	}
	if s.federation == nil || !s.federation.Enabled() {
		s.respondJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "not_federated"})
		return
	}

	req, rejection := federation.RequestFrom(r, s.federation.MaxEnvelopeBytes())
	if rejection != nil {
		s.rejectFederation(w, r, rejection)
		return
	}

	accepted, rejection := s.federation.Receive(r.Context(), req)
	if rejection != nil {
		s.rejectFederation(w, r, rejection)
		return
	}
	s.respondJSON(w, http.StatusAccepted, accepted)
}

// rejectFederation writes a protocol failure. The `error` code is what tells the
// peer whether to retry, so it is part of the answer rather than a log line.
func (s *Server) rejectFederation(w http.ResponseWriter, r *http.Request, rejection *federation.Rejection) {
	s.log.Warn("federation request refused",
		"path", r.URL.Path, "sender", r.Header.Get("X-Federation-Sender"),
		"status", rejection.Status, "code", rejection.Code, "detail", rejection.Detail)

	body := map[string]string{"error": rejection.Code}
	if rejection.Detail != "" {
		body["detail"] = rejection.Detail
	}
	s.respondJSON(w, rejection.Status, body)
}

// domainFromHost turns a Federation hostname back into the DNS domain it serves.
// Every endpoint lives at webmail.{domain} and nowhere else, so anything not
// carrying that label is not a Domain of ours (RFC 1.3).
func domainFromHost(host string) string {
	if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return strings.TrimPrefix(host, "webmail.")
}

func (s *Server) respondJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}
