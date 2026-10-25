package calendar

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/103mail/webmail-server/internal/blob"
	"github.com/103mail/webmail-server/internal/federation"
	"github.com/103mail/webmail-server/internal/rrule"
	"github.com/103mail/webmail-server/internal/store"
	"github.com/103mail/webmail-server/internal/store/storetest"
)

// Against a real database: SQLite by default, PostgreSQL when
// WEBMAIL_TEST_POSTGRES_DSN is set (ADR 19).

type attendee struct {
	email                 string
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
	a := attendee{email: email}
	a.customerID = insert(`INSERT INTO "customer" ("name") VALUES (?)`, name)
	a.mailboxID = insert(`INSERT INTO "mailbox" ("customer_id", "name", "color") VALUES (?,?,?)`, a.customerID, name, "red")
	alias := insert(`INSERT INTO "alias" ("customer_id", "email", "name", "mailbox_id") VALUES (?,?,?,?)`,
		a.customerID, email, name, a.mailboxID)
	if _, err := db.ExecContext(ctx, `UPDATE "mailbox" SET "default_alias_id" = ? WHERE "id" = ?`, alias, a.mailboxID); err != nil {
		t.Fatal(err)
	}
	return a
}

// fakeFederator hosts example.test and records what would be sent. Domains
// named nocal do not do calendar, and nowhere.test cannot be discovered.
type fakeFederator struct {
	meetings []federation.MeetingOutbound
	answers  []federation.RSVPOutbound
}

func (f *fakeFederator) IsLocal(domain string) bool { return domain == "example.test" }
func (f *fakeFederator) OriginFor(address string) (*federation.LocalDomain, bool) {
	if domain := federation.DomainOf(address); f.IsLocal(domain) {
		return &federation.LocalDomain{Domain: domain}, true
	}
	return nil, false
}
func (f *fakeFederator) Calendar(ctx context.Context, domain string) (bool, error) {
	if domain == "nowhere.test" {
		return false, errors.New("no descriptor")
	}
	return !strings.HasPrefix(domain, "nocal"), nil
}
func (f *fakeFederator) EnqueueMeeting(ctx context.Context, tx *sql.Tx, out federation.MeetingOutbound) error {
	f.meetings = append(f.meetings, out)
	return nil
}
func (f *fakeFederator) EnqueueRSVP(ctx context.Context, tx *sql.Tx, out federation.RSVPOutbound) error {
	f.answers = append(f.answers, out)
	return nil
}

type notice struct{ recipient, subject, body string }

type recordingNotices struct{ sent []notice }

func (n *recordingNotices) Notice(ctx context.Context, origin, recipient, subject, body string) error {
	n.sent = append(n.sent, notice{recipient, subject, body})
	return nil
}

type fixture struct {
	t       *testing.T
	db      *store.DB
	svc     *Service
	fed     *fakeFederator
	notices *recordingNotices
	now     time.Time
}

func newFixture(t *testing.T, federated bool) *fixture {
	db := storetest.Open(t)
	f := &fixture{t: t, db: db, svc: New(db, blob.New(t.TempDir())), notices: &recordingNotices{},
		now: time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)}
	db.Clock = func() time.Time { return f.now }
	f.svc.NotifyBy(f.notices)
	if federated {
		f.fed = &fakeFederator{}
		f.svc.Federate(f.fed)
	}
	return f
}

// occurrences lists what someone sees between the 1st of November and the
// end of the year, in order.
func (f *fixture) occurrences(a attendee) []Meeting {
	f.t.Helper()
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	days, err := f.svc.ListMeetings(context.Background(), a.customerID, []int64{a.mailboxID}, from, from.AddDate(0, 2, 0))
	if err != nil {
		f.t.Fatal(err)
	}
	seen := map[string]bool{}
	var out []Meeting
	for d := from; d.Before(from.AddDate(0, 2, 0)); d = d.AddDate(0, 0, 1) {
		for _, m := range days[d.Format("2006-01-02")] {
			if key := fmt.Sprint(m.MeetingID, m.Occurrence); !seen[key] {
				seen[key] = true
				out = append(out, m)
			}
		}
	}
	return out
}

