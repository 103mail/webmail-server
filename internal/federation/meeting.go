package federation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/103mail/webmail-server/internal/rrule"
)

// The kinds of envelope the calendar extension adds, on version 0.3 (RFC
// 15.2, 15.5). They are drained by the mail loop and retried like mail,
// because a meeting may lie days ahead (RFC 15.3).
const (
	KindMeeting     = "meeting"
	KindMeetingRSVP = "meeting_rsvp"
)

// Error codes of the calendar extension (RFC 15.2, 15.3, 15.6).
const (
	ErrOrganizerMismatch = "organizer_mismatch"
	ErrUnknownTimeZone   = "unknown_time_zone"
	ErrUnsupportedRepeat = "unsupported_repeat"
)

// ErrWrongOrganizer is what a CalendarDeliverer's error matches when an
// envelope names another organizer for a meeting this Server holds.
var ErrWrongOrganizer = errors.New("the meeting has another organizer")

// Field bounds of RFC 15.2 and 15.6.
const (
	maxTitleBytes    = 255
	maxTimeZoneBytes = 255
)

// MeetingEnvelope carries a meeting as its organizer's Domain says it now is
// (RFC 15.2). Thread, chat and message are read only to refuse an envelope
// that carries one.
type MeetingEnvelope struct {
	Version      string          `json:"version"`
	Kind         string          `json:"kind"`
	EnvelopeID   string          `json:"envelope_id"`
	OriginDomain string          `json:"origin_domain"`
	OriginSentAt string          `json:"origin_sent_at"`
	Thread       json.RawMessage `json:"thread,omitempty"`
	Chat         json.RawMessage `json:"chat,omitempty"`
	Message      json.RawMessage `json:"message,omitempty"`
	RSVP         json.RawMessage `json:"rsvp,omitempty"`
	Meeting      Meeting         `json:"meeting"`
	Attachments  *[]Attachment   `json:"attachments"`
	Recipients   []string        `json:"recipients"`
}

// Meeting is the whole meeting at one revision (RFC 15.2).
type Meeting struct {
	FederationID    string           `json:"federation_id"`
	Revision        int              `json:"revision"`
	Status          string           `json:"status"`
	Organizer       Organizer        `json:"organizer"`
	Attendees       []string         `json:"attendees"`
	Removed         []string         `json:"removed,omitempty"`
	Title           string           `json:"title"`
	Description     string           `json:"description"`
	DescriptionHTML string           `json:"description_html,omitempty"`
	Start           string           `json:"start"`
	End             string           `json:"end"`
	TimeZone        string           `json:"time_zone"`
	AllDay          bool             `json:"all_day,omitempty"`
	Repeat          *rrule.Repeat    `json:"repeat,omitempty"`
	Overrides       []rrule.Override `json:"overrides,omitempty"`
}

// Organizer is the one person whose Domain says what the meeting is.
type Organizer struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

// The statuses of a meeting, or of one occurrence.
const (
	StatusConfirmed = rrule.StatusConfirmed
	StatusCancelled = rrule.StatusCancelled
)

// RSVPEnvelope is one attendee's answer, from their own Domain (RFC 15.5).
type RSVPEnvelope struct {
	Version      string          `json:"version"`
	Kind         string          `json:"kind"`
	EnvelopeID   string          `json:"envelope_id"`
	OriginDomain string          `json:"origin_domain"`
	OriginSentAt string          `json:"origin_sent_at"`
	Thread       json.RawMessage `json:"thread,omitempty"`
	Chat         json.RawMessage `json:"chat,omitempty"`
	Message      json.RawMessage `json:"message,omitempty"`
	Attachments  json.RawMessage `json:"attachments,omitempty"`
	Meeting      MeetingRef      `json:"meeting"`
	RSVP         *RSVP           `json:"rsvp"`
	Recipients   []string        `json:"recipients"`
}

// MeetingRef names the meeting an answer is for. Every other member of the
// meeting is ignored: an answer never changes the meeting (RFC 15.5).
type MeetingRef struct {
	FederationID string    `json:"federation_id"`
	Organizer    Organizer `json:"organizer"`
}

// RSVP is the answer itself.
type RSVP struct {
	AttendeeEmail string `json:"attendee_email"`
	Answer        string `json:"answer"`
	Revision      int    `json:"revision"`
	AnsweredAt    string `json:"answered_at"`
	Occurrence    string `json:"occurrence,omitempty"`
}

