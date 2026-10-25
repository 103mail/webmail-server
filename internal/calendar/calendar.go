// Package calendar implements the calendaring domain: calendars, meetings,
// attendees, answers and documents.
//
// A meeting is stored the way a thread is (ADR 22). g_meeting says who
// organizes it; every change the organizer makes is a new row in
// g_meeting_revision with its attendees and documents, and every answer a new
// row in g_meeting_rsvp. Nothing canonical is updated or deleted. A customer
// sees the latest revision and the latest answers, through one `meeting`
// projection per meeting, and a series is expanded into occurrences when a
// calendar is read, by internal/rrule.
//
// Meetings cross Domains as RFC 15 describes: the organizer's Domain sends
// every revision to the Domains of the attendees, and each attendee's Domain
// sends their answers to all the others (federate.go, receive.go). A calendar
// is a mailbox, as ADR 14 decided: calendarId is the mailbox id.
package calendar

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/103mail/webmail-server/internal/blob"
	"github.com/103mail/webmail-server/internal/federation"
	"github.com/103mail/webmail-server/internal/rrule"
	"github.com/103mail/webmail-server/internal/store"
)

// Service implements the calendar domain.
type Service struct {
	db   *store.DB
	blob *blob.Store
	// fed is nil on a Server without federation, where every address is
	// local and no meeting leaves it.
	fed Federator
	// notices writes the organizer's mailer-daemon messages; nil drops them.
	notices Noticer
}

// New returns a Service. documents is the directory holding document bytes.
func New(db *store.DB, documents *blob.Store) *Service {
	return &Service{db: db, blob: documents}
}

// Noticer writes a local message from mailer-daemon into someone's inbox. It
// is satisfied by *email.Service and never sends anything off this Server
// (RFC 15.1, 15.4).
type Noticer interface {
	Notice(ctx context.Context, originDomain, recipient, subject, body string) error
}

// NotifyBy supplies the mail domain that tells organizers who their
// invitations did not reach.
func (s *Service) NotifyBy(n Noticer) { s.notices = n }

// ErrNotOrganizer refuses a change to a meeting by anyone but its organizer,
// who is the only one who can change it, invite or remove (RFC 15.4, ADR 22).
var ErrNotOrganizer = errors.New("only the organizer can change a meeting")

// ErrBadInput is a change that cannot be made as asked, such as an end before
// the start or an occurrence the meeting does not have.
var ErrBadInput = errors.New("invalid meeting")

func badInput(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrBadInput, fmt.Sprintf(format, args...))
}

// Calendar is a mailbox viewed as a calendar.
type Calendar struct {
	CalendarID int64  `json:"calendarId"`
	MailboxID  int64  `json:"mailboxId"`
	Name       string `json:"name"`
	Color      string `json:"color"`
	IsShared   bool   `json:"isShared"`
}

// Meeting is one occurrence of a meeting as one customer sees it, including
// the layout hints the day view uses to place overlapping events side by side.
type Meeting struct {
	MeetingID int64 `json:"meetingId"`
	// Occurrence is what this occurrence is known by, the start it has by
	// its rule, which an answer for it names (RFC 15.5).
	Occurrence     string  `json:"occurrence"`
	CalendarID     int64   `json:"calendarId"`
	Title          string  `json:"title"`
	StartTime      string  `json:"startTime"`
	EndTime        string  `json:"endTime"`
	TimeZone       string  `json:"timeZone"`
	IsAllDay       bool    `json:"isAllDay"`
	OrganizerEmail string  `json:"organizerEmail"`
	OrganizerName  string  `json:"organizerName"`
	Description    string  `json:"description"`
	WillAttend     *string `json:"willAttend"`
	// Status is cancelled when the meeting or this occurrence is, and for
	// someone the organizer removed (RFC 15.4).
	Status      string `json:"status"`
	Revision    int    `json:"revision"`
	Unseen      bool   `json:"unseen"`
	IsOrganizer bool   `json:"isOrganizer"`
	// Repeat is the rule of a series, and RepeatText the rule in words.
	Repeat             *rrule.Repeat `json:"repeat"`
	RepeatText         string        `json:"repeatText"`
	Attendees          []Attendee    `json:"attendees"`
	Documents          []DocumentRef `json:"documents"`
	WidthFraction      int           `json:"widthFraction"`
	HorizontalPosition int           `json:"horizontalPosition"`
}