func answerOf(m Meeting, email string) (string, Attendee) {
	for _, a := range m.Attendees {
		if a.Email == email {
			if a.WillAttend == nil {
				return "", a
			}
			return *a.WillAttend, a
		}
	}
	return "absent", Attendee{}
}

func ptr[T any](v T) *T { return &v }

var launch = time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)

// A weekly meeting on one Server: the attendee sees every occurrence, answers
// one of them and then the whole series, and only the organizer changes it.
// Every change is a revision and earlier answers are marked as given to an
// earlier one (ADR 22, RFC 15.5).
func TestMeetingLifecycle(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()
	alice := newAttendee(t, f.db, "alice@example.test", "Alice")
	bob := newAttendee(t, f.db, "bob@example.test", "Bob")

	calendars, err := f.svc.ListCalendars(ctx, alice.customerID)
	if err != nil || len(calendars) != 1 || calendars[0].CalendarID != alice.mailboxID {
		t.Fatalf("ListCalendars = %+v, %v", calendars, err)
	}
	if _, err := f.svc.AddMeeting(ctx, alice.customerID, alice.mailboxID, NewMeeting{
		Title: "x", StartTime: launch, EndTime: launch.Add(time.Hour), TimeZone: "UTC", RepeatRule: "FREQ=HOURLY",
	}); err == nil {
		t.Error("an hourly rule, which RFC 15.6 has no place for, was accepted")
	}
	aliceMeeting, err := f.svc.AddMeeting(ctx, alice.customerID, alice.mailboxID, NewMeeting{
		Attendees: []string{"Bob@Example.test"}, Title: "Launch review",
		StartTime: launch, EndTime: launch.Add(time.Hour), TimeZone: "Europe/Amsterdam",
		RepeatRule: "FREQ=WEEKLY;BYDAY=TU;COUNT=3",
	})
	if err != nil || aliceMeeting == 0 {
		t.Fatalf("AddMeeting = %d, %v", aliceMeeting, err)
	}

	got := f.occurrences(bob)
	if len(got) != 3 || got[1].StartTime != "2026-11-10T08:00:00+00:00" || got[0].RepeatText != "weekly on Tuesday, 3 times" {
		t.Fatalf("Bob's occurrences = %+v", got)
	}
	if got[0].MeetingID != got[2].MeetingID || got[0].IsOrganizer || !got[0].Unseen || got[0].Status != "confirmed" {
		t.Errorf("one projection per meeting, Bob not organizing, unseen: %+v", got[0])
	}
	if answer, _ := answerOf(got[0], "alice@example.test"); answer != "yes" {
		t.Errorf("the organizer answers %q, want yes", answer)
	}
	bobMeeting := got[0].MeetingID

	// An answer for the second occurrence, then one for the whole series.
	if ok, err := f.svc.SetRSVP(ctx, bob.customerID, bobMeeting, "no", got[1].Occurrence); err != nil || !ok {
		t.Fatalf("SetRSVP(occurrence) = %v, %v", ok, err)
	}
	if _, err := f.svc.SetRSVP(ctx, bob.customerID, bobMeeting, "no", "2026-11-11T08:00:00Z"); !errors.Is(err, ErrBadInput) {
		t.Errorf("an answer for an occurrence the meeting does not have: %v", err)
	}
	got = f.occurrences(alice)
	if a, _ := answerOf(got[1], "bob@example.test"); a != "no" {
		t.Errorf("Bob's answer for the 10th = %q", a)
	}
	if a, _ := answerOf(got[0], "bob@example.test"); a != "" {
		t.Errorf("Bob answered the 3rd as %q, without answering it", a)
	}
	f.now = f.now.Add(time.Minute)
	if ok, err := f.svc.SetRSVP(ctx, bob.customerID, bobMeeting, "yes", ""); err != nil || !ok {
		t.Fatalf("SetRSVP = %v, %v", ok, err)
	}
	for _, m := range f.occurrences(alice) {
		if a, _ := answerOf(m, "bob@example.test"); a != "yes" {
			t.Errorf("a later answer for the series also answers %s, got %q", m.Occurrence, a)
		}
	}
	if ok, _ := f.svc.SetRSVP(ctx, alice.customerID, aliceMeeting, "no", ""); ok {
		t.Error("the organizer answered their own meeting")
	}
	if ok, _ := f.svc.SetRSVP(ctx, bob.customerID, bobMeeting, "perhaps", ""); ok {
		t.Error("an invalid answer was accepted")
	}

	// Only the organizer changes the meeting.
	if _, err := f.svc.UpdateMeeting(ctx, bob.customerID, bobMeeting, Change{Title: ptr("Mine")}); !errors.Is(err, ErrNotOrganizer) {
		t.Errorf("an attendee changed the meeting: %v", err)
	}
	if _, err := f.svc.AddAttendee(ctx, bob.customerID, bobMeeting, "carol@example.test"); !errors.Is(err, ErrNotOrganizer) {
		t.Errorf("an attendee invited someone: %v", err)
	}
	if ok, err := f.svc.UpdateMeeting(ctx, alice.customerID, aliceMeeting, Change{Title: ptr("Launch review, moved")}); err != nil || !ok {
		t.Fatalf("UpdateMeeting = %v, %v", ok, err)
	}
	got = f.occurrences(bob)
	if got[0].Title != "Launch review, moved" || got[0].Revision != 2 {
		t.Errorf("after the change Bob sees %q at revision %d", got[0].Title, got[0].Revision)
	}
	if a, given := answerOf(got[0], "bob@example.test"); a != "yes" || !given.Outdated || given.Revision != 1 {
		t.Errorf("Bob's answer to revision 1 is not marked as outdated: %+v", given)
	}

	// One occurrence cancelled and one moved, by overrides.
	if ok, err := f.svc.UpdateMeeting(ctx, alice.customerID, aliceMeeting, Change{
		Occurrence: got[1].Occurrence, Status: ptr("cancelled")}); err != nil || !ok {
		t.Fatalf("cancel one = %v, %v", ok, err)
	}
	moved := time.Date(2026, 11, 18, 13, 0, 0, 0, time.UTC)
	if ok, err := f.svc.UpdateMeeting(ctx, alice.customerID, aliceMeeting, Change{
		Occurrence: "2026-11-17T09:00:00+01:00", StartTime: &moved}); err != nil || !ok {
		t.Fatalf("move one = %v, %v", ok, err)
	}
	got = f.occurrences(bob)
	if got[1].Status != "cancelled" || got[0].Status != "confirmed" || got[2].StartTime != "2026-11-18T13:00:00+00:00" ||
		got[2].EndTime != "2026-11-18T14:00:00+00:00" || got[2].Occurrence != "2026-11-17T08:00:00.000Z" {
		t.Errorf("after the overrides: %+v", got)
	}

	// The whole meeting cancelled, and confirmed again.
	f.svc.UpdateMeeting(ctx, alice.customerID, aliceMeeting, Change{Status: ptr("cancelled")})
	if got = f.occurrences(bob); got[0].Status != "cancelled" {
		t.Errorf("a cancelled meeting shows %q", got[0].Status)
	}
	f.svc.UpdateMeeting(ctx, alice.customerID, aliceMeeting, Change{Status: ptr("confirmed")})
	if got = f.occurrences(bob); got[0].Status != "confirmed" || got[1].Status != "cancelled" {
		t.Errorf("confirmed again: %q, and the cancelled occurrence %q", got[0].Status, got[1].Status)
	}

	// A new rule drops overrides for occurrences it no longer gives.
	if ok, err := f.svc.UpdateMeeting(ctx, alice.customerID, aliceMeeting, Change{
		RepeatRule: ptr("FREQ=WEEKLY;BYDAY=TU;COUNT=2")}); err != nil || !ok {
		t.Fatalf("change rule = %v, %v", ok, err)
	}
	if got = f.occurrences(bob); len(got) != 2 || got[1].Status != "cancelled" {
		t.Errorf("with two occurrences left: %+v", got)
	}

	// Deleting hides it from Bob, and the next revision brings it back.
	if ok, err := f.svc.DeleteMeeting(ctx, bob.customerID, bobMeeting); err != nil || !ok {
		t.Fatalf("DeleteMeeting = %v, %v", ok, err)
	}
	if got = f.occurrences(bob); len(got) != 0 {
		t.Errorf("a deleted meeting is still shown: %+v", got)
	}
	f.svc.UpdateMeeting(ctx, alice.customerID, aliceMeeting, Change{Title: ptr("Back")})
	if got = f.occurrences(bob); len(got) != 2 || got[0].Title != "Back" {
		t.Errorf("a new revision did not bring the meeting back: %+v", got)
	}

	// Documents travel with revisions; the organizer adds and removes them.
	if _, err := f.svc.AddDocument(ctx, bob.customerID, bobMeeting, "x.txt", "text/plain", []byte("x")); !errors.Is(err, ErrNotOrganizer) {
		t.Errorf("an attendee added a document: %v", err)
	}
	docID, err := f.svc.AddDocument(ctx, alice.customerID, aliceMeeting, "notes.txt", "text/plain", []byte("notes"))
	if err != nil || docID == 0 {
		t.Fatalf("AddDocument = %d, %v", docID, err)
	}
	got = f.occurrences(bob)
	if len(got[0].Documents) != 1 || got[0].Documents[0].DocumentID != docID {
		t.Fatalf("Bob's documents = %+v", got[0].Documents)
	}
	if doc, err := f.svc.GetDocument(ctx, bob.customerID, docID); err != nil || doc == nil || string(doc.FileData) != "notes" {
		t.Fatalf("GetDocument = %+v, %v", doc, err)
	}
	if ok, err := f.svc.DeleteDocument(ctx, alice.customerID, docID); err != nil || !ok {
		t.Fatalf("DeleteDocument = %v, %v", ok, err)
	}
	if got = f.occurrences(bob); len(got[0].Documents) != 0 {
		t.Errorf("a deleted document is in the latest revision: %+v", got[0].Documents)
	}

	// Removing Bob shows him the revision that removed him, cancelled, and
	// nothing the organizer changes afterwards.
	if ok, err := f.svc.RemoveAttendee(ctx, alice.customerID, aliceMeeting, "bob@example.test"); err != nil || !ok {
		t.Fatalf("RemoveAttendee = %v, %v", ok, err)
	}
	if _, err := f.svc.RemoveAttendee(ctx, alice.customerID, aliceMeeting, "alice@example.test"); !errors.Is(err, ErrBadInput) {
		t.Errorf("the organizer was removed from their own meeting: %v", err)
	}
	removedAt := f.occurrences(bob)[0].Revision
	f.svc.UpdateMeeting(ctx, alice.customerID, aliceMeeting, Change{Title: ptr("Without Bob")})
	got = f.occurrences(bob)
	if len(got) == 0 || got[0].Status != "cancelled" || got[0].Title == "Without Bob" || got[0].Revision != removedAt {
		t.Errorf("Bob, removed, sees %+v", got)
	}
	if ok, _ := f.svc.SetRSVP(ctx, bob.customerID, bobMeeting, "yes", ""); ok {
		t.Error("someone removed answered the meeting")
	}
	if got = f.occurrences(alice); len(got[0].Attendees) != 1 || got[0].Title != "Without Bob" || !got[0].IsOrganizer {
		t.Errorf("Alice sees %+v", got[0])
	}

	// Inviting Bob again.
	if ok, err := f.svc.AddAttendee(ctx, alice.customerID, aliceMeeting, "bob@example.test"); err != nil || !ok {
		t.Fatalf("AddAttendee = %v, %v", ok, err)
	}
	if got = f.occurrences(bob); got[0].Status != "confirmed" || got[0].Title != "Without Bob" {
		t.Errorf("Bob invited again sees %+v", got[0])
	}
}

