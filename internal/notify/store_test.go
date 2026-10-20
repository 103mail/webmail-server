package notify

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mevdschee/webmail-server/internal/mailer"
	"github.com/mevdschee/webmail-server/internal/store"
	"github.com/mevdschee/webmail-server/internal/store/storetest"
)

// Send against a real database: SQLite by default, PostgreSQL when
// WEBMAIL_TEST_POSTGRES_DSN is set (ADR 19).
func TestSendRecordsEveryAttempt(t *testing.T) {
	db := storetest.Open(t)
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	db.Clock = func() time.Time { return now }
	ctx := context.Background()
	for _, lang := range []string{"en", "nl"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO "email_template" ("name", "language", "sender_name", "sender_email", "subject", "body", "parameters") `+
				`VALUES (?,?,?,?,?,?,?)`,
			"invite", lang, "Webmail", "no-reply@example.test", "Hello "+lang, "Open {{link}}", "link"); err != nil {
			t.Fatal(err)
		}
	}
	recorder := mailer.NewRecorder()
	svc := New(db, recorder)

	if err := svc.Send(ctx, nil, "Bob", "bob@example.test", "invite", "nl",
		map[string]string{"link": "https://x.test/t"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.Send(ctx, nil, "Bob", "bob@example.test", "invite", "de", nil, ""); err != nil {
		t.Fatal(err)
	}
	sent := recorder.Sent()
	if len(sent) != 2 || sent[0].Subject != "Hello nl" || sent[1].Subject != "Hello en" {
		t.Fatalf("sent %+v", sent)
	}
	if err := svc.Send(ctx, nil, "", "bob@example.test", "missing", "en", nil, ""); !errors.Is(err, ErrNoTemplate) {
		t.Errorf("an unknown template: %v", err)
	}

	// A provider error longer than the column is cut on a rune boundary.
	recorder.FailWith(errors.New(strings.Repeat("é", 300)))
	if err := svc.Send(ctx, nil, "", "bob@example.test", "invite", "en", nil, ""); err == nil {
		t.Fatal("a failed send reported success")
	}
	var status, message string
	var created time.Time
	if err := db.QueryRowContext(ctx,
		`SELECT "status", "status_message", "created_at" FROM "sent_email" ORDER BY "id" DESC LIMIT 1`).
		Scan(&status, &message, &created); err != nil {
		t.Fatal(err)
	}
	if status != "error" || len([]rune(message)) != 255 || !created.Equal(now) {
		t.Errorf("recorded %s, %d runes, at %v", status, len([]rune(message)), created)
	}
	if n, _ := store.SelectInt(ctx, db, `SELECT COUNT(*) FROM "sent_email"`); n != 3 {
		t.Errorf("%d sent_email rows, want 3", n)
	}
}
