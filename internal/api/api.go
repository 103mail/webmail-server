// Package api exposes the domain packages over HTTP. Handlers translate between
// requests and the domain; they do not implement domain rules themselves.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/mevdschee/webmail-server/internal/auth"
	"github.com/mevdschee/webmail-server/internal/calendar"
	"github.com/mevdschee/webmail-server/internal/chat"
	"github.com/mevdschee/webmail-server/internal/config"
	"github.com/mevdschee/webmail-server/internal/email"
	"github.com/mevdschee/webmail-server/internal/federation"
	"github.com/mevdschee/webmail-server/internal/mailer"
	"github.com/mevdschee/webmail-server/internal/wsbridge"
)

// Server routes API requests to the domain services.
type Server struct {
	cfg      *config.Config
	log      *slog.Logger
	auth     *auth.Service
	email    *email.Service
	calendar *calendar.Service
	chat     *chat.Service
	origins  map[string]bool
	// receiver and verifier are nil when inbound mail is not configured, in
	// which case the webhook reports that it is unavailable rather than
	// silently accepting posts it cannot authenticate.
	receiver *mailer.Receiver
	verifier *mailer.Verifier
	// federation is nil when no Domain is configured, in which case the
	// well-known endpoint answers 404 and the inbound endpoint 503 rather than
	// accepting envelopes there is no key to verify.
	federation *federation.Service
	// wsbridge is nil when chat has no WebSocket tier, in which case the
	// endpoints ws2api calls answer 503 and the client falls back to reloading.
	wsbridge *wsbridge.Service
}

// New returns a Server. receiver and verifier may be nil, which disables the
// inbound mail webhook.
func New(cfg *config.Config, log *slog.Logger, a *auth.Service, e *email.Service,
	c *calendar.Service, ch *chat.Service, receiver *mailer.Receiver, verifier *mailer.Verifier,
	fed *federation.Service, ws *wsbridge.Service) *Server {
	origins := make(map[string]bool, len(cfg.AllowedOrigins))
	for _, o := range cfg.AllowedOrigins {
		if o != "" {
			origins[o] = true
		}
	}
	return &Server{cfg: cfg, log: log, auth: a, email: e, calendar: c, chat: ch,
		origins: origins, receiver: receiver, verifier: verifier, federation: fed, wsbridge: ws}
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/", s.route(s.handleAuth))
	mux.HandleFunc("/api/v1/email/", s.route(s.handleEmail))
	mux.HandleFunc("/api/v1/calendar/", s.route(s.handleCalendar))
	mux.HandleFunc("/api/v1/chat/", s.route(s.handleChat))
	mux.HandleFunc("/api/v1/mail/inbound", s.handleInboundMail)
	// Federation. Both are authenticated by Ed25519 signature or are public by
	// definition, so neither goes through the bearer-token route wrapper.
	mux.HandleFunc(federation.WellKnownPath, s.handleWellKnown)
	mux.HandleFunc(federation.InboundPath, s.handleFederationInbound)
	// The chat WebSocket bridge. Reachable only from inside the Domain network:
	// it is authenticated by knowing a ClientId, which the reverse proxy must
	// never expose. See ADR 18.
	mux.HandleFunc("/wsoverhttp/", s.handleWSOverHTTP)
	mux.HandleFunc("/health", s.handleHealth)
	return mux
}

// request is one parsed API call.
type request struct {
	Method   string
	Entity   string
	ID       string
	Data     map[string]any
	Identity auth.Identity
}

// handlerFunc is an entity handler. It returns the response body and status.
type handlerFunc func(w http.ResponseWriter, r *http.Request, req *request) (any, int)

// route wraps a handler with CORS, body parsing and authorization, which every
// API group shares.
func (s *Server) route(h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.setCORS(w, r)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		entity, id := splitPath(r.URL.Path)
		if entity == "" {
			// The PHP redirects to Swagger UI here. There is no UI to serve.
			s.respond(w, http.StatusNotFound, nil)
			return
		}

		data, err := s.parseBody(r)
		if err != nil {
			s.respond(w, http.StatusBadRequest, nil)
			return
		}

		identity, err := s.auth.Authorize(r.Context(), r.Header.Get("Authorization"))
		if err != nil {
			s.fail(w, r, err)
			return
		}

		req := &request{
			Method:   r.Method,
			Entity:   entity,
			ID:       id,
			Data:     data,
			Identity: identity,
		}
		body, status := h(w, r, req)
		if status == 0 {
			return // the handler wrote the response itself
		}
		s.respond(w, status, body)
	}
}

// splitPath extracts the entity and id from /api/v1/{group}/{entity}/{id}.
func splitPath(path string) (entity, id string) {
	trimmed := strings.Trim(path, "/")
	parts := strings.Split(trimmed, "/")
	// api / v1 / group / entity / id
	if len(parts) < 4 {
		return "", ""
	}
	entity = parts[3]
	if len(parts) > 4 {
		id = parts[4]
	}
	return entity, id
}