// All-day meetings are whole days from midnight to midnight in their zone,
// whatever times the client sends.
func TestAllDayMeetingsAreWholeDays(t *testing.T) {
	f := newFixture(t, false)
	ctx := context.Background()
	alice := newAttendee(t, f.db, "alice@example.test", "Alice")
	start := time.Date(2026, 11, 3, 0, 0, 0, 0, time.FixedZone("CET", 3600))
	if _, err := f.svc.AddMeeting(ctx, alice.customerID, alice.mailboxID, NewMeeting{
		Title: "Offsite", StartTime: start, EndTime: start.Add(23*time.Hour + 59*time.Minute),
		TimeZone: "Europe/Amsterdam", IsAllDay: true,
	}); err != nil {
		t.Fatal(err)
	}
	got := f.occurrences(alice)
	if len(got) != 1 || got[0].StartTime != "2026-11-02T23:00:00+00:00" || got[0].EndTime != "2026-11-03T23:00:00+00:00" {
		t.Errorf("all-day meeting = %+v", got)
	}
	if _, err := f.svc.AddMeeting(ctx, alice.customerID, alice.mailboxID, NewMeeting{
		Title: "x", StartTime: start, EndTime: start.Add(time.Hour), TimeZone: "Mars/Olympus",
	}); !errors.Is(err, ErrBadInput) {
		t.Errorf("an unknown zone: %v", err)
	}
}

