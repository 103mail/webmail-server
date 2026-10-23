package api

import (
	"net/http"

	"github.com/103mail/webmail-server/internal/federation"
)

// ControlHandler serves the read-only controls the end-to-end suite uses to
// see what a Domain did: which envelopes it sent where, which accounts it
// holds, which peer keys it pinned, how often it stores a file's bytes, and
// who is on its federated threads (ADR 20). It is never part of Handler. cmd/webmail-server serves it on
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
	mux.HandleFunc("GET /control/peers", func(w http.ResponseWriter, r *http.Request) {
		if s.federation == nil {
			s.respond(w, http.StatusOK, []federation.PinnedPeer{})
			return
		}
		peers, err := s.federation.Peers(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.respond(w, http.StatusOK, peers)
	})
	mux.HandleFunc("GET /control/files", func(w http.ResponseWriter, r *http.Request) {
		files, err := s.email.StoredFiles(r.Context(), r.URL.Query().Get("md5"))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.respond(w, http.StatusOK, files)
	})
	mux.HandleFunc("GET /control/threads", func(w http.ResponseWriter, r *http.Request) {
		threads, err := s.email.ThreadsOf(r.Context(), r.URL.Query().Get("member"))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.respond(w, http.StatusOK, threads)
	})
	return mux
}
