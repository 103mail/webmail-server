package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/mevdschee/webmail-server/internal/auth"
	"github.com/mevdschee/webmail-server/internal/blob"
	"github.com/mevdschee/webmail-server/internal/config"
	"github.com/mevdschee/webmail-server/internal/email"
	"github.com/mevdschee/webmail-server/internal/store"
	"github.com/mevdschee/webmail-server/internal/store/storetest"
)

// A send to an address on this Domain that has no account is the caller's
// mistake, so it is a 400 that names the address, not a 500.
func TestComposeToUnknownLocalAddressIsRefused(t *testing.T) {
	db := storetest.Open(t)
	ctx := context.Background()
	mail := email.New(db, blob.New(t.TempDir()), nil, nil, nil)
	if _, err := db.ExecContext(ctx, `INSERT INTO "domain" ("name") VALUES ('example.test')`); err != nil {
		t.Fatal(err)
	}
	customerID, err := store.Insert(ctx, db, `INSERT INTO "customer" ("name") VALUES (?)`, "Alice")
	if err != nil {
		t.Fatal(err)
	}
	userID, err := store.Insert(ctx, db,
		`INSERT INTO "user" ("customer_id", "username", "password", "name") VALUES (?,?,?,?)`,
		customerID, "alice@example.test", "", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	mailboxID, err := mail.AddMailbox(ctx, customerID, "Alice", "red", "alice@example.test", "Alice")
	if err != nil || mailboxID == 0 {
		t.Fatalf("AddMailbox = %d, %v", mailboxID, err)
	}
	folderID, err := mail.DefaultFolderID(ctx, customerID, mailboxID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO "token" ("customer_id", "user_id", "name", "value") VALUES (?,?,?,?)`,
		customerID, userID, "Session", "secret"); err != nil {
		t.Fatal(err)
	}
	s := New(&config.Config{MaxBodyBytes: 1 << 20}, slog.New(slog.NewTextHandler(io.Discard, nil)),
		auth.New(db), mail, nil, nil, nil, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/email/thread/"+strconv.FormatInt(folderID, 10),
		strings.NewReader(`{"subject":"Hi","subscribers":["ghost@example.test"],"body":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("answered %d, want 400: %s", rec.Code, rec.Body)
	}
	var body struct {
		Error      string   `json:"error"`
		Recipients []string `json:"recipients"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error != "unknown_recipients" || len(body.Recipients) != 1 || body.Recipients[0] != "ghost@example.test" {
		t.Errorf("body = %+v", body)
	}
}