// absent reports whether a member that must be left out was.
func absent(raw json.RawMessage) bool { return len(raw) == 0 || string(raw) == "null" }

// validateOuter checks the fields every kind shares (RFC 5.3).
func validateOuter(version, envelopeID, origin, sentAt string, recipients []string) *Rejection {
	if version != ChatVersion {
		return reject(400, ErrUnsupportedVersion, "a calendar envelope is version "+ChatVersion)
	}
	if !validUUID(envelopeID) {
		return reject(400, ErrSchemaViolation, "envelope_id is not a UUID")
	}
	if origin == "" || origin != strings.ToLower(origin) {
		return reject(400, ErrSchemaViolation, "origin_domain must be a lowercased DNS name")
	}
	if _, err := ParseTimestamp(sentAt); err != nil {
		return reject(400, ErrSchemaViolation, "origin_sent_at is not an RFC 3339 UTC timestamp")
	}
	if len(recipients) == 0 || len(recipients) > maxRecipients {
		return reject(400, ErrSchemaViolation, "recipients must hold between 1 and 100 addresses")
	}
	for _, address := range recipients {
		if !validAddress(address) {
			return reject(400, ErrSchemaViolation, "recipient is not an address: "+address)
		}
	}
	return nil
}

// Validate checks every field rule of RFC 15.2 and 15.6, including each
// document's size and hash.
func (e *MeetingEnvelope) Validate(maxAttachmentBytes int64) *Rejection {
	if r := validateOuter(e.Version, e.EnvelopeID, e.OriginDomain, e.OriginSentAt, e.Recipients); r != nil {
		return r
	}
	if e.Kind != KindMeeting {
		return reject(400, ErrSchemaViolation, "kind must be meeting")
	}
	if !absent(e.Thread) || !absent(e.Chat) || !absent(e.Message) || !absent(e.RSVP) {
		return reject(400, ErrSchemaViolation, "a meeting envelope carries no thread, chat, message or rsvp")
	}
	m := &e.Meeting
	if !validUUID(m.FederationID) {
		return reject(400, ErrSchemaViolation, "meeting.federation_id is not a UUID")
	}
	if m.Revision < 1 {
		return reject(400, ErrSchemaViolation, "meeting.revision is 1 or more")
	}
	if m.Status != StatusConfirmed && m.Status != StatusCancelled {
		return reject(400, ErrSchemaViolation, "meeting.status is confirmed or cancelled")
	}
	// Only the organizer's Domain sends a meeting (RFC 15.2).
	if !validAddress(m.Organizer.Email) || !strings.EqualFold(DomainOf(m.Organizer.Email), e.OriginDomain) {
		return reject(400, ErrSchemaViolation, "meeting.organizer.email is not an address on "+e.OriginDomain)
	}
	if len(m.Organizer.Name) > maxSenderNameBytes {
		return reject(400, ErrSchemaViolation, "meeting.organizer.name exceeds 255 bytes")
	}
	if len(m.Attendees) == 0 || len(m.Attendees) > maxMembers {
		return reject(400, ErrSchemaViolation, "meeting.attendees must hold between 1 and 1000 addresses")
	}
	for _, address := range m.Attendees {
		if !validAddress(address) {
			return reject(400, ErrSchemaViolation, "attendee is not an address: "+address)
		}
	}
	if !containsFold(m.Attendees, m.Organizer.Email) {
		return reject(400, ErrSchemaViolation, "meeting.attendees leaves out the organizer")
	}
	if len(m.Removed) > maxMembers {
		return reject(400, ErrSchemaViolation, "meeting.removed holds more than 1000 addresses")
	}
	for _, address := range m.Removed {
		if !validAddress(address) {
			return reject(400, ErrSchemaViolation, "meeting.removed entry is not an address: "+address)
		}
		if containsFold(m.Attendees, address) {
			return reject(400, ErrSchemaViolation, address+" is removed and an attendee")
		}
	}
	if r := validateContent(m.Title, m.Description, m.DescriptionHTML, "meeting."); r != nil {
		return r
	}
	start, err := ParseTimestamp(m.Start)
	if err != nil {
		return reject(400, ErrSchemaViolation, "meeting.start is not an RFC 3339 UTC timestamp")
	}
	end, err := ParseTimestamp(m.End)
	if err != nil {
		return reject(400, ErrSchemaViolation, "meeting.end is not an RFC 3339 UTC timestamp")
	}
	if !end.After(start) {
		return reject(400, ErrSchemaViolation, "meeting.end is not later than meeting.start")
	}
	loc, r := LoadTimeZone(m.TimeZone)
	if r != nil {
		return r
	}
	if m.AllDay {
		if !Midnight(start, loc) || !Midnight(end, loc) {
			return reject(400, ErrSchemaViolation, "an all-day meeting starts and ends at midnight in "+m.TimeZone)
		}
		if !end.In(loc).After(start.In(loc).AddDate(0, 0, 1).Add(-time.Second)) {
			return reject(400, ErrSchemaViolation, "an all-day meeting lasts at least a day")
		}
	}
	if m.Repeat == nil && len(m.Overrides) > 0 {
		return reject(400, ErrUnsupportedRepeat, "overrides are only allowed with repeat")
	}
	if m.Repeat != nil {
		if err := m.Repeat.Validate(); err != nil {
			return reject(400, ErrUnsupportedRepeat, err.Error())
		}
	}
	if err := rrule.ValidateOverrides(m.Overrides, maxTitleBytes, maxBodyBytes, maxBodyHTMLBytes); err != nil {
		return reject(400, ErrUnsupportedRepeat, err.Error())
	}

	if e.Attachments == nil {
		return reject(400, ErrSchemaViolation, "a meeting envelope carries attachments, empty or not")
	}
	if len(*e.Attachments) > maxAttachments {
		return reject(400, ErrSchemaViolation, "more than 64 attachments")
	}
	for i := range *e.Attachments {
		if r := (*e.Attachments)[i].validate(maxAttachmentBytes); r != nil {
			return r
		}
	}
	for _, address := range e.Recipients {
		if !containsFold(m.Attendees, address) && !containsFold(m.Removed, address) {
			return reject(400, ErrSchemaViolation, "recipient "+address+" is neither an attendee nor removed")
		}
	}
	return nil
}

