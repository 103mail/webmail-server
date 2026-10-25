package federation

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/103mail/webmail-server/internal/rrule"
	"github.com/103mail/webmail-server/internal/store"
	"github.com/103mail/webmail-server/internal/store/storetest"
)

func intPtr(n int) *int { return &n }

// validMeeting is the example of RFC 15.2.
func validMeeting() MeetingEnvelope {
	attachments := []Attachment{}
	return MeetingEnvelope{
		Version:      ChatVersion,
		Kind:         KindMeeting,
		EnvelopeID:   "018f4d2b-9f21-7890-abcd-ef01234567c0",
		OriginDomain: "103mail.com",
		OriginSentAt: FormatTimestamp(time.Now()),
		Meeting: Meeting{
			FederationID: "018f4d2b-9f21-7890-abcd-ef01234567c1",
			Revision:     3,
			Status:       StatusConfirmed,
			Organizer:    Organizer{Email: "alice@103mail.com", Name: "Alice"},
			Attendees:    []string{"alice@103mail.com", "bob@example.com", "erin@mail.acme.io"},
			Title:        "Launch review",
			Description:  "Walk through the open items",
			Start:        "2026-11-03T08:00:00Z",
			End:          "2026-11-03T09:00:00Z",
			TimeZone:     "Europe/Amsterdam",
			Repeat:       &rrule.Repeat{Frequency: "weekly", Weekdays: []string{"tu"}, Count: intPtr(6)},
			Overrides:    []rrule.Override{{Occurrence: "2026-11-10T08:00:00Z", Status: StatusCancelled}},
		},
		Attachments: &attachments,
		Recipients:  []string{"bob@example.com"},
	}
}

// validAnswer is the example of RFC 15.5.
func validAnswer() RSVPEnvelope {
	return RSVPEnvelope{
		Version:      ChatVersion,
		Kind:         KindMeetingRSVP,
		EnvelopeID:   "018f4d2b-9f21-7890-abcd-ef01234567d0",
		OriginDomain: "example.com",
		OriginSentAt: FormatTimestamp(time.Now()),
		Meeting: MeetingRef{FederationID: "018f4d2b-9f21-7890-abcd-ef01234567c1",
			Organizer: Organizer{Email: "alice@103mail.com"}},
		RSVP: &RSVP{AttendeeEmail: "bob@example.com", Answer: "yes", Revision: 3,
			AnsweredAt: "2026-10-25T10:19:58.000Z"},
		Recipients: []string{"alice@103mail.com"},
	}
}

