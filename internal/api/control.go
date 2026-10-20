package api

import (
	"net/http"

	"github.com/mevdschee/webmail-server/internal/federation"
)

// ControlHandler serves the read-only controls the end-to-end suite uses to
// see what a Domain did: which envelopes it sent where, and which accounts it
// holds (ADR 20). It is never part of Handler. cmd/webmail-server serves it on
// its own listener, and only when WEBMAIL_CONTROL_ADDR is set, so no public
// hostname can reach it.
//
// There is no authentication: whoever can reach the listener is the operator.
func (s *Server) ControlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /control/envelopes", func(w http.ResponseWriter, r *http.Request) {
		if s.federation == nil {
			s.respond(w, http.StatusOK, []federation.OutboxEntry{})
			return
		}
		entries, err := s.federation.Outbox(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.respond(w, http.StatusOK, entries)
	})
	mux.HandleFunc("GET /control/accounts", func(w http.ResponseWriter, r *http.Request) {
		accounts, err := s.auth.Accounts(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.respond(w, http.StatusOK, accounts)
	})
	return mux
}