func validateContent(title, description, html, prefix string) *Rejection {
	if len(title) > maxTitleBytes {
		return reject(400, ErrSchemaViolation, prefix+"title exceeds 255 bytes")
	}
	if len(description) > maxBodyBytes {
		return reject(400, ErrSchemaViolation, prefix+"description exceeds 1 MiB")
	}
	if len(html) > maxBodyHTMLBytes {
		return reject(400, ErrSchemaViolation, prefix+"description_html exceeds 4 MiB")
	}
	return nil
}

// LoadTimeZone reads an IANA zone identifier. The empty name and "Local",
// which Go would take as UTC and as this machine's zone, name no zone.
func LoadTimeZone(name string) (*time.Location, *Rejection) {
	if name == "" || name == "Local" || len(name) > maxTimeZoneBytes {
		return nil, reject(400, ErrUnknownTimeZone, "meeting.time_zone is not an IANA time zone")
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, reject(400, ErrUnknownTimeZone, "unknown time zone "+name)
	}
	return loc, nil
}

// Midnight reports whether an instant is the start of a day in loc.
func Midnight(t time.Time, loc *time.Location) bool {
	local := t.In(loc)
	return local.Hour() == 0 && local.Minute() == 0 && local.Second() == 0 && local.Nanosecond() == 0
}

// Validate checks the field rules of RFC 15.5.
func (e *RSVPEnvelope) Validate() *Rejection {
	if r := validateOuter(e.Version, e.EnvelopeID, e.OriginDomain, e.OriginSentAt, e.Recipients); r != nil {
		return r
	}
	if e.Kind != KindMeetingRSVP {
		return reject(400, ErrSchemaViolation, "kind must be meeting_rsvp")
	}
	if !absent(e.Thread) || !absent(e.Chat) || !absent(e.Message) || !absent(e.Attachments) {
		return reject(400, ErrSchemaViolation, "an answer carries no thread, chat, message or attachments")
	}
	if !validUUID(e.Meeting.FederationID) {
		return reject(400, ErrSchemaViolation, "meeting.federation_id is not a UUID")
	}
	if !validAddress(e.Meeting.Organizer.Email) {
		return reject(400, ErrSchemaViolation, "meeting.organizer.email is not an address")
	}
	a := e.RSVP
	if a == nil {
		return reject(400, ErrSchemaViolation, "an answer carries rsvp")
	}
	// A Domain speaks only for its own users, and never for the organizer,
	// whose answer is always yes (RFC 15.5).
	if !validAddress(a.AttendeeEmail) || !strings.EqualFold(DomainOf(a.AttendeeEmail), e.OriginDomain) {
		return reject(400, ErrSchemaViolation, "rsvp.attendee_email is not an address on "+e.OriginDomain)
	}
	if strings.EqualFold(a.AttendeeEmail, e.Meeting.Organizer.Email) {
		return reject(400, ErrSchemaViolation, "the organizer does not answer their own meeting")
	}
	switch a.Answer {
	case "yes", "no", "maybe":
	default:
		return reject(400, ErrSchemaViolation, "rsvp.answer is yes, no or maybe")
	}
	if a.Revision < 1 {
		return reject(400, ErrSchemaViolation, "rsvp.revision is 1 or more")
	}
	if _, err := ParseTimestamp(a.AnsweredAt); err != nil {
		return reject(400, ErrSchemaViolation, "rsvp.answered_at is not an RFC 3339 UTC timestamp")
	}
	if a.Occurrence != "" {
		if _, err := ParseTimestamp(a.Occurrence); err != nil {
			return reject(400, ErrSchemaViolation, "rsvp.occurrence is not an RFC 3339 UTC timestamp")
		}
	}
	return nil
}