// Every field rule of RFC 15.2, 15.5 and 15.6 rejects, with the code that
// section gives it.
func TestMeetingEnvelopeFieldRules(t *testing.T) {
	if e := validMeeting(); e.Validate(DefaultMaxAttachmentBytes) != nil {
		t.Fatalf("the RFC's meeting was rejected: %v", e.Validate(DefaultMaxAttachmentBytes))
	}
	if e := validAnswer(); e.Validate() != nil {
		t.Fatalf("the RFC's answer was rejected: %v", e.Validate())
	}
	allDay := func(e *MeetingEnvelope) {
		e.Meeting.AllDay = true
		e.Meeting.Start, e.Meeting.End = "2026-11-02T23:00:00Z", "2026-11-03T23:00:00Z"
	}
	for name, mutate := range map[string]func(*MeetingEnvelope){
		"an all-day meeting": allDay,
		"a single meeting":   func(e *MeetingEnvelope) { e.Meeting.Repeat, e.Meeting.Overrides = nil, nil },
		"a removal sent to the one removed": func(e *MeetingEnvelope) {
			e.Meeting.Attendees = e.Meeting.Attendees[:1]
			e.Meeting.Removed = []string{"bob@example.com"}
		},
		"a cancelled meeting": func(e *MeetingEnvelope) { e.Meeting.Status = StatusCancelled },
	} {
		e := validMeeting()
		mutate(&e)
		if r := e.Validate(DefaultMaxAttachmentBytes); r != nil {
			t.Errorf("%s was rejected: %v", name, r)
		}
	}

	for _, tt := range []struct {
		name   string
		mutate func(*MeetingEnvelope)
		code   string
	}{
		{"version 0.2", func(e *MeetingEnvelope) { e.Version = Version }, ErrUnsupportedVersion},
		{"a thread", func(e *MeetingEnvelope) { e.Thread = json.RawMessage(`{}`) }, ErrSchemaViolation},
		{"a message", func(e *MeetingEnvelope) { e.Message = json.RawMessage(`{}`) }, ErrSchemaViolation},
		{"revision zero", func(e *MeetingEnvelope) { e.Meeting.Revision = 0 }, ErrSchemaViolation},
		{"an unknown status", func(e *MeetingEnvelope) { e.Meeting.Status = "tentative" }, ErrSchemaViolation},
		{"an organizer on another Domain", func(e *MeetingEnvelope) {
			e.Meeting.Organizer.Email = "bob@example.com"
		}, ErrSchemaViolation},
		{"no attendees", func(e *MeetingEnvelope) { e.Meeting.Attendees = nil }, ErrSchemaViolation},
		{"attendees without the organizer", func(e *MeetingEnvelope) {
			e.Meeting.Attendees = []string{"bob@example.com"}
		}, ErrSchemaViolation},
		{"someone removed and attending", func(e *MeetingEnvelope) { e.Meeting.Removed = []string{"bob@example.com"} }, ErrSchemaViolation},
		{"a recipient neither attending nor removed", func(e *MeetingEnvelope) {
			e.Recipients = []string{"carol@example.com"}
		}, ErrSchemaViolation},
		{"a long title", func(e *MeetingEnvelope) { e.Meeting.Title = strings.Repeat("x", 256) }, ErrSchemaViolation},
		{"an end before the start", func(e *MeetingEnvelope) { e.Meeting.End = "2026-11-03T07:00:00Z" }, ErrSchemaViolation},
		{"a start that is not UTC", func(e *MeetingEnvelope) { e.Meeting.Start = "2026-11-03T09:00:00+01:00" }, ErrSchemaViolation},
		{"an unknown zone", func(e *MeetingEnvelope) { e.Meeting.TimeZone = "Mars/Olympus" }, ErrUnknownTimeZone},
		{"the Local zone", func(e *MeetingEnvelope) { e.Meeting.TimeZone = "Local" }, ErrUnknownTimeZone},
		{"an all-day meeting not at midnight", func(e *MeetingEnvelope) {
			allDay(e)
			e.Meeting.Start = "2026-11-03T08:00:00Z"
			e.Meeting.End = "2026-11-04T08:00:00Z"
		}, ErrSchemaViolation},
		{"count and until", func(e *MeetingEnvelope) { e.Meeting.Repeat.Until = "2026-12-31T23:00:00Z" }, ErrUnsupportedRepeat},
		{"overrides without repeat", func(e *MeetingEnvelope) { e.Meeting.Repeat = nil }, ErrUnsupportedRepeat},
		{"no attachments member", func(e *MeetingEnvelope) { e.Attachments = nil }, ErrSchemaViolation},
	} {
		e := validMeeting()
		tt.mutate(&e)
		if r := e.Validate(DefaultMaxAttachmentBytes); r == nil || r.Code != tt.code {
			t.Errorf("%s: got %v, want %s", tt.name, r, tt.code)
		}
	}

	for _, tt := range []struct {
		name   string
		mutate func(*RSVPEnvelope)
	}{
		{"an attendee on another Domain", func(e *RSVPEnvelope) { e.RSVP.AttendeeEmail = "erin@mail.acme.io" }},
		{"the organizer answering", func(e *RSVPEnvelope) {
			e.OriginDomain = "103mail.com"
			e.RSVP.AttendeeEmail = "alice@103mail.com"
		}},
		{"an unknown answer", func(e *RSVPEnvelope) { e.RSVP.Answer = "perhaps" }},
		{"revision zero", func(e *RSVPEnvelope) { e.RSVP.Revision = 0 }},
		{"attachments", func(e *RSVPEnvelope) { e.Attachments = json.RawMessage(`[]`) }},
		{"an occurrence that is not a time", func(e *RSVPEnvelope) { e.RSVP.Occurrence = "next week" }},
		{"no rsvp", func(e *RSVPEnvelope) { e.RSVP = nil }},
	} {
		e := validAnswer()
		tt.mutate(&e)
		if r := e.Validate(); r == nil || r.Code != ErrSchemaViolation {
			t.Errorf("%s: got %v, want %s", tt.name, r, ErrSchemaViolation)
		}
	}
}

