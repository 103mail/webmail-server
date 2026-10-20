package calendar

import (
	"context"
	"testing"
	"time"

	"github.com/mevdschee/webmail-server/internal/blob"
	"github.com/mevdschee/webmail-server/internal/store"
	"github.com/mevdschee/webmail-server/internal/store/storetest"
)

// Against a real database: SQLite by default, PostgreSQL when
// WEBMAIL_TEST_POSTGRES_DSN is set (ADR 19).

type attendee struct {
	customerID, mailboxID int64
}

func newAttendee(t *testing.T, db *store.DB, email, name string) attendee {
	t.Helper()
	ctx := context.Background()
	insert := func(query string, args ...any) int64 {
		id, err := store.Insert(ctx, db, query, args...)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return id
	}
	var a attendee
	a.customerID = insert(`INSERT INTO "customer" ("name") VALUES (?)`, name)
	a.mailboxID = insert(`INSERT INTO "mailbox" ("customer_id", "name", "color") VALUES (?,?,?)`, a.customerID, name, "red")
	alias := insert(`INSERT INTO "alias" ("customer_id", "email", "name", "mailbox_id") VALUES (?,?,?,?)`,
		a.customerID, email, name, a.mailboxID)
	if _, err := db.ExecContext(ctx, `UPDATE "mailbox" SET "default_alias_id" = ? WHERE "id" = ?`, alias, a.mailboxID); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestMeetings(t *testing.T) {
	db := storetest.Open(t)
	ctx := context.Background()
	svc := New(db, blob.New(t.TempDir()))
	alice := newAttendee(t, db, "alice@example.test", "Alice")
	bob := newAttendee(t, db, "bob@example.test", "Bob")

	calendars, err := svc.ListCalendars(ctx, alice.customerID)
	if err != nil || len(calendars) != 1 || calendars[0].CalendarID != alice.mailboxID {
		t.Fatalf("ListCalendars = %+v, %v", calendars, err)
	}

	amsterdam, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Skip("no zoneinfo:", err)
	}
	start := time.Date(2026, 10, 5, 9, 0, 0, 0, amsterdam)
	meetingID, err := svc.AddMeeting(ctx, alice.customerID, alice.mailboxID, NewMeeting{
		Attendees: []string{"Bob@Example.test"}, Title: "Standup",
		StartTime: start, EndTime: start.Add(30 * time.Minute), TimeZone: "Europe/Amsterdam",
		RepeatRule: "FREQ=DAILY;COUNT=3",
	})
	if err != nil || meetingID == 0 {
		t.Fatalf("AddMeeting = %d, %v", meetingID, err)
	}

	from := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	days, err := svc.ListMeetings(ctx, bob.customerID, []int64{bob.mailboxID}, from, from.AddDate(0, 0, 7))
	if err != nil {
		t.Fatal(err)
	}
	if got := days["2026-10-06"]; len(got) != 1 || got[0].StartTime != "2026-10-06T07:00:00+00:00" {
		t.Fatalf("Bob's meetings on the 6th = %+v", got)
	}
	total := 0
	for _, meetings := range days {
		total += len(meetings)
	}
	if total != 3 {
		t.Errorf("Bob has %d occurrences, want 3", total)
	}
	bobMeeting := days["2026-10-05"][0].MeetingID

	if ok, err := svc.SetRSVP(ctx, bob.customerID, bobMeeting, "no"); err != nil || !ok {
		t.Fatalf("SetRSVP = %v, %v", ok, err)
	}
	if ok, _ := svc.SetRSVP(ctx, bob.customerID, bobMeeting, "perhaps"); ok {
		t.Error("an invalid answer was accepted")
	}
	carol := newAttendee(t, db, "carol@example.test", "Carol")
	if ok, err := svc.AddAttendee(ctx, alice.customerID, meetingID, "CAROL@example.test"); err != nil || !ok {
		t.Fatalf("AddAttendee = %v, %v", ok, err)
	}
	if days, _ := svc.ListMeetings(ctx, carol.customerID, []int64{carol.mailboxID}, from, from.AddDate(0, 0, 1)); len(days["2026-10-05"]) != 1 {
		t.Error("Carol did not get the meeting")
	}

	docID, err := svc.AddDocument(ctx, alice.customerID, meetingID, "notes.txt", "text/plain", []byte("notes"))
	if err != nil || docID == 0 {
		t.Fatalf("AddDocument = %d, %v", docID, err)
	}
	if doc, err := svc.GetDocument(ctx, bob.customerID, docID); err != nil || doc == nil || string(doc.FileData) != "notes" {
		t.Fatalf("GetDocument = %+v, %v", doc, err)
	}
	if ok, err := svc.DeleteDocument(ctx, alice.customerID, docID); err != nil || !ok {
		t.Fatalf("DeleteDocument = %v, %v", ok, err)
	}
	if ok, err := svc.DeleteMeeting(ctx, bob.customerID, bobMeeting); err != nil || !ok {
		t.Fatalf("DeleteMeeting = %v, %v", ok, err)
	}
}