// CalendarDeliverer writes accepted meetings and answers into the calendar
// domain, and tells it how its own meetings fared. Declared here and
// satisfied by *calendar.Service, so this package never imports that one.
type CalendarDeliverer interface {
	// DeliverMeeting applies a revision for its local recipients (RFC 15.3).
	// It returns how many it delivered to and the ones with no account here.
	// An error matching ErrWrongOrganizer is answered organizer_mismatch.
	DeliverMeeting(ctx context.Context, in MeetingInbound) (int, []string, error)
	// UndeliveredMeeting lists the recipients of an accepted envelope that
	// have no account here, to answer a replay the same way (RFC 5.4).
	UndeliveredMeeting(ctx context.Context, in MeetingInbound) ([]string, error)
	// ReceiveRSVP records an answer (RFC 15.5). An error matching
	// ErrNotMember is answered 425, one matching ErrWrongOrganizer 400.
	ReceiveRSVP(ctx context.Context, in RSVPInbound) error
	// MeetingUndelivered tells the organizer that a revision did not reach
	// some attendees, and drops the ones that cannot be reached (RFC 15.1,
	// 15.4). It is local and never leaves this Server.
	MeetingUndelivered(ctx context.Context, b MeetingBounce) error
}

// MeetingInbound is an accepted revision, decoded and checked.
type MeetingInbound struct {
	OriginDomain string
	Meeting      Meeting
	Attachments  []InboundAttachment
	Recipients   []string
}

// RSVPInbound is an accepted answer.
type RSVPInbound struct {
	OriginDomain   string
	MeetingFedID   string
	OrganizerEmail string
	AttendeeEmail  string
	Answer         string
	Revision       int
	AnsweredAt     time.Time
	// Occurrence is the occurrence answered, in the form FormatTimestamp
	// gives it, or empty for the whole meeting.
	Occurrence string
	Recipients []string
}

// MeetingBounce is a revision that did not reach some of its recipients.
type MeetingBounce struct {
	GMeetingID   int64
	MeetingFedID string
	Title        string
	Recipients   []string
	PeerDomain   string
	HTTPStatus   int
	ErrorCode    string
	// Refused means the recipients do not exist at the peer (RFC 15.4).
	// Unreachable means their Domain does not do calendar (RFC 15.1). Both
	// are dropped from the meeting. Backfill means the revision timed out
	// and is sent again when the peer answers (RFC 6.4).
	Refused     bool
	Unreachable bool
	Backfill    bool
}

// MeetingOutbound is one revision for one peer.
type MeetingOutbound struct {
	PeerDomain   string
	OriginDomain string
	Meeting      Meeting
	Attachments  []OutboundAttachment
	Recipients   []string
	// RelatedID is the local g_meeting.
	RelatedID int64
}

// RSVPOutbound is one answer for one peer.
type RSVPOutbound struct {
	PeerDomain     string
	OriginDomain   string
	MeetingFedID   string
	OrganizerEmail string
	AttendeeEmail  string
	Answer         string
	Revision       int
	AnsweredAt     time.Time
	Occurrence     string
	Recipients     []string
	RelatedID      int64
}

// Outbox kinds of the calendar extension.
const (
	outboxMeeting     = "meeting"
	outboxMeetingRSVP = "meeting_rsvp"
)

func isMeetingKind(kind string) bool { return kind == outboxMeeting || kind == outboxMeetingRSVP }