// recordingCalendar keeps what the inbound endpoint and the outbox ask of the
// calendar domain.
type recordingCalendar struct {
	meetings   []MeetingInbound
	answers    []RSVPInbound
	bounces    []MeetingBounce
	refuse     []string
	err        error
	undelivers int
}

func (c *recordingCalendar) DeliverMeeting(ctx context.Context, in MeetingInbound) (int, []string, error) {
	if c.err != nil {
		return 0, nil, c.err
	}
	c.meetings = append(c.meetings, in)
	return len(in.Recipients) - len(c.refuse), c.refuse, nil
}
func (c *recordingCalendar) UndeliveredMeeting(ctx context.Context, in MeetingInbound) ([]string, error) {
	c.undelivers++
	return c.refuse, nil
}
func (c *recordingCalendar) ReceiveRSVP(ctx context.Context, in RSVPInbound) error {
	if c.err != nil {
		return c.err
	}
	c.answers = append(c.answers, in)
	return nil
}
func (c *recordingCalendar) MeetingUndelivered(ctx context.Context, b MeetingBounce) error {
	c.bounces = append(c.bounces, b)
	return nil
}

// A Domain advertises calendar, and 0.3, only with the calendar domain
// attached, and a peer's advertisement is remembered with its descriptor
// (RFC 15.1).
func TestCalendarIsAdvertisedAndRemembered(t *testing.T) {
	db := storetest.Open(t)
	ctx := context.Background()
	s := newStoreService(t, db)
	if d, _ := s.DescriptorFor("example.test"); d.Calendar || !slices.Equal(d.Versions, []string{Version}) {
		t.Errorf("without calendar: %+v", d)
	}
	s.AttachCalendar(&recordingCalendar{})
	if d, _ := s.DescriptorFor("example.test"); !d.Calendar || d.Chat || !slices.Equal(d.Versions, []string{Version, ChatVersion}) {
		t.Errorf("with calendar: %+v", d)
	}

	peer := &Descriptor{Domain: "peer.test", PublicKey: EncodePublicKey(make([]byte, 32)),
		Versions: []string{Version, ChatVersion}, Chat: true, Calendar: true}
	if err := s.storePeer(ctx, peer); err != nil {
		t.Fatal(err)
	}
	cached, _, _, err := s.cachedPeer(ctx, "peer.test")
	if err != nil || !cached.Chat || !cached.Calendar {
		t.Errorf("the cached descriptor forgot its extensions: %+v, %v", cached, err)
	}
	if ok, err := s.Calendar(ctx, "peer.test"); err != nil || !ok {
		t.Errorf("Calendar(peer.test) = %v, %v", ok, err)
	}
}

func meetingRequest(t *testing.T, e any, origin string) *Request {
	t.Helper()
	body, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return &Request{Sender: origin, Destination: "example.test", Version: ChatVersion, Body: body}
}

func meetingFrom(origin string) MeetingEnvelope {
	e := validMeeting()
	e.OriginDomain = origin
	e.Meeting.Organizer.Email = "alice@" + origin
	e.Meeting.Attendees = []string{"alice@" + origin, "bob@example.test", "carol@example.test"}
	e.Recipients = []string{"bob@example.test", "carol@example.test"}
	return e
}

