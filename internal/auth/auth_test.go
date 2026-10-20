package auth

import (
	"context"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/mevdschee/webmail-server/internal/store"
	"github.com/mevdschee/webmail-server/internal/store/storetest"
)

// Against a real database: SQLite by default, PostgreSQL when
// WEBMAIL_TEST_POSTGRES_DSN is set (ADR 19).
func TestLoginAndAuthorize(t *testing.T) {
	db := storetest.Open(t)
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	db.Clock = func() time.Time { return now }
	ctx := context.Background()
	hash, err := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	customerID, err := store.Insert(ctx, db, `INSERT INTO "customer" ("name") VALUES (?)`, "Alice")
	if err != nil {
		t.Fatal(err)
	}
	userID, err := store.Insert(ctx, db,
		`INSERT INTO "user" ("customer_id", "username", "password", "name") VALUES (?,?,?,?)`,
		customerID, "alice@example.test", string(hash), "Alice")
	if err != nil {
		t.Fatal(err)
	}
	svc := New(db)

	if _, err := svc.Login(ctx, "", "alice@example.test", "wrong"); err != ErrInvalidCredentials {
		t.Errorf("a wrong password: %v", err)
	}
	token, err := svc.Login(ctx, "", " Alice@Example.TEST ", "secret")
	if err != nil {
		t.Fatalf("Login = %v", err)
	}
	again, err := svc.Login(ctx, "", "alice@example.test", "secret")
	if err != nil || again.Value != token.Value {
		t.Errorf("a second login made a new token: %+v, %v", again, err)
	}

	id, err := svc.Authorize(ctx, "Bearer "+token.Value)
	if err != nil || id.CustomerID != customerID || id.UserID != userID {
		t.Fatalf("Authorize = %+v, %v", id, err)
	}
	if id, _ := svc.Authorize(ctx, "Bearer nope"); id.Authenticated() {
		t.Error("an unknown token authenticated")
	}
	var used time.Time
	if err := db.QueryRowContext(ctx, `SELECT "used_at" FROM "token"`).Scan(&used); err != nil || !used.Equal(now) {
		t.Errorf("used_at = %v, %v", used, err)
	}
	u, err := svc.CurrentUser(ctx, id)
	if err != nil || u == nil || u.Email != "alice@example.test" {
		t.Errorf("CurrentUser = %+v, %v", u, err)
	}
}