// AttachCalendar supplies the calendar domain. A Server that attaches it
// advertises calendar and version 0.3 (RFC 15.1).
func (s *Service) AttachCalendar(c CalendarDeliverer) { s.calendar = c }

// Calendar reports whether a peer Domain implements the calendar extension,
// asking it again when what is pinned says no, since a peer pinned before it
// took up calendar still looks as it did then (RFC 15.1). It fails when the
// Domain cannot be discovered at all, which is how an address that is not
// on a Webmail Domain looks.
func (s *Service) Calendar(ctx context.Context, domain string) (bool, error) {
	peer, err := s.Peer(ctx, domain, false)
	if err != nil {
		return false, err
	}
	if !advertisesCalendar(peer) {
		if refreshed, err := s.Peer(ctx, domain, true); err == nil {
			peer = refreshed
		}
	}
	return advertisesCalendar(peer), nil
}

func advertisesCalendar(d *Descriptor) bool { return d.Calendar && d.Speaks(ChatVersion) }

// EnqueueMeeting writes a revision to the outbox inside the caller's
// transaction. A queued revision of the same meeting for the same peer and
// recipients that has not gone out yet is dropped, since this one says
// everything it did; that is also what makes a backfill send the latest
// revision (RFC 15.3).
func (s *Service) EnqueueMeeting(ctx context.Context, tx *sql.Tx, out MeetingOutbound) error {
	if !s.Enabled() {
		return fmt.Errorf("federation is not configured")
	}
	envelopeID, err := NewUUID()
	if err != nil {
		return err
	}
	attachments := make([]Attachment, 0, len(out.Attachments))
	for _, a := range out.Attachments {
		attachments = append(attachments, encodeAttachment(a))
	}
	envelope := MeetingEnvelope{
		Version:      ChatVersion,
		Kind:         KindMeeting,
		EnvelopeID:   envelopeID,
		OriginDomain: out.OriginDomain,
		OriginSentAt: FormatTimestamp(s.db.Now()),
		Meeting:      out.Meeting,
		Attachments:  &attachments,
		Recipients:   out.Recipients,
	}
	if err := s.supersede(ctx, tx, outboxMeeting, out.PeerDomain, out.RelatedID, func(body []byte) bool {
		var queued MeetingEnvelope
		return json.Unmarshal(body, &queued) == nil && sameSet(queued.Recipients, out.Recipients)
	}); err != nil {
		return err
	}
	return s.enqueueKind(ctx, tx, outboxMeeting, out.PeerDomain, out.OriginDomain, envelopeID, out.RelatedID, envelope)
}

// EnqueueRSVP writes an answer to the outbox inside the caller's transaction,
// dropping a queued earlier answer by the same attendee for the same
// occurrence and peer (RFC 15.5).
func (s *Service) EnqueueRSVP(ctx context.Context, tx *sql.Tx, out RSVPOutbound) error {
	if !s.Enabled() {
		return fmt.Errorf("federation is not configured")
	}
	envelopeID, err := NewUUID()
	if err != nil {
		return err
	}
	envelope := RSVPEnvelope{
		Version:      ChatVersion,
		Kind:         KindMeetingRSVP,
		EnvelopeID:   envelopeID,
		OriginDomain: out.OriginDomain,
		OriginSentAt: FormatTimestamp(s.db.Now()),
		Meeting:      MeetingRef{FederationID: out.MeetingFedID, Organizer: Organizer{Email: out.OrganizerEmail}},
		RSVP: &RSVP{
			AttendeeEmail: out.AttendeeEmail,
			Answer:        out.Answer,
			Revision:      out.Revision,
			AnsweredAt:    FormatTimestamp(out.AnsweredAt),
			Occurrence:    out.Occurrence,
		},
		Recipients: out.Recipients,
	}
	if err := s.supersede(ctx, tx, outboxMeetingRSVP, out.PeerDomain, out.RelatedID, func(body []byte) bool {
		var queued RSVPEnvelope
		return json.Unmarshal(body, &queued) == nil && queued.RSVP != nil &&
			queued.Meeting.FederationID == out.MeetingFedID &&
			strings.EqualFold(queued.RSVP.AttendeeEmail, out.AttendeeEmail) &&
			queued.RSVP.Occurrence == out.Occurrence
	}); err != nil {
		return err
	}
	return s.enqueueKind(ctx, tx, outboxMeetingRSVP, out.PeerDomain, out.OriginDomain, envelopeID, out.RelatedID, envelope)
}

