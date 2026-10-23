package api

import (
	"errors"
	"net/http"

	"github.com/103mail/webmail-server/internal/auth"
)

// handleAuth implements /api/v1/auth/{entity}.
func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request, req *request) (any, int) {
	if req.Entity != "token" {
		return nil, http.StatusNotFound
	}
	switch req.Method {
	case http.MethodGet:
		if !req.Identity.Authenticated() {
			return nil, http.StatusUnauthorized
		}
		user, err := s.auth.CurrentUser(r.Context(), req.Identity)
		if err != nil {
			s.fail(w, r, err)
			return nil, 0
		}
		if user == nil {
			return nil, http.StatusUnauthorized
		}
		return user, http.StatusOK

	case http.MethodPost:
		token, err := s.auth.Login(r.Context(), req.str("name"), req.str("email"), req.str("password"))
		if errors.Is(err, auth.ErrInvalidCredentials) {
			return nil, http.StatusForbidden
		}
		if err != nil {
			s.fail(w, r, err)
			return nil, 0
		}
		return token, http.StatusOK

	default:
		return nil, http.StatusMethodNotAllowed
	}
}
