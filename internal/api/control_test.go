package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/mevdschee/webmail-server/internal/auth"
	"github.com/mevdschee/webmail-server/internal/blob"
	"github.com/mevdschee/webmail-server/internal/config"
	"github.com/mevdschee/webmail-server/internal/email"
	"github.com/mevdschee/webmail-server/internal/federation"
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

// The controls show how often a file's bytes are stored and who is on a
// thread, without content: the same bytes attached twice are stored once.
func TestControlsShowFilesAndThreads(t *testing.T) {
	a := newAPIStack(t)
	alice, aliceInbox := a.account("alice@example.test", "Alice")
	a.account("bob@example.test", "Bob")
	ctx := context.Background()
	logo := []email.NewAttachment{{FileName: "logo.png", MimeType: "image/png", FileData: []byte("png bytes")}}
	for _, subject := range []string{"Logo", "Logo again"} {
		if _, err := a.mail.AddThread(ctx, alice, aliceInbox, []string{"bob@example.test"}, subject, "<p>x</p>",
			slices.Clone(logo)); err != nil {
			t.Fatal(err)
		}
	}
	// A thread gets its federation id when it first federates; set it as that would.
	for i, id := range []string{"0192f0c0-0000-7000-8000-000000000001", "0192f0c0-0000-7000-8000-000000000002"} {
		if _, err := a.db.ExecContext(ctx, `UPDATE "g_thread" SET "federation_id" = ? WHERE "id" = ?`, id, i+1); err != nil {
			t.Fatal(err)
		}
	}
	h := a.server.ControlHandler()

	var files []email.StoredFile
	get(t, h, "/control/files?md5="+blob.Hash([]byte("png bytes")), http.StatusOK, &files)
	if len(files) != 1 || files[0].Attachments != 2 || !files[0].Stored || files[0].FileSize != 9 {
		t.Errorf("files = %+v, want one stored file behind two attachments", files)
	}
	var threads []email.ThreadMembers
	get(t, h, "/control/threads?member=bob@example.test", http.StatusOK, &threads)
	if len(threads) != 2 || !slices.Equal(threads[0].Members, []string{"alice@example.test", "bob@example.test"}) {
		t.Errorf("threads = %+v, want two threads with Alice and Bob", threads)
	}
	var peers []json.RawMessage
	get(t, h, "/control/peers", http.StatusOK, &peers)
	if peers == nil || len(peers) != 0 {
		t.Errorf("peers = %v, want an empty list without federation", peers)
	}
}

// A 429 tells the peer how long to wait, in the header and in the body
// (RFC 6.3, 6.4).
func TestRateLimitedAnswerSaysHowLong(t *testing.T) {
	s := New(&config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil,
		nil, nil, nil, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	s.rejectFederation(rec, httptest.NewRequest(http.MethodPost, "/api/v1/federation/inbound", nil),
		&federation.Rejection{Status: 429, Code: federation.ErrRateLimited, RetryAfter: 42})
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "42" {
		t.Errorf("answered %d with Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	var body struct {
		Error             string `json:"error"`
		RetryAfterSeconds int    `json:"retry_after_seconds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error != "rate_limited" || body.RetryAfterSeconds != 42 {
		t.Errorf("body %s, %v", rec.Body, err)
	}
}