func (s *Service) enqueueKind(ctx context.Context, tx *sql.Tx, kind, peer, origin, envelopeID string,
	relatedID int64, envelope any) error {
	body, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("serialize %s envelope: %w", kind, err)
	}
	var related any
	if relatedID != 0 {
		related = relatedID
	}
	now := s.db.Now()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO "federation_outbox" ("kind", "peer_domain", "origin_domain", "body", `+
			`"idempotency_key", "related_id", "status", "next_retry_at", "created_at") `+
			"VALUES (?,?,?,?,?,?, 'pending', ?, ?)",
		kind, peer, origin, string(body), envelopeID, related, now, now); err != nil {
		return fmt.Errorf("enqueue %s envelope for %s: %w", kind, peer, err)
	}
	return nil
}

// supersede marks as superseded the rows of one kind, for one peer and local
// meeting, that wait to be sent or are owed, are not leased to a worker, and
// match. A row a worker holds is left to finish: it may be on the wire.
func (s *Service) supersede(ctx context.Context, tx *sql.Tx, kind, peer string, relatedID int64,
	match func(body []byte) bool) error {
	if relatedID == 0 {
		return nil
	}
	now := s.db.Now()
	rows, err := tx.QueryContext(ctx,
		`SELECT "id", "body" FROM "federation_outbox" WHERE "kind" = ? AND "peer_domain" = ? `+
			`AND "related_id" = ? AND "status" IN ('pending', 'owed') `+
			`AND ("claimed_until" IS NULL OR "claimed_until" < ?)`, kind, peer, relatedID, now)
	if err != nil {
		return fmt.Errorf("list queued %s envelopes: %w", kind, err)
	}
	var stale []int64
	for rows.Next() {
		var id int64
		var body string
		if err := rows.Scan(&id, &body); err != nil {
			rows.Close()
			return fmt.Errorf("scan queued %s envelope: %w", kind, err)
		}
		if match([]byte(body)) {
			stale = append(stale, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list queued %s envelopes: %w", kind, err)
	}
	for _, id := range stale {
		if _, err := tx.ExecContext(ctx,
			`UPDATE "federation_outbox" SET "status" = 'superseded' WHERE "id" = ?`, id); err != nil {
			return fmt.Errorf("supersede envelope %d: %w", id, err)
		}
	}
	return nil
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if !containsFold(b, x) {
			return false
		}
	}
	return true
}

// receiveMeeting is Receive for an envelope of kind meeting or meeting_rsvp,
// from the point where the request is authenticated and its nonce claimed.
func (s *Service) receiveMeeting(ctx context.Context, req *Request, kind string) (*Accepted, *Rejection) {
	if s.calendar == nil {
		return nil, reject(400, ErrUnsupportedKind, "this Domain does not do calendar")
	}
	if kind == KindMeetingRSVP {
		return s.receiveRSVP(ctx, req)
	}
	var envelope MeetingEnvelope
	if err := json.Unmarshal(req.Body, &envelope); err != nil {
		var unknown *rrule.ErrUnknownMember
		if errors.As(err, &unknown) {
			return nil, reject(400, ErrUnsupportedRepeat, unknown.Reason)
		}
		return nil, reject(400, ErrMalformedEnvelope, "envelope is not JSON")
	}
	if r := envelope.Validate(s.cfg.MaxAttachmentBytes); r != nil {
		return nil, r
	}
	// The triple check of RFC 9.6, with the organizer in the role of the
	// sender, which Validate holds to the Origin's domain.
	if !strings.EqualFold(envelope.OriginDomain, req.Sender) {
		return nil, reject(401, ErrSenderMismatch, "origin_domain does not match X-Federation-Sender")
	}
	if !strings.EqualFold(DomainOf(envelope.Meeting.Organizer.Email), req.Sender) {
		return nil, reject(401, ErrSenderMismatch, "the organizer is not on the sending Domain")
	}
	if r := recipientsOn(envelope.Recipients, req.Destination); r != nil {
		return nil, r
	}
	in, r := toMeetingInbound(&envelope)
	if r != nil {
		return nil, r
	}

	seen, err := s.alreadyAccepted(ctx, req.Sender, envelope.EnvelopeID)
	if err != nil {
		return nil, reject(500, ErrServerError, err.Error())
	}
	if seen {
		refused, err := s.calendar.UndeliveredMeeting(ctx, *in)
		if err != nil {
			return nil, reject(500, ErrServerError, err.Error())
		}
		return accepted(envelope.EnvelopeID, refused), nil
	}
	if r := s.rateLimited(ctx, req.Sender); r != nil {
		return nil, r
	}
	delivered, refused, err := s.calendar.DeliverMeeting(ctx, *in)
	if errors.Is(err, ErrWrongOrganizer) {
		return nil, reject(400, ErrOrganizerMismatch, err.Error())
	}
	if err != nil {
		s.log.Error("inbound meeting failed", "sender", req.Sender, "envelope", envelope.EnvelopeID, "error", err)
		return nil, reject(500, ErrServerError, "could not store the meeting")
	}
	if delivered == 0 && len(refused) > 0 {
		return nil, reject(404, ErrNoRecipients, "no recipient is known here")
	}
	s.recordChatAccepted(ctx, req.Sender, envelope.EnvelopeID)
	s.log.Info("meeting accepted", "sender", req.Sender, "envelope", envelope.EnvelopeID,
		"meeting", envelope.Meeting.FederationID, "revision", envelope.Meeting.Revision, "refused", len(refused))
	return accepted(envelope.EnvelopeID, refused), nil
}

func (s *Service) receiveRSVP(ctx context.Context, req *Request) (*Accepted, *Rejection) {
	var envelope RSVPEnvelope
	if err := json.Unmarshal(req.Body, &envelope); err != nil {
		return nil, reject(400, ErrMalformedEnvelope, "envelope is not JSON")
	}
	if r := envelope.Validate(); r != nil {
		return nil, r
	}
	if !strings.EqualFold(envelope.OriginDomain, req.Sender) {
		return nil, reject(401, ErrSenderMismatch, "origin_domain does not match X-Federation-Sender")
	}
	if !strings.EqualFold(DomainOf(envelope.RSVP.AttendeeEmail), req.Sender) {
		return nil, reject(401, ErrSenderMismatch, "the attendee is not on the sending Domain")
	}
	if r := recipientsOn(envelope.Recipients, req.Destination); r != nil {
		return nil, r
	}
	seen, err := s.alreadyAccepted(ctx, req.Sender, envelope.EnvelopeID)
	if err != nil {
		return nil, reject(500, ErrServerError, err.Error())
	}
	if seen {
		return accepted(envelope.EnvelopeID, nil), nil
	}
	if r := s.rateLimited(ctx, req.Sender); r != nil {
		return nil, r
	}
	answeredAt, _ := ParseTimestamp(envelope.RSVP.AnsweredAt)
	occurrence := ""
	if envelope.RSVP.Occurrence != "" {
		at, _ := ParseTimestamp(envelope.RSVP.Occurrence)
		occurrence = FormatTimestamp(at)
	}
	err = s.calendar.ReceiveRSVP(ctx, RSVPInbound{
		OriginDomain:   envelope.OriginDomain,
		MeetingFedID:   envelope.Meeting.FederationID,
		OrganizerEmail: strings.ToLower(envelope.Meeting.Organizer.Email),
		AttendeeEmail:  strings.ToLower(envelope.RSVP.AttendeeEmail),
		Answer:         envelope.RSVP.Answer,
		Revision:       envelope.RSVP.Revision,
		AnsweredAt:     answeredAt,
		Occurrence:     occurrence,
		Recipients:     lowercase(envelope.Recipients),
	})
	switch {
	case errors.Is(err, ErrNotMember):
		// Not recorded as accepted: the revision that invites this Domain's
		// users may still be on its way (RFC 15.5).
		return nil, reject(http.StatusTooEarly, ErrNotAMember, err.Error())
	case errors.Is(err, ErrWrongOrganizer):
		return nil, reject(400, ErrOrganizerMismatch, err.Error())
	case err != nil:
		s.log.Error("inbound answer failed", "sender", req.Sender, "envelope", envelope.EnvelopeID, "error", err)
		return nil, reject(500, ErrServerError, "could not record the answer")
	}
	s.recordChatAccepted(ctx, req.Sender, envelope.EnvelopeID)
	return accepted(envelope.EnvelopeID, nil), nil
}

// recipientsOn holds every recipient to the Destination's own domain (RFC 5.3).
func recipientsOn(recipients []string, domain string) *Rejection {
	for _, address := range recipients {
		if !strings.EqualFold(DomainOf(address), domain) {
			return reject(400, ErrNotRegistrar, "recipient "+address+" is not on "+domain)
		}
	}
	return nil
}

// toMeetingInbound lowercases the addresses, puts every instant in the one
// form FormatTimestamp gives, so that occurrences compare as strings on every
// Domain, and decodes the documents.
func toMeetingInbound(e *MeetingEnvelope) (*MeetingInbound, *Rejection) {
	m := e.Meeting
	m.Organizer.Email = strings.ToLower(m.Organizer.Email)
	m.Attendees = withoutMailerDaemon(lowercase(m.Attendees))
	m.Removed = lowercase(m.Removed)
	m.Start, m.End = normal(m.Start), normal(m.End)
	if m.Repeat != nil {
		r := *m.Repeat
		if r.Until != "" {
			r.Until = normal(r.Until)
		}
		r.Extra = slices.Clone(r.Extra)
		for i := range r.Extra {
			r.Extra[i] = normal(r.Extra[i])
		}
		m.Repeat = &r
	}
	m.Overrides = slices.Clone(m.Overrides)
	for i := range m.Overrides {
		o := &m.Overrides[i]
		o.Occurrence = normal(o.Occurrence)
		if o.Start != "" {
			o.Start = normal(o.Start)
		}
		if o.End != "" {
			o.End = normal(o.End)
		}
	}
	in := &MeetingInbound{OriginDomain: e.OriginDomain, Meeting: m, Recipients: lowercase(e.Recipients)}
	for _, a := range *e.Attachments {
		data, r := a.Bytes()
		if r != nil {
			return nil, r
		}
		in.Attachments = append(in.Attachments, InboundAttachment{
			FederationID: a.FederationID, FileName: a.FileName, MimeType: a.MimeType, Data: data,
		})
	}
	return in, nil
}

// normal rewrites a validated timestamp in the form FormatTimestamp gives.
func normal(v string) string {
	t, err := ParseTimestamp(v)
	if err != nil {
		return v
	}
	return FormatTimestamp(t)
}

// afterMeetingAttempt tells the calendar domain what became of a revision it
// sent: who was refused, whose Domain does not do calendar, and who has not
// been reached yet. An answer that fails is only logged; a later answer, or
// the backfill, says everything it did.
func (s *Service) afterMeetingAttempt(ctx context.Context, row outboxRow, httpStatus int, code string,
	refused []Refusal, failed, backfill bool) {
	if s.calendar == nil {
		return
	}
	if row.Kind != outboxMeeting {
		if failed || backfill {
			s.log.Warn("an answer did not arrive", "peer", row.PeerDomain, "envelope", row.EnvelopeID,
				"status", httpStatus, "error", code)
		}
		return
	}
	var envelope MeetingEnvelope
	if err := json.Unmarshal(row.Body, &envelope); err != nil {
		s.log.Error("could not read a meeting envelope after sending it", "id", row.ID, "error", err)
		return
	}
	b := MeetingBounce{
		GMeetingID:   row.RelatedID,
		MeetingFedID: envelope.Meeting.FederationID,
		Title:        envelope.Meeting.Title,
		PeerDomain:   row.PeerDomain,
		HTTPStatus:   httpStatus,
		ErrorCode:    code,
	}
	switch {
	case !failed && !backfill:
		for _, r := range refused {
			// Only an attendee can be refused into leaving the meeting; a
			// removed address was being told it is out already.
			if r.Error == ErrUnknownRecipient && containsFold(envelope.Recipients, r.Address) &&
				containsFold(envelope.Meeting.Attendees, r.Address) {
				b.Recipients = append(b.Recipients, r.Address)
			}
		}
		if len(b.Recipients) == 0 {
			return
		}
		b.Refused = true
	case backfill:
		b.Recipients, b.Backfill = envelope.Recipients, true
	default:
		b.Recipients = envelope.Recipients
		switch {
		case httpStatus == http.StatusNotFound:
			b.Refused = true
		case code == ErrUnsupportedKind || code == ErrUnsupportedVersion:
			b.Unreachable = true
		}
		// Addresses this revision removed are not dropped again.
		if b.Refused || b.Unreachable {
			b.Recipients = slices.DeleteFunc(slices.Clone(b.Recipients), func(a string) bool {
				return !containsFold(envelope.Meeting.Attendees, a)
			})
			if len(b.Recipients) == 0 {
				return
			}
		}
	}
	if err := s.calendar.MeetingUndelivered(ctx, b); err != nil {
		s.log.Error("could not tell the organizer about a meeting", "envelope", row.EnvelopeID, "error", err)
	}
}
