package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mevdschee/webmail-server/internal/auth"
	"github.com/mevdschee/webmail-server/internal/config"
	"github.com/mevdschee/webmail-server/internal/store"
	"github.com/mevdschee/webmail-server/internal/store/storetest"
)

func TestControlHandler(t *testing.T) {
	db := storetest.Open(t)
	ctx := context.Background()
	customerID, err := store.Insert(ctx, db, `INSERT INTO "customer" ("name") VALUES (?)`, "Alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Insert(ctx, db,
		`INSERT INTO "user" ("customer_id", "username", "password", "name") VALUES (?,?,?,?)`,
		customerID, "alice@example.test", "", "Alice"); err != nil {
		t.Fatal(err)
	}
	// No federation configured, as on a Domain that does not federate.
	s := New(&config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), auth.New(db),
		nil, nil, nil, nil, nil, nil, nil)
	h := s.ControlHandler()

	var accounts []auth.Account
	get(t, h, "/control/accounts", http.StatusOK, &accounts)
	if len(accounts) != 1 || accounts[0].Username != "alice@example.test" {
		t.Errorf("accounts = %+v", accounts)
	}
	var envelopes []json.RawMessage
	get(t, h, "/control/envelopes", http.StatusOK, &envelopes)
	if envelopes == nil || len(envelopes) != 0 {
		t.Errorf("envelopes = %v, want an empty list without federation", envelopes)
	}
	// Read-only: nothing answers a write.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/control/accounts", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST answered %d, want 405", rec.Code)
	}
}

func get(t *testing.T, h http.Handler, path string, status int, out any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != status {
		t.Fatalf("GET %s answered %d, want %d", path, rec.Code, status)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
}