// An authenticated meeting reaches the calendar domain once, an unknown
// member of repeat is refused, the calendar domain's refusals carry their
// codes, and a Server without calendar answers unsupported_kind (RFC 15).
func TestInboundMeetings(t *testing.T) {
	db := storetest.Open(t)
	ctx := context.Background()
	s := newStoreService(t, db)
	if _, r := s.receiveMeeting(ctx, meetingRequest(t, meetingFrom("peer.test"), "peer.test"), KindMeeting); r == nil ||
		r.Code != ErrUnsupportedKind {
		t.Errorf("a Server without calendar accepted a meeting: %v", r)
	}
	c := &recordingCalendar{}
	s.AttachCalendar(c)

	e := meetingFrom("peer.test")
	e.Meeting.Start = "2026-11-03T08:00:00.000Z"
	a, r := s.receiveMeeting(ctx, meetingRequest(t, e, "peer.test"), KindMeeting)
	if r != nil || a.EnvelopeID != e.EnvelopeID || len(c.meetings) != 1 {
		t.Fatalf("receive = %+v, %v", a, r)
	}
	if got := c.meetings[0]; got.Meeting.Start != "2026-11-03T08:00:00.000Z" || got.Meeting.Overrides[0].Occurrence != "2026-11-10T08:00:00.000Z" ||
		got.Meeting.Revision != 3 || got.OriginDomain != "peer.test" {
		t.Errorf("delivered %+v", got)
	}
	if _, r := s.receiveMeeting(ctx, meetingRequest(t, e, "peer.test"), KindMeeting); r != nil || len(c.meetings) != 1 || c.undelivers != 1 {
		t.Errorf("a replay was delivered again: %v", r)
	}

	// A member of repeat this Domain does not know is refused, not ignored.
	raw, _ := json.Marshal(meetingFrom("peer.test"))
	raw = []byte(strings.Replace(string(raw), `"frequency":"weekly"`, `"frequency":"weekly","by_set_pos":[-1]`, 1))
	raw = []byte(strings.Replace(string(raw), e.EnvelopeID, "018f4d2b-9f21-7890-abcd-ef01234567e1", 1))
	if _, r := s.receiveMeeting(ctx, &Request{Sender: "peer.test", Destination: "example.test", Body: raw}, KindMeeting); r == nil ||
		r.Code != ErrUnsupportedRepeat {
		t.Errorf("an unknown member of repeat: %v", r)
	}

	forged := meetingFrom("peer.test")
	forged.EnvelopeID = "018f4d2b-9f21-7890-abcd-ef01234567e2"
	if _, r := s.receiveMeeting(ctx, meetingRequest(t, forged, "other.test"), KindMeeting); r == nil || r.Code != ErrSenderMismatch {
		t.Errorf("a meeting sent by a Domain that is not the organizer's: %v", r)
	}
	elsewhere := meetingFrom("peer.test")
	elsewhere.EnvelopeID = "018f4d2b-9f21-7890-abcd-ef01234567e3"
	elsewhere.Meeting.Attendees = append(elsewhere.Meeting.Attendees, "dave@other.test")
	elsewhere.Recipients = []string{"dave@other.test"}
	if _, r := s.receiveMeeting(ctx, meetingRequest(t, elsewhere, "peer.test"), KindMeeting); r == nil || r.Code != ErrNotRegistrar {
		t.Errorf("a recipient on another Domain: %v", r)
	}

	c.err = ErrWrongOrganizer
	mismatch := meetingFrom("peer.test")
	mismatch.EnvelopeID = "018f4d2b-9f21-7890-abcd-ef01234567e4"
	if _, r := s.receiveMeeting(ctx, meetingRequest(t, mismatch, "peer.test"), KindMeeting); r == nil ||
		r.Status != 400 || r.Code != ErrOrganizerMismatch {
		t.Errorf("another organizer: %v", r)
	}

	// Answers: one from the Domain's own attendee is recorded; one for a
	// meeting not held yet is answered 425 and not recorded as accepted.
	c.err = nil
	answer := validAnswer()
	answer.OriginDomain = "peer.test"
	answer.RSVP.AttendeeEmail = "pete@peer.test"
	answer.RSVP.Occurrence = "2026-11-10T08:00:00Z"
	answer.Recipients = []string{"bob@example.test"}
	if _, r := s.receiveMeeting(ctx, meetingRequest(t, answer, "peer.test"), KindMeetingRSVP); r != nil || len(c.answers) != 1 ||
		c.answers[0].Occurrence != "2026-11-10T08:00:00.000Z" || c.answers[0].AttendeeEmail != "pete@peer.test" {
		t.Fatalf("answer = %v, %+v", r, c.answers)
	}
	c.err = ErrNotMember
	early := answer
	early.EnvelopeID = "018f4d2b-9f21-7890-abcd-ef01234567e5"
	if _, r := s.receiveMeeting(ctx, meetingRequest(t, early, "peer.test"), KindMeetingRSVP); r == nil ||
		r.Status != http.StatusTooEarly || r.Code != ErrNotAMember {
		t.Errorf("an answer for a meeting not held: %v", r)
	}
	if seen, _ := s.alreadyAccepted(ctx, "peer.test", early.EnvelopeID); seen {
		t.Error("a refused answer was recorded as accepted, so its retry would be a replay")
	}
	speaksForOthers := answer
	speaksForOthers.EnvelopeID = "018f4d2b-9f21-7890-abcd-ef01234567e6"
	if _, r := s.receiveMeeting(ctx, meetingRequest(t, speaksForOthers, "other.test"), KindMeetingRSVP); r == nil ||
		r.Code != ErrSenderMismatch {
		t.Errorf("an answer from a Domain for someone else's user: %v", r)
	}
}