func (s *Server) setCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin != "" && s.origins[origin] {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	}
	w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Requested-With")
}

// parseBody reads the request parameters. A GET takes them from the query
// string; anything else from a JSON or form-encoded body.
func (s *Server) parseBody(r *http.Request) (map[string]any, error) {
	if r.Method == http.MethodGet || r.Method == http.MethodDelete {
		return valuesToMap(r.URL.Query()), nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, s.cfg.MaxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(body) == 0 {
		return map[string]any{}, nil
	}
	contentType := r.Header.Get("Content-Type")
	if i := strings.Index(contentType, ";"); i >= 0 {
		contentType = contentType[:i]
	}
	if strings.TrimSpace(contentType) == "application/json" {
		var data map[string]any
		if err := json.Unmarshal(body, &data); err != nil {
			return nil, fmt.Errorf("parse json body: %w", err)
		}
		if data == nil {
			data = map[string]any{}
		}
		return data, nil
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, fmt.Errorf("parse form body: %w", err)
	}
	return valuesToMap(values), nil
}

func valuesToMap(values url.Values) map[string]any {
	out := make(map[string]any, len(values))
	for k, v := range values {
		if strings.HasSuffix(k, "[]") {
			list := make([]any, len(v))
			for i, s := range v {
				list[i] = s
			}
			out[strings.TrimSuffix(k, "[]")] = list
			continue
		}
		if len(v) > 1 {
			list := make([]any, len(v))
			for i, s := range v {
				list[i] = s
			}
			out[k] = list
			continue
		}
		out[k] = v[0]
	}
	return out
}

// respond writes a JSON body, matching the PHP in leaving slashes and unicode
// unescaped.
func (s *Server) respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		s.log.Error("write response", "error", err)
	}
}

// fail logs an internal error and answers 500 without leaking detail.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	s.respond(w, http.StatusInternalServerError, nil)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.respond(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Accessors on request for reading parameters out of Data.

func (r *request) str(key string) string {
	switch v := r.Data[key].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

// strPtr distinguishes an absent key from an empty value, which several
// endpoints need in order to leave a field untouched.
func (r *request) strPtr(key string) *string {
	if _, ok := r.Data[key]; !ok {
		return nil
	}
	v := r.str(key)
	return &v
}

func (r *request) boolPtr(key string) *bool {
	raw, ok := r.Data[key]
	if !ok {
		return nil
	}
	var v bool
	switch t := raw.(type) {
	case bool:
		v = t
	case string:
		// Matches PHP's FILTER_VALIDATE_BOOLEAN.
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "1", "true", "on", "yes":
			v = true
		}
	case float64:
		v = t != 0
	}
	return &v
}

func (r *request) boolean(key string) bool {
	if p := r.boolPtr(key); p != nil {
		return *p
	}
	return false
}

func (r *request) int64(key string) int64 {
	switch v := r.Data[key].(type) {
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

// strings reads a list of strings, tolerating a single value or a JSON array.
func (r *request) strings(key string) ([]string, bool) {
	raw, ok := r.Data[key]
	if !ok {
		return nil, true
	}
	switch v := raw.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	case string:
		if v == "" {
			return nil, true
		}
		return []string{v}, true
	}
	return nil, false
}

// attachment is an uploaded file as it appears in a JSON request body.
type attachment struct {
	FileName string `json:"fileName"`
	MimeType string `json:"mimeType"`
	FileData []byte `json:"fileData"`
}

// attachments reads the attachments array. It reports false when the value is
// present but malformed, which the API answers with 400.
func (r *request) attachments() ([]attachment, bool) {
	raw, ok := r.Data["attachments"]
	if !ok {
		return nil, true
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	out := make([]attachment, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, false
		}
		name, hasName := m["fileName"].(string)
		mime, hasMime := m["mimeType"].(string)
		if !hasName || !hasMime {
			return nil, false
		}
		encoded, _ := m["fileData"].(string)
		data, err := decodeBase64(encoded)
		if err != nil {
			return nil, false
		}
		out = append(out, attachment{FileName: name, MimeType: mime, FileData: data})
	}
	return out, true
}

// parseID reads a numeric path segment. Zero means missing or malformed, which
// every endpoint answers with 400.
func parseID(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// parseIDs reads a comma-separated list of ids, as used by the meeting and
// presence endpoints. It reports false when any element is not a positive id.
func parseIDs(s string) ([]int64, bool) {
	if s == "" {
		return nil, false
	}
	parts := strings.Split(s, ",")
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		n := parseID(strings.TrimSpace(p))
		if n == 0 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

var errBadBase64 = errors.New("invalid base64")