// Attendees on another Domain get the revision, one envelope per Domain;
// those whose Domain does not do calendar, or who are on none, are left out
// and the organizer is told (RFC 15.1). Answers go to every other Domain.
func TestMeetingsGoToTheAttendeesDomains(t *testing.T) {
	f := newFixture(t, true)
	ctx := context.Background()
	alice := newAttendee(t, f.db, "alice@example.test", "Alice")
	bob := newAttendee(t, f.db, "bob@example.test", "Bob")
	aliceMeeting, err := f.svc.AddMeeting(ctx, alice.customerID, alice.mailboxID, NewMeeting{
		Attendees: []string{"bob@example.test", "erin@peer.test", "pete@peer.test", "frank@nocal.test", "x@nowhere.test"},
		Title:     "Launch review", StartTime: launch, EndTime: launch.Add(time.Hour), TimeZone: "UTC",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.fed.meetings) != 1 {
		t.Fatalf("%d envelopes, want one for peer.test", len(f.fed.meetings))
	}
	out := f.fed.meetings[0]
	if out.PeerDomain != "peer.test" || strings.Join(out.Recipients, ",") != "erin@peer.test,pete@peer.test" ||
		strings.Join(out.Meeting.Attendees, ",") != "alice@example.test,bob@example.test,erin@peer.test,pete@peer.test" ||
		out.Meeting.Revision != 1 || out.Meeting.Organizer.Email != "alice@example.test" || out.OriginDomain != "example.test" {
		t.Errorf("envelope = %+v", out)
	}
	if len(f.notices.sent) != 1 || f.notices.sent[0].recipient != "alice@example.test" ||
		!strings.Contains(f.notices.sent[0].body, "frank@nocal.test, x@nowhere.test") ||
		f.notices.sent[0].subject != "Invitation failed: Launch review" {
		t.Errorf("notices = %+v", f.notices.sent)
	}

	bobMeeting := f.occurrences(bob)[0].MeetingID
	if ok, err := f.svc.SetRSVP(ctx, bob.customerID, bobMeeting, "maybe", ""); err != nil || !ok {
		t.Fatalf("SetRSVP = %v, %v", ok, err)
	}
	if len(f.fed.answers) != 1 || f.fed.answers[0].PeerDomain != "peer.test" ||
		strings.Join(f.fed.answers[0].Recipients, ",") != "erin@peer.test,pete@peer.test" ||
		f.fed.answers[0].AttendeeEmail != "bob@example.test" || f.fed.answers[0].Revision != 1 {
		t.Errorf("answers = %+v", f.fed.answers)
	}

	// Someone invited later hears Bob's answer from here, addressed to
	// them only (RFC 15.4).
	if ok, err := f.svc.AddAttendee(ctx, alice.customerID, aliceMeeting, "gina@other.test"); err != nil || !ok {
		t.Fatalf("AddAttendee = %v, %v", ok, err)
	}
	last := f.fed.answers[len(f.fed.answers)-1]
	if last.PeerDomain != "other.test" || strings.Join(last.Recipients, ",") != "gina@other.test" || last.Answer != "maybe" {
		t.Errorf("the newcomer was sent %+v", last)
	}
	if ok, _ := f.svc.AddAttendee(ctx, alice.customerID, aliceMeeting, "frank@nocal.test"); ok {
		t.Error("someone on a Domain without calendar was invited")
	}

	// Removing someone elsewhere tells their Domain, naming them removed.
	f.fed.meetings = nil
	if ok, err := f.svc.RemoveAttendee(ctx, alice.customerID, aliceMeeting, "pete@peer.test"); err != nil || !ok {
		t.Fatalf("RemoveAttendee = %v, %v", ok, err)
	}
	var toPeer *federation.MeetingOutbound
	for i := range f.fed.meetings {
		if f.fed.meetings[i].PeerDomain == "peer.test" {
			toPeer = &f.fed.meetings[i]
		}
	}
	if toPeer == nil || strings.Join(toPeer.Recipients, ",") != "erin@peer.test,pete@peer.test" ||
		strings.Join(toPeer.Meeting.Removed, ",") != "pete@peer.test" || contains(toPeer.Meeting.Attendees, "pete@peer.test") {
		t.Errorf("removal sent as %+v", toPeer)
	}

	// A peer that refuses an attendee as unknown has them dropped by a new
	// revision that does not name them removed, and the organizer is told.
	f.fed.meetings = nil
	gMeetingID, _ := store.SelectInt(ctx, f.db, `SELECT "g_meeting_id" FROM "meeting" WHERE "id" = ?`, aliceMeeting)
	if err := f.svc.MeetingUndelivered(ctx, federation.MeetingBounce{
		GMeetingID: gMeetingID, Title: "Launch review", Recipients: []string{"erin@peer.test"},
		PeerDomain: "peer.test", HTTPStatus: 202, ErrorCode: federation.ErrUnknownRecipient, Refused: true,
	}); err != nil {
		t.Fatal(err)
	}
	if len(f.fed.meetings) == 0 || contains(f.fed.meetings[0].Meeting.Attendees, "erin@peer.test") ||
		len(f.fed.meetings[0].Meeting.Removed) != 0 {
		t.Errorf("after the refusal: %+v", f.fed.meetings)
	}
	if n := f.notices.sent[len(f.notices.sent)-1]; !strings.Contains(n.body, "does not know them") {
		t.Errorf("the organizer was told %+v", n)
	}
	// Told twice, the meeting is not revised twice.
	before := len(f.fed.meetings)
	f.svc.MeetingUndelivered(ctx, federation.MeetingBounce{GMeetingID: gMeetingID, Recipients: []string{"erin@peer.test"}, Refused: true})
	if len(f.fed.meetings) != before {
		t.Error("a second refusal of someone already dropped made a revision")
	}
}

// wireMeeting is a revision as peer.test's organizer would send it.
func wireMeeting(revision int, title string, attendees ...string) federation.Meeting {
	return federation.Meeting{
		FederationID: "018f4d2b-9f21-7890-abcd-ef01234567c1",
		Revision:     revision,
		Status:       federation.StatusConfirmed,
		Organizer:    federation.Organizer{Email: "pete@peer.test", Name: "Pete"},
		Attendees:    append([]string{"pete@peer.test"}, attendees...),
		Title:        title,
		Start:        "2026-11-03T08:00:00.000Z",
		End:          "2026-11-03T09:00:00.000Z",
		TimeZone:     "Europe/Amsterdam",
		Repeat:       &rrule.Repeat{Frequency: "weekly", Count: ptr(2)},
	}
}

// A meeting from another Domain is created for its recipients, replaced by
// each higher revision whatever order they arrive in, and refused when it
// names another organizer (RFC 15.3).
func TestReceivingRevisions(t *testing.T) {
	f := newFixture(t, true)
	ctx := context.Background()
	bob := newAttendee(t, f.db, "bob@example.test", "Bob")

	delivered, refused, err := f.svc.DeliverMeeting(ctx, federation.MeetingInbound{
		OriginDomain: "peer.test",
		Meeting:      wireMeeting(2, "Second", "bob@example.test", "nobody@example.test"),
		Attachments:  []federation.InboundAttachment{{FederationID: "018f4d2b-9f21-7890-abcd-ef01234567d1", FileName: "a.txt", MimeType: "text/plain", Data: []byte("a")}},
		Recipients:   []string{"bob@example.test", "nobody@example.test"},
	})
	if err != nil || delivered != 1 || strings.Join(refused, ",") != "nobody@example.test" {
		t.Fatalf("DeliverMeeting = %d, %v, %v", delivered, refused, err)
	}
	got := f.occurrences(bob)
	if len(got) != 2 || got[0].Title != "Second" || got[0].OrganizerEmail != "pete@peer.test" || len(got[0].Documents) != 1 {
		t.Fatalf("Bob sees %+v", got)
	}
	if refused, _ := f.svc.UndeliveredMeeting(ctx, federation.MeetingInbound{Recipients: []string{"bob@example.test", "nobody@example.test"}}); strings.Join(refused, ",") != "nobody@example.test" {
		t.Errorf("UndeliveredMeeting = %v", refused)
	}

	// An earlier revision arriving late is kept and changes nothing shown.
	if _, _, err := f.svc.DeliverMeeting(ctx, federation.MeetingInbound{
		Meeting: wireMeeting(1, "First", "bob@example.test"), Recipients: []string{"bob@example.test"},
	}); err != nil {
		t.Fatal(err)
	}
	if got = f.occurrences(bob); got[0].Title != "Second" {
		t.Errorf("a late revision replaced a later one: %q", got[0].Title)
	}
	if n, _ := store.SelectInt(ctx, f.db, `SELECT COUNT(*) FROM "g_meeting_revision"`); n != 2 {
		t.Errorf("%d revisions held, want both", n)
	}

	// Another organizer for the same meeting is refused.
	forged := wireMeeting(9, "Mine", "bob@example.test")
	forged.Organizer.Email = "mallory@peer.test"
	if _, _, err := f.svc.DeliverMeeting(ctx, federation.MeetingInbound{Meeting: forged,
		Recipients: []string{"bob@example.test"}}); !errors.Is(err, federation.ErrWrongOrganizer) {
		t.Errorf("another organizer: %v", err)
	}

	// Bob's answer goes back to the organizer's Domain, and an answer from
	// someone there is recorded.
	bobMeeting := got[0].MeetingID
	if ok, err := f.svc.SetRSVP(ctx, bob.customerID, bobMeeting, "yes", ""); err != nil || !ok {
		t.Fatalf("SetRSVP = %v, %v", ok, err)
	}
	if len(f.fed.answers) != 1 || f.fed.answers[0].PeerDomain != "peer.test" || f.fed.answers[0].Revision != 2 ||
		f.fed.answers[0].OrganizerEmail != "pete@peer.test" {
		t.Errorf("Bob's answer = %+v", f.fed.answers)
	}
	f.fed.answers = nil

	// A revision that invites someone on a third Domain makes this Server
	// send them Bob's answer.
	third := wireMeeting(3, "Third", "bob@example.test", "gina@other.test")
	if _, _, err := f.svc.DeliverMeeting(ctx, federation.MeetingInbound{Meeting: third,
		Recipients: []string{"bob@example.test"}}); err != nil {
		t.Fatal(err)
	}
	if len(f.fed.answers) != 1 || f.fed.answers[0].PeerDomain != "other.test" ||
		strings.Join(f.fed.answers[0].Recipients, ",") != "gina@other.test" || f.fed.answers[0].AttendeeEmail != "bob@example.test" {
		t.Errorf("answers for the newcomer = %+v", f.fed.answers)
	}

	answer := federation.RSVPInbound{
		OriginDomain: "other.test", MeetingFedID: third.FederationID, OrganizerEmail: "pete@peer.test",
		AttendeeEmail: "gina@other.test", Answer: "no", Revision: 3, AnsweredAt: f.now, Occurrence: "2026-11-10T08:00:00.000Z",
		Recipients: []string{"bob@example.test"},
	}
	if err := f.svc.ReceiveRSVP(ctx, answer); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ReceiveRSVP(ctx, answer); err != nil {
		t.Fatal(err)
	}
	got = f.occurrences(bob)
	if a, _ := answerOf(got[1], "gina@other.test"); a != "no" {
		t.Errorf("Gina's answer for the 10th = %q", a)
	}
	if a, _ := answerOf(got[0], "gina@other.test"); a != "" {
		t.Errorf("Gina answered the 3rd as %q", a)
	}
	if n, _ := store.SelectInt(ctx, f.db, `SELECT COUNT(*) FROM "g_meeting_rsvp" WHERE "email" = 'gina@other.test'`); n != 1 {
		t.Errorf("the same answer twice is %d rows", n)
	}

	unknownMeeting := answer
	unknownMeeting.MeetingFedID = "018f4d2b-9f21-7890-abcd-ef0123456799"
	if err := f.svc.ReceiveRSVP(ctx, unknownMeeting); !errors.Is(err, federation.ErrNotMember) {
		t.Errorf("an answer for a meeting not held: %v", err)
	}
	early := answer
	early.AttendeeEmail, early.Revision = "hank@other.test", 4
	if err := f.svc.ReceiveRSVP(ctx, early); !errors.Is(err, federation.ErrNotMember) {
		t.Errorf("an answer from someone a later revision invites: %v", err)
	}
	late := answer
	late.AttendeeEmail, late.Revision = "hank@other.test", 2
	if err := f.svc.ReceiveRSVP(ctx, late); err != nil {
		t.Errorf("an answer from someone no longer invited: %v", err)
	}
	wrong := answer
	wrong.OrganizerEmail = "mallory@peer.test"
	if err := f.svc.ReceiveRSVP(ctx, wrong); !errors.Is(err, federation.ErrWrongOrganizer) {
		t.Errorf("an answer naming another organizer: %v", err)
	}

	// A revision that leaves Bob out shows it to him cancelled, and a deleted
	// copy comes back with a new revision.
	if _, err := f.svc.DeleteMeeting(ctx, bob.customerID, bobMeeting); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.DeliverMeeting(ctx, federation.MeetingInbound{Meeting: wireMeeting(4, "Fourth", "bob@example.test"),
		Recipients: []string{"bob@example.test"}}); err != nil {
		t.Fatal(err)
	}
	if got = f.occurrences(bob); len(got) == 0 || got[0].Title != "Fourth" {
		t.Errorf("a new revision did not bring back a deleted meeting: %+v", got)
	}
	out := wireMeeting(6, "Sixth")
	if _, _, err := f.svc.DeliverMeeting(ctx, federation.MeetingInbound{Meeting: out,
		Recipients: []string{"pete@peer.test"}}); err != nil {
		t.Fatal(err)
	}
	// Revision 5, which removed him, never came; 6 is what told this Domain.
	if got = f.occurrences(bob); len(got) == 0 || got[0].Status != "cancelled" || got[0].Revision != 6 {
		t.Errorf("Bob, left out by a later revision, sees %+v", got)
	}
	if ok, _ := f.svc.SetRSVP(ctx, bob.customerID, bobMeeting, "yes", ""); ok {
		t.Error("someone left out answered")
	}
}
