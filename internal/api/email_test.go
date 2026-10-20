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

// apiStack is a Server on an SQLite database hosting example.test, with
// accounts that sign in with their address as the token.
type apiStack struct {
	t      *testing.T
	db     *store.DB
	mail   *email.Service
	server *Server
}

func newAPIStack(t *testing.T) *apiStack {
	t.Helper()
	db := storetest.Open(t)
	if _, err := db.ExecContext(context.Background(), `INSERT INTO "domain" ("name") VALUES ('example.test')`); err != nil {
		t.Fatal(err)
	}
	mail := email.New(db, blob.New(t.TempDir()), nil, nil, nil)
	return &apiStack{t: t, db: db, mail: mail,
		server: New(&config.Config{MaxBodyBytes: 1 << 20}, slog.New(slog.NewTextHandler(io.Discard, nil)),
			auth.New(db), mail, nil, nil, nil, nil, nil, nil)}
}

// account creates someone whose bearer token is their address, and returns
// their customer id and Inbox.
func (a *apiStack) account(address, name string) (customerID, inboxID int64) {
	a.t.Helper()
	ctx := context.Background()
	customerID, err := store.Insert(ctx, a.db, `INSERT INTO "customer" ("name") VALUES (?)`, name)
	if err != nil {
		a.t.Fatal(err)
	}
	userID, err := store.Insert(ctx, a.db,
		`INSERT INTO "user" ("customer_id", "username", "password", "name") VALUES (?,?,?,?)`,
		customerID, address, "", name)
	if err != nil {
		a.t.Fatal(err)
	}
	mailboxID, err := a.mail.AddMailbox(ctx, customerID, name, "red", address, name)
	if err != nil || mailboxID == 0 {
		a.t.Fatalf("AddMailbox = %d, %v", mailboxID, err)
	}
	if _, err := a.mail.EditMailbox(ctx, customerID, 0, mailboxID, "", "", nil, nil, address, name); err != nil {
		a.t.Fatal(err)
	}
	inboxID, err = a.mail.DefaultFolderID(ctx, customerID, mailboxID)
	if err != nil {
		a.t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx,
		`INSERT INTO "token" ("customer_id", "user_id", "name", "value") VALUES (?,?,?,?)`,
		customerID, userID, "Session", address); err != nil {
		a.t.Fatal(err)
	}
	return customerID, inboxID
}

// call makes a request as the account whose token is as, and decodes the
// answer's error code when there is one.
func (a *apiStack) call(as, method, path, body string) (int, string) {
	a.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+as)
	rec := httptest.NewRecorder()
	a.server.Handler().ServeHTTP(rec, req)
	var answer struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &answer)
	return rec.Code, answer.Error
}

// Leaving is DELETE on one's own subscription. Removing anyone else, and
// writing after leaving, are refused as 403 with a code that says why.
func TestLeavingThroughTheAPI(t *testing.T) {
	a := newAPIStack(t)
	alice, aliceInbox := a.account("alice@example.test", "Alice")
	chris, chrisInbox := a.account("chris@example.test", "Chris")
	ctx := context.Background()
	if _, err := a.mail.AddThread(ctx, alice, aliceInbox, []string{"chris@example.test"}, "Vendors", "<p>x</p>", nil); err != nil {
		t.Fatal(err)
	}
	threads, err := a.mail.ListThreads(ctx, chris, chrisInbox, "")
	if err != nil || len(threads) != 1 {
		t.Fatalf("Chris has %d threads, %v", len(threads), err)
	}
	aliceThreads, _ := a.mail.ListThreads(ctx, alice, aliceInbox, "")
	aliceThread := strconv.FormatInt(aliceThreads[0].ThreadID, 10)
	chrisThread := strconv.FormatInt(threads[0].ThreadID, 10)

	if status, code := a.call("alice@example.test", http.MethodDelete,
		"/api/v1/email/subscriber/"+aliceThread+"?email=chris@example.test", ""); status != 403 || code != "only_yourself" {
		t.Errorf("Alice removing Chris answered %d %q, want 403 only_yourself", status, code)
	}
	if status, _ := a.call("chris@example.test", http.MethodDelete,
		"/api/v1/email/subscriber/"+chrisThread+"?email=chris@example.test", ""); status != 200 {
		t.Fatalf("Chris leaving answered %d", status)
	}
	if status, code := a.call("chris@example.test", http.MethodPost,
		"/api/v1/email/message/"+chrisThread, `{"body":"wait"}`); status != 403 || code != "not_a_member" {
		t.Errorf("Chris replying after leaving answered %d %q, want 403 not_a_member", status, code)
	}
}

// A send to an address on this Domain that has no account is the caller's
// mistake, so it is a 400 that names the address, not a 500.
func TestComposeToUnknownLocalAddressIsRefused(t *testing.T) {
	a := newAPIStack(t)
	_, folderID := a.account("alice@example.test", "Alice")
	s := a.server

	req := httptest.NewRequest(http.MethodPost, "/api/v1/email/thread/"+strconv.FormatInt(folderID, 10),
		strings.NewReader(`{"subject":"Hi","subscribers":["ghost@example.test"],"body":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice@example.test")
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