func meetingOut(peer string, revision int) MeetingOutbound {
	e := validMeeting()
	e.Meeting.Organizer.Email = "alice@example.test"
	e.Meeting.Attendees = []string{"alice@example.test", "bob@" + peer}
	e.Meeting.Revision = revision
	return MeetingOutbound{PeerDomain: peer, OriginDomain: "example.test", Meeting: e.Meeting,
		Recipients: []string{"bob@" + peer}, RelatedID: 7}
}

// What EnqueueMeeting and EnqueueRSVP write is valid, a later revision for
// the same peer and recipients replaces one not yet sent, and so does a
// later answer by the same attendee (RFC 15.3, 15.5). Both go out on the
// mail loop.
func TestEnqueuedMeetingsSupersedeEachOther(t *testing.T) {
	db := storetest.Open(t)
	ctx := context.Background()
	s := newStoreService(t, db)
	enqueue := func(fn func(tx *sql.Tx) error) {
		t.Helper()
		if err := db.Tx(ctx, fn); err != nil {
			t.Fatal(err)
		}
	}
	enqueue(func(tx *sql.Tx) error { return s.EnqueueMeeting(ctx, tx, meetingOut("peer.test", 1)) })
	enqueue(func(tx *sql.Tx) error { return s.EnqueueMeeting(ctx, tx, meetingOut("other.test", 1)) })
	enqueue(func(tx *sql.Tx) error { return s.EnqueueMeeting(ctx, tx, meetingOut("peer.test", 2)) })
	answer := RSVPOutbound{PeerDomain: "peer.test", OriginDomain: "example.test",
		MeetingFedID: "018f4d2b-9f21-7890-abcd-ef01234567c1", OrganizerEmail: "pete@peer.test",
		AttendeeEmail: "alice@example.test", Answer: "yes", Revision: 2, AnsweredAt: time.Now(),
		Recipients: []string{"pete@peer.test"}, RelatedID: 8}
	enqueue(func(tx *sql.Tx) error { return s.EnqueueRSVP(ctx, tx, answer) })
	answer.Answer = "no"
	enqueue(func(tx *sql.Tx) error { return s.EnqueueRSVP(ctx, tx, answer) })
	answer.Occurrence = "2026-11-10T08:00:00.000Z"
	enqueue(func(tx *sql.Tx) error { return s.EnqueueRSVP(ctx, tx, answer) })

	rows, err := db.QueryContext(ctx, `SELECT "kind", "peer_domain", "status", "body" FROM "federation_outbox" ORDER BY "id"`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var statuses []string
	for rows.Next() {
		var kind, peer, status, body string
		if err := rows.Scan(&kind, &peer, &status, &body); err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, kind+"/"+peer+"/"+status)
		if kind == outboxMeeting {
			var e MeetingEnvelope
			if err := json.Unmarshal([]byte(body), &e); err != nil {
				t.Fatal(err)
			}
			if r := e.Validate(DefaultMaxAttachmentBytes); r != nil {
				t.Errorf("an enqueued meeting is invalid: %v\n%s", r, body)
			}
		} else {
			var e RSVPEnvelope
			if err := json.Unmarshal([]byte(body), &e); err != nil {
				t.Fatal(err)
			}
			e.OriginDomain = "example.test"
			if r := e.Validate(); r != nil {
				t.Errorf("an enqueued answer is invalid: %v\n%s", r, body)
			}
		}
	}
	want := []string{
		"meeting/peer.test/superseded", "meeting/other.test/pending", "meeting/peer.test/pending",
		"meeting_rsvp/peer.test/superseded", "meeting_rsvp/peer.test/pending", "meeting_rsvp/peer.test/pending",
	}
	if !slices.Equal(statuses, want) {
		t.Errorf("outbox = %v\nwant     %v", statuses, want)
	}
	if peers, _ := s.duePeers(ctx, false); len(peers) != 2 {
		t.Errorf("the mail loop sees %v, want both peers", peers)
	}
}

