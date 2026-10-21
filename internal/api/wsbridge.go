package api

import (
	"io"
	"net/http"
	"strings"
)

// The endpoints ../ws2api calls. See ADR 13 for the contract and ADR 18 for the
// frame format.
//
// These are NOT public. They are authenticated only by knowing a ClientId,
// which is what makes them safe for a proxy on a private network and unsafe on
// the internet. The reverse proxy in ../webmail-deploy forwards /api/ and /ws/
// and nothing else; /wsoverhttp/ is reachable only from inside the Domain
// network.

// maxFrameBytes caps a single client frame. Chat is text; anything larger is
// either a mistake or an attempt to make the server buffer for someone.
const maxFrameBytes = 64 << 10

// handleWSOverHTTP implements the three requests a connection's lifecycle
// becomes:
//
//	GET    /wsoverhttp/{ClientId}  the upgrade check, answered "ok" or refused
//	POST   /wsoverhttp/{ClientId}  one client frame, answered with one frame
//	DELETE /wsoverhttp/{ClientId}  the close, with the reason as the body
func (s *Server) handleWSOverHTTP(w http.ResponseWriter, r *http.Request) {
	if s.wsbridge == nil {
		http.Error(w, "chat bridge not configured", http.StatusServiceUnavailable)
		return
	}
	clientID := strings.TrimPrefix(r.URL.Path, "/wsoverhttp/")
	if clientID == "" || strings.Contains(clientID, "/") {
		http.Error(w, "bad client id", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		// The proxy accepts the upgrade only on exactly "ok"; anything else
		// refuses the connection with 403.
		if !s.wsbridge.Connect(r.Context(), clientID) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "denied")
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok")

	case http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, maxFrameBytes+1))
		if err != nil || len(body) > maxFrameBytes {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"type":"error","error":"frame too large"}`))
			return
		}
		// Whatever comes back is written to that same socket. An empty body is
		// a handler with nothing to say, and the client ignores the empty frame
		// the proxy writes.
		reply := s.wsbridge.Handle(r.Context(), clientID, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(reply)

	case http.MethodDelete:
		reason, _ := io.ReadAll(io.LimitReader(r.Body, 1024))
		s.wsbridge.Disconnect(r.Context(), clientID, strings.TrimSpace(string(reason)))
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok")

	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