// Attendee is one invited address and how it answered this occurrence.
type Attendee struct {
	Email      string  `json:"email"`
	WillAttend *string `json:"willAttend"`
	// Revision is the revision the answer was given to. Outdated marks an
	// answer given to an earlier version of the meeting (RFC 15.5).
	Revision   int     `json:"revision"`
	AnsweredAt *string `json:"answeredAt"`
	Outdated   bool    `json:"outdated"`
}

// DocumentRef is a document of the meeting, without its bytes.
type DocumentRef struct {
	DocumentID int64  `json:"documentId"`
	FileName   string `json:"fileName"`
	MimeType   string `json:"mimeType"`
	FileSize   int64  `json:"fileSize"`
}

// Document is a meeting attachment with its bytes.
type Document struct {
	FileName string `json:"fileName"`
	MimeType string `json:"mimeType"`
	FileSize int64  `json:"fileSize"`
	FileData []byte `json:"fileData"`
}

// rfc3339 formats a timestamp the way PHP's date('c') does, with an explicit
// +00:00 offset rather than Go's "Z", so responses stay byte-identical to the
// PHP backend while both are in service.
func rfc3339(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05-07:00") }

// ListCalendars returns the customer's calendars.
func (s *Service) ListCalendars(ctx context.Context, customerID int64) ([]Calendar, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT "id", "name", "color", "user_id" FROM "mailbox" `+
			`WHERE "customer_id" = ? AND "deleted_at" IS NULL ORDER BY "id"`, customerID)
	if err != nil {
		return nil, fmt.Errorf("list calendars: %w", err)
	}
	defer rows.Close()

	calendars := []Calendar{}
	for rows.Next() {
		var c Calendar
		var name sql.NullString
		var userID sql.NullInt64
		if err := rows.Scan(&c.CalendarID, &name, &c.Color, &userID); err != nil {
			return nil, fmt.Errorf("scan calendar: %w", err)
		}
		c.MailboxID = c.CalendarID
		c.Name = name.String
		if c.Name == "" {
			c.Name = "Calendar"
		}
		c.IsShared = !userID.Valid
		calendars = append(calendars, c)
	}
	return calendars, rows.Err()
}

// meetingRow is a g_meeting: the meeting's identity and organizer.
type meetingRow struct {
	id            int64
	fedID         string
	organizer     string
	organizerName string
}

// revision is one g_meeting_revision: the whole meeting at one point.
type revision struct {
	id          int64
	number      int
	status      string
	title       string
	description string
	html        string
	start, end  time.Time
	timeZone    string
	allDay      bool
	repeat      *rrule.Repeat
	overrides   []rrule.Override
}

// series is the revision's timing, ready to expand.
func (r *revision) series() (rrule.Series, error) {
	loc, err := time.LoadLocation(r.timeZone)
	if err != nil {
		return rrule.Series{}, fmt.Errorf("time zone of meeting revision %d: %w", r.id, err)
	}
	return rrule.Series{Start: r.start, End: r.end, Location: loc, AllDay: r.allDay,
		Repeat: r.repeat, Overrides: r.overrides}, nil
}

// document is a g_meeting_document.
type document struct {
	id       int64
	fedID    string
	name     string
	mime     string
	size     int64
	md5      string
	sha256   string
	revision int
}

func loadMeeting(ctx context.Context, q store.Queryer, gMeetingID int64) (*meetingRow, error) {
	m := meetingRow{id: gMeetingID}
	err := q.QueryRowContext(ctx,
		`SELECT "federation_id", "organizer_email", "organizer_name" FROM "g_meeting" WHERE "id" = ? LIMIT 1`,
		gMeetingID).Scan(&m.fedID, &m.organizer, &m.organizerName)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look up meeting: %w", err)
	}
	return &m, nil
}

const revisionColumns = `"id", "revision", "status", "title", "description", "description_html", ` +
	`"start_time", "end_time", "time_zone", "is_all_day", "repeat", "overrides"`

func scanRevision(row interface{ Scan(...any) error }) (*revision, error) {
	var r revision
	var html, repeat, overrides sql.NullString
	if err := row.Scan(&r.id, &r.number, &r.status, &r.title, &r.description, &html,
		&r.start, &r.end, &r.timeZone, &r.allDay, &repeat, &overrides); err != nil {
		return nil, err
	}
	r.html = html.String
	r.start, r.end = r.start.UTC(), r.end.UTC()
	if repeat.Valid && repeat.String != "" {
		r.repeat = &rrule.Repeat{}
		if err := json.Unmarshal([]byte(repeat.String), r.repeat); err != nil {
			return nil, fmt.Errorf("read repeat of meeting revision %d: %w", r.id, err)
		}
	}
	if overrides.Valid && overrides.String != "" {
		if err := json.Unmarshal([]byte(overrides.String), &r.overrides); err != nil {
			return nil, fmt.Errorf("read overrides of meeting revision %d: %w", r.id, err)
		}
	}
	return &r, nil
}

// latestRevision is the highest revision held of a meeting, nil for none.
func latestRevision(ctx context.Context, q store.Queryer, gMeetingID int64) (*revision, error) {
	r, err := scanRevision(q.QueryRowContext(ctx,
		`SELECT `+revisionColumns+` FROM "g_meeting_revision" WHERE "g_meeting_id" = ? `+
			`ORDER BY "revision" DESC LIMIT 1`, gMeetingID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look up meeting revision: %w", err)
	}
	return r, nil
}

func attendeesOf(ctx context.Context, q store.Queryer, revisionID int64) ([]string, error) {
	attendees, err := store.SelectStrings(ctx, q,
		`SELECT "email" FROM "g_meeting_attendee" WHERE "g_meeting_revision_id" = ? ORDER BY "id"`, revisionID)
	if err != nil {
		return nil, fmt.Errorf("list attendees: %w", err)
	}
	return attendees, nil
}

func documentsOf(ctx context.Context, q store.Queryer, revisionID int64) ([]document, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT "g_meeting_document"."id", "g_meeting_document"."federation_id", "g_meeting_document"."file_name", `+
			`"g_meeting_document"."mime_type", "g_meeting_document"."file_size", "g_meeting_document"."hash_md5", `+
			`"g_meeting_document"."hash_sha256", "g_meeting_revision"."revision" FROM "g_meeting_document" `+
			`JOIN "g_meeting_revision" ON "g_meeting_revision"."id" = "g_meeting_document"."g_meeting_revision_id" `+
			`WHERE "g_meeting_document"."g_meeting_revision_id" = ? ORDER BY "g_meeting_document"."id"`, revisionID)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	defer rows.Close()
	var out []document
	for rows.Next() {
		var d document
		if err := rows.Scan(&d.id, &d.fedID, &d.name, &d.mime, &d.size, &d.md5, &d.sha256, &d.revision); err != nil {
			return nil, fmt.Errorf("scan document: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// aliasesOf are the addresses a mailbox receives at, which is who its owner
// is in a meeting.
func aliasesOf(ctx context.Context, q store.Queryer, mailboxID int64) ([]string, error) {
	aliases, err := store.SelectStrings(ctx, q,
		`SELECT "email" FROM "alias" WHERE "mailbox_id" = ? ORDER BY "id"`, mailboxID)
	if err != nil {
		return nil, fmt.Errorf("list aliases: %w", err)
	}
	return aliases, nil
}

// view is a meeting as one mailbox is shown it. Someone the organizer removed
// is shown the revision that removed them, as cancelled, and nothing later,
// since their Domain receives no later revision (RFC 15.4).
type view struct {
	meeting   *meetingRow
	revision  *revision
	attendees []string
	// self is the viewer's address in the meeting, removed whether the
	// organizer took them out.
	self    string
	removed bool
}

func (s *Service) viewOf(ctx context.Context, q store.Queryer, gMeetingID int64, aliases []string) (*view, error) {
	m, err := loadMeeting(ctx, q, gMeetingID)
	if err != nil || m == nil {
		return nil, err
	}
	latest, err := latestRevision(ctx, q, gMeetingID)
	if err != nil || latest == nil {
		return nil, err
	}
	attendees, err := attendeesOf(ctx, q, latest.id)
	if err != nil {
		return nil, err
	}
	v := &view{meeting: m, revision: latest, attendees: attendees}
	for _, a := range aliases {
		if contains(attendees, a) {
			v.self = a
			return v, nil
		}
	}
	if len(aliases) == 0 {
		return v, nil
	}
	// The last revision that still listed them, and the first after it,
	// which is the one that removed them, or, when that one never arrived,
	// the first that told this Domain they were out (RFC 15.4).
	args := append([]any{gMeetingID}, stringArgs(aliases)...)
	last, err := store.SelectInt(ctx, q,
		`SELECT COALESCE(MAX("g_meeting_revision"."revision"), 0) FROM "g_meeting_revision" `+
			`JOIN "g_meeting_attendee" ON "g_meeting_attendee"."g_meeting_revision_id" = "g_meeting_revision"."id" `+
			`WHERE "g_meeting_revision"."g_meeting_id" = ? AND "g_meeting_attendee"."email" IN (`+
			store.Placeholders(len(aliases))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("look up removal: %w", err)
	}
	v.removed = true
	if last == 0 {
		return v, nil
	}
	shown, err := scanRevision(q.QueryRowContext(ctx,
		`SELECT `+revisionColumns+` FROM "g_meeting_revision" WHERE "g_meeting_id" = ? AND "revision" > ? `+
			`ORDER BY "revision" LIMIT 1`, gMeetingID, last))
	if err != nil {
		return nil, fmt.Errorf("look up the revision that removed them: %w", err)
	}
	v.revision = shown
	if v.attendees, err = attendeesOf(ctx, q, shown.id); err != nil {
		return nil, err
	}
	v.self = aliases[0]
	return v, nil
}

// ListMeetings returns the occurrences in the given calendars that start
// between from and until, grouped by calendar day. An occurrence spanning
// several days appears under each day it covers.
func (s *Service) ListMeetings(ctx context.Context, customerID int64, calendarIDs []int64,
	from, until time.Time) (map[string][]Meeting, error) {
	days := map[string][]Meeting{}
	for d := from.UTC().Truncate(24 * time.Hour); d.Before(until); d = d.AddDate(0, 0, 1) {
		days[d.Format("2006-01-02")] = []Meeting{}
	}
	if len(calendarIDs) == 0 {
		return days, nil
	}

	// Only meetings with a revision that can fall in the range are read and
	// expanded (ADR 22).
	args := append([]any{customerID, until, from}, store.Args(calendarIDs)...)
	rows, err := s.db.QueryContext(ctx,
		`SELECT "meeting"."id", "meeting"."mailbox_id", "meeting"."g_meeting_id", "meeting"."seen_revision" `+
			`FROM "meeting" WHERE "meeting"."customer_id" = ? AND "meeting"."deleted_at" IS NULL `+
			`AND EXISTS (SELECT 1 FROM "g_meeting_revision" WHERE "g_meeting_revision"."g_meeting_id" = "meeting"."g_meeting_id" `+
			`AND "g_meeting_revision"."first_start" < ? AND ("g_meeting_revision"."last_end" IS NULL `+
			`OR "g_meeting_revision"."last_end" > ?)) `+
			`AND "meeting"."mailbox_id" IN (`+store.Placeholders(len(calendarIDs))+`) ORDER BY "meeting"."id"`, args...)
	if err != nil {
		return nil, fmt.Errorf("list meetings: %w", err)
	}
	type projection struct {
		id, mailboxID, gMeetingID int64
		seen                      int
	}
	var projections []projection
	for rows.Next() {
		var p projection
		if err := rows.Scan(&p.id, &p.mailboxID, &p.gMeetingID, &p.seen); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan meeting: %w", err)
		}
		projections = append(projections, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list meetings: %w", err)
	}

	type span struct {
		meeting    Meeting
		start, end time.Time
	}
	var spans []span
	aliases := map[int64][]string{}
	for _, p := range projections {
		if _, ok := aliases[p.mailboxID]; !ok {
			if aliases[p.mailboxID], err = aliasesOf(ctx, s.db, p.mailboxID); err != nil {
				return nil, err
			}
		}
		v, err := s.viewOf(ctx, s.db, p.gMeetingID, aliases[p.mailboxID])
		if err != nil {
			return nil, err
		}
		if v == nil {
			continue
		}
		occurrences, err := s.occurrencesOf(ctx, v, p.id, p.mailboxID, p.seen, from, until)
		if err != nil {
			return nil, err
		}
		for _, o := range occurrences {
			spans = append(spans, span{meeting: o.meeting, start: o.start, end: o.end})
		}
	}
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].start.Before(spans[j].start) })

	for day := range days {
		dayStart, err := time.ParseInLocation("2006-01-02", day, time.UTC)
		if err != nil {
			continue
		}
		dayEnd := dayStart.AddDate(0, 0, 1)
		var todays []Meeting
		var ranges [][2]int64
		for _, sp := range spans {
			if sp.start.Before(dayEnd) && sp.end.After(dayStart) {
				todays = append(todays, sp.meeting)
				ranges = append(ranges, [2]int64{sp.start.Unix(), sp.end.Unix()})
			}
		}
		widths := widthFractions(ranges)
		positions := horizontalPositions(ranges)
		for i := range todays {
			todays[i].WidthFraction = widths[i]
			todays[i].HorizontalPosition = positions[i]
		}
		if todays == nil {
			todays = []Meeting{}
		}
		days[day] = todays
	}
	return days, nil
}

type listed struct {
	meeting    Meeting
	start, end time.Time
}

// occurrencesOf expands one meeting as one viewer sees it, keeping the
// occurrences that start in [from, until).
func (s *Service) occurrencesOf(ctx context.Context, v *view, projectionID, mailboxID int64, seen int,
	from, until time.Time) ([]listed, error) {
	r := v.revision
	series, err := r.series()
	if err != nil {
		return nil, err
	}
	answers, err := answersOf(ctx, s.db, v.meeting.id)
	if err != nil {
		return nil, err
	}
	docs, err := documentsOf(ctx, s.db, r.id)
	if err != nil {
		return nil, err
	}
	refs := make([]DocumentRef, 0, len(docs))
	for _, d := range docs {
		refs = append(refs, DocumentRef{DocumentID: d.id, FileName: d.name, MimeType: d.mime, FileSize: d.size})
	}
	isOrganizer := strings.EqualFold(v.self, v.meeting.organizer) && !v.removed

	var out []listed
	for _, o := range series.Expand(until) {
		if o.Start.Before(from) {
			continue
		}
		m := Meeting{
			MeetingID:      projectionID,
			Occurrence:     o.Key,
			CalendarID:     mailboxID,
			Title:          r.title,
			StartTime:      rfc3339(o.Start),
			EndTime:        rfc3339(o.End),
			TimeZone:       r.timeZone,
			IsAllDay:       r.allDay,
			OrganizerEmail: v.meeting.organizer,
			OrganizerName:  v.meeting.organizerName,
			Description:    r.description,
			Status:         r.status,
			Revision:       r.number,
			Unseen:         seen < r.number,
			IsOrganizer:    isOrganizer,
			Repeat:         r.repeat,
			RepeatText:     r.repeat.HumanReadable(),
			Documents:      refs,
		}
		if ov := o.Override; ov != nil {
			if ov.Status != "" && m.Status != federation.StatusCancelled {
				m.Status = ov.Status
			}
			if ov.Title != nil {
				m.Title = *ov.Title
			}
			if ov.Description != nil {
				m.Description = *ov.Description
			}
		}
		if v.removed {
			m.Status = federation.StatusCancelled
		}
		for _, email := range v.attendees {
			a := Attendee{Email: email}
			if strings.EqualFold(email, v.meeting.organizer) {
				// The organizer attends their own meeting (RFC 15.5).
				yes := "yes"
				a.WillAttend, a.Revision = &yes, r.number
			} else if given, ok := answers.effective(email, o.Key); ok {
				answer, at := given.answer, rfc3339(given.at)
				a.WillAttend, a.Revision, a.AnsweredAt = &answer, given.revision, &at
				a.Outdated = given.revision < r.number
			}
			m.Attendees = append(m.Attendees, a)
			if strings.EqualFold(email, v.self) {
				m.WillAttend = a.WillAttend
			}
		}
		if m.Attendees == nil {
			m.Attendees = []Attendee{}
		}
		out = append(out, listed{meeting: m, start: o.Start, end: o.End})
	}
	return out, nil
}

// widthFractions returns, for each range, the largest number of ranges that
// overlap at once anywhere in its connected group. A day view divides the
// available width by this number.
func widthFractions(ranges [][2]int64) []int {
	out := make([]int, len(ranges))
	if len(ranges) == 0 {
		return out
	}
	type event struct {
		time  int64
		delta int
		idx   int
	}
	var events []event
	for i, r := range ranges {
		events = append(events, event{time: r[0], delta: +1, idx: i})
		events = append(events, event{time: r[1], delta: -1, idx: i})
	}
	// Ends sort before starts at the same instant, so touching ranges do not
	// count as overlapping.
	sort.SliceStable(events, func(a, b int) bool {
		if events[a].time == events[b].time {
			return events[a].delta < events[b].delta
		}
		return events[a].time < events[b].time
	})

	var group []int
	maxGroup, current := 0, 0
	for _, e := range events {
		if e.delta == +1 {
			group = append(group, e.idx)
			current++
			if current > maxGroup {
				maxGroup = current
			}
			continue
		}
		current--
		if current == 0 {
			for _, idx := range group {
				out[idx] = maxGroup
			}
			group = nil
			maxGroup = 0
		}
	}
	return out
}

// horizontalPositions assigns each range the lowest track index on which it
// does not overlap an already placed range.
func horizontalPositions(ranges [][2]int64) []int {
	out := make([]int, len(ranges))
	tracks := make([][][2]int64, len(ranges))
	overlaps := func(a, b [2]int64) bool { return a[1] > b[0] && a[0] < b[1] }
	for i, r := range ranges {
		for t := 0; t < len(ranges); t++ {
			clash := false
			for _, placed := range tracks[t] {
				if overlaps(r, placed) {
					clash = true
					break
				}
			}
			if !clash {
				out[i] = t
				tracks[t] = append(tracks[t], r)
				break
			}
		}
	}
	return out
}

// DeleteMeeting hides a meeting from the customer. It is not an answer: they
// stay an attendee, and a new revision brings the meeting back (RFC 15.7,
// ADR 22).
func (s *Service) DeleteMeeting(ctx context.Context, customerID, meetingID int64) (bool, error) {
	n, err := store.Affected(ctx, s.db,
		`UPDATE "meeting" SET "deleted_at" = ? WHERE "customer_id" = ? AND "id" = ? AND "deleted_at" IS NULL`,
		s.db.Now(), customerID, meetingID)
	if err != nil {
		return false, fmt.Errorf("delete meeting: %w", err)
	}
	return n > 0, nil
}

// GetDocument returns a meeting document, if the customer is shown the
// revision it belongs to or a later one.
func (s *Service) GetDocument(ctx context.Context, customerID, documentID int64) (*Document, error) {
	var d Document
	var hash string
	var gMeetingID int64
	var docRevision int
	err := s.db.QueryRowContext(ctx,
		`SELECT "g_meeting_document"."file_name", "g_meeting_document"."mime_type", "g_meeting_document"."file_size", `+
			`"g_meeting_document"."hash_md5", "g_meeting_revision"."g_meeting_id", "g_meeting_revision"."revision" `+
			`FROM "g_meeting_document" JOIN "g_meeting_revision" `+
			`ON "g_meeting_revision"."id" = "g_meeting_document"."g_meeting_revision_id" `+
			`WHERE "g_meeting_document"."id" = ? LIMIT 1`, documentID).
		Scan(&d.FileName, &d.MimeType, &d.FileSize, &hash, &gMeetingID, &docRevision)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look up document: %w", err)
	}
	mailboxIDs, err := store.SelectInts(ctx, s.db,
		`SELECT "mailbox_id" FROM "meeting" WHERE "customer_id" = ? AND "g_meeting_id" = ?`, customerID, gMeetingID)
	if err != nil {
		return nil, fmt.Errorf("look up meeting projection: %w", err)
	}
	visible := false
	for _, mailboxID := range mailboxIDs {
		aliases, err := aliasesOf(ctx, s.db, mailboxID)
		if err != nil {
			return nil, err
		}
		v, err := s.viewOf(ctx, s.db, gMeetingID, aliases)
		if err != nil {
			return nil, err
		}
		if v != nil && v.revision.number >= docRevision {
			visible = true
			break
		}
	}
	if !visible {
		return nil, nil
	}
	data, err := s.blob.Get(hash)
	if err != nil {
		return nil, err
	}
	d.FileData = data
	return &d, nil
}

func contains(list []string, address string) bool {
	for _, a := range list {
		if strings.EqualFold(a, address) {
			return true
		}
	}
	return false
}

func without(list []string, drop []string) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		if !contains(drop, a) {
			out = append(out, a)
		}
	}
	return out
}