// A revision's outcome reaches the calendar domain: refused attendees and a
// Domain without calendar are dropped, a timeout promises a resend, and an
// answer that fails is only logged.
func TestMeetingOutcomesReachTheCalendarDomain(t *testing.T) {
	db := storetest.Open(t)
	ctx := context.Background()
	s := newStoreService(t, db)
	c := &recordingCalendar{}
	s.AttachCalendar(c)
	out := meetingOut("peer.test", 2)
	out.Meeting.Attendees = append(out.Meeting.Attendees, "carol@peer.test")
	out.Meeting.Removed = []string{"dave@peer.test"}
	out.Recipients = []string{"bob@peer.test", "carol@peer.test", "dave@peer.test"}
	if err := db.Tx(ctx, func(tx *sql.Tx) error { return s.EnqueueMeeting(ctx, tx, out) }); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.claim(ctx, "peer.test", 10, false)
	if len(rows) != 1 || rows[0].Kind != outboxMeeting || rows[0].RelatedID != 7 {
		t.Fatalf("claimed %+v", rows)
	}

	s.afterMeetingAttempt(ctx, rows[0], 202, "", []Refusal{
		{Address: "carol@peer.test", Error: ErrUnknownRecipient},
		{Address: "dave@peer.test", Error: ErrUnknownRecipient},
		{Address: "mallory@peer.test", Error: ErrUnknownRecipient},
	}, false, false)
	if len(c.bounces) != 1 || !c.bounces[0].Refused || !slices.Equal(c.bounces[0].Recipients, []string{"carol@peer.test"}) ||
		c.bounces[0].GMeetingID != 7 || c.bounces[0].Title != "Launch review" {
		t.Errorf("refusals = %+v, want carol only: dave was removed and mallory not sent it", c.bounces)
	}

	s.afterMeetingAttempt(ctx, rows[0], 400, ErrUnsupportedKind, nil, true, false)
	if b := c.bounces[len(c.bounces)-1]; !b.Unreachable || !slices.Equal(b.Recipients, []string{"bob@peer.test", "carol@peer.test"}) {
		t.Errorf("a Domain without calendar = %+v", b)
	}
	s.afterMeetingAttempt(ctx, rows[0], 0, "timed_out", nil, false, true)
	if b := c.bounces[len(c.bounces)-1]; !b.Backfill || b.Refused || len(b.Recipients) != 3 {
		t.Errorf("a timeout = %+v", b)
	}
	s.afterMeetingAttempt(ctx, rows[0], 202, "", nil, false, false)
	before := len(c.bounces)

	answer := RSVPOutbound{PeerDomain: "peer.test", OriginDomain: "example.test",
		MeetingFedID: "018f4d2b-9f21-7890-abcd-ef01234567c1", OrganizerEmail: "pete@peer.test",
		AttendeeEmail: "alice@example.test", Answer: "yes", Revision: 2, AnsweredAt: time.Now(),
		Recipients: []string{"pete@peer.test"}}
	if err := db.Tx(ctx, func(tx *sql.Tx) error { return s.EnqueueRSVP(ctx, tx, answer) }); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.claim(ctx, "peer.test", 10, false)
	s.fail(ctx, rows[0], 400, ErrSchemaViolation)
	if len(c.bounces) != before {
		t.Errorf("a failed answer bounced: %+v", c.bounces[before:])
	}
	if n, _ := store.SelectInt(ctx, db, `SELECT COUNT(*) FROM "federation_outbox" WHERE "status" = 'permanent_failure'`); n != 1 {
		t.Errorf("%d failed rows, want the answer", n)
	}
}
