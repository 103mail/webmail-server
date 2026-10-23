// Package calendar implements the calendaring domain: calendars, meetings,
// attendees and documents.
//
// It is a reimplementation rather than a direct port. The PHP CalendarClient
// reads a `calendar` table and g_meeting columns (g_schedule_id, repeat_rule,
// sender_email, sender_name) that exist in neither the live database nor the
// test schema, so it cannot run as written. This package follows the schema and
// the OpenAPI definition instead, and treats a calendar as a mailbox, which is
// what the meeting listing already assumes by reporting meeting.mailbox_id as
// the calendarId.
package calendar

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/103mail/webmail-server/internal/blob"
	"github.com/103mail/webmail-server/internal/mdconv"
	"github.com/103mail/webmail-server/internal/rrule"
	"github.com/103mail/webmail-server/internal/store"
)

// Service implements the calendar domain.
type Service struct {
	db   *store.DB
	blob *blob.Store
}

// New returns a Service. documents is the directory holding document bytes.
func New(db *store.DB, documents *blob.Store) *Service {
	return &Service{db: db, blob: documents}
}

// Calendar is a mailbox viewed as a calendar.
type Calendar struct {
	CalendarID int64  `json:"calendarId"`
	MailboxID  int64  `json:"mailboxId"`
	Name       string `json:"name"`
	Color      string `json:"color"`
	IsShared   bool   `json:"isShared"`
}

// Meeting is an event as one customer sees it, including the layout hints the
// day view uses to place overlapping events side by side.
type Meeting struct {
	MeetingID          int64   `json:"meetingId"`
	CalendarID         int64   `json:"calendarId"`
	Title              string  `json:"title"`
	StartTime          string  `json:"startTime"`
	EndTime            string  `json:"endTime"`
	TimeZone           string  `json:"timeZone"`
	IsAllDay           bool    `json:"isAllDay"`
	OrganizerEmail     string  `json:"organizerEmail"`
	OrganizerName      string  `json:"organizerName"`
	Description        string  `json:"description"`
	WillAttend         *string `json:"willAttend"`
	WidthFraction      int     `json:"widthFraction"`
	HorizontalPosition int     `json:"horizontalPosition"`
}

// Document is a meeting attachment with its bytes.
type Document struct {
	FileName string `json:"fileName"`
	MimeType string `json:"mimeType"`
	FileSize int64  `json:"fileSize"`
	FileData []byte `json:"fileData"`
}

// NewMeeting is the input for creating an event.
type NewMeeting struct {
	Attendees       []string
	Title           string
	StartTime       time.Time
	EndTime         time.Time
	TimeZone        string
	RepeatRule      string
	DescriptionHTML string
	IsAllDay        bool
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

// ListMeetings returns the meetings in the given calendars between from and
// until, grouped by calendar day. A meeting spanning several days appears under
// each day it covers.
func (s *Service) ListMeetings(ctx context.Context, customerID int64, calendarIDs []int64,
	from, until time.Time) (map[string][]Meeting, error) {
	days := map[string][]Meeting{}
	for d := from.UTC().Truncate(24 * time.Hour); d.Before(until); d = d.AddDate(0, 0, 1) {
		days[d.Format("2006-01-02")] = []Meeting{}
	}
	if len(calendarIDs) == 0 {
		return days, nil
	}

	in := store.Placeholders(len(calendarIDs))
	args := append([]any{customerID, from, until}, store.Args(calendarIDs)...)
	rows, err := s.db.QueryContext(ctx,
		`SELECT "meeting"."id", "meeting"."mailbox_id", "meeting"."will_attend", `+
			`"g_meeting"."title", "g_meeting"."start_time", "g_meeting"."end_time", `+
			`"g_meeting"."time_zone", "g_meeting"."is_all_day", "g_meeting"."organizer_email", `+
			`"g_meeting"."organizer_name", "g_meeting"."description" FROM "meeting" `+
			`JOIN "g_meeting" ON "meeting"."g_meeting_id" = "g_meeting"."id" `+
			`WHERE "meeting"."customer_id" = ? AND "g_meeting"."start_time" >= ? `+
			`AND "g_meeting"."start_time" < ? AND "meeting"."mailbox_id" IN (`+in+") "+
			`ORDER BY "g_meeting"."start_time"`, args...)
	if err != nil {
		return nil, fmt.Errorf("list meetings: %w", err)
	}
	defer rows.Close()

	type span struct {
		meeting    Meeting
		start, end time.Time
	}
	var spans []span
	for rows.Next() {
		var m Meeting
		var willAttend sql.NullString
		var start, end time.Time
		if err := rows.Scan(&m.MeetingID, &m.CalendarID, &willAttend, &m.Title, &start, &end,
			&m.TimeZone, &m.IsAllDay, &m.OrganizerEmail, &m.OrganizerName, &m.Description); err != nil {
			return nil, fmt.Errorf("scan meeting: %w", err)
		}
		if willAttend.Valid {
			v := willAttend.String
			m.WillAttend = &v
		}
		m.StartTime = rfc3339(start)
		m.EndTime = rfc3339(end)
		m.WidthFraction = 1
		spans = append(spans, span{meeting: m, start: start, end: end})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list meetings: %w", err)
	}

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

// AddMeeting creates an event, expanding a recurrence rule into concrete
// meetings, and gives every attendee a projection. It returns the organizer's
// meeting projection id for the first occurrence.
func (s *Service) AddMeeting(ctx context.Context, customerID, calendarID int64, in NewMeeting) (int64, error) {
	rule, err := rrule.Parse(in.RepeatRule)
	if err != nil {
		return 0, fmt.Errorf("parse repeat rule: %w", err)
	}
	attendees := make([]string, len(in.Attendees))
	for i, email := range in.Attendees {
		attendees[i] = store.Address(email)
	}
	in.Attendees = attendees

	var meetingID int64
	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		organizer, err := senderIdentity(ctx, tx, customerID, calendarID)
		if err != nil {
			return err
		}
		if organizer.Email == "" {
			return nil
		}

		humanReadable := rule.HumanReadable()
		gScheduleID, err := store.Insert(ctx, tx,
			`INSERT INTO "g_schedule" ("start_time", "end_time", "time_zone", "repeat_rule", `+
				`"recurrence_dates", "exception_dates", "repeat_rule_human_readable") VALUES (?,?,?,?,?,?,?)`,
			in.StartTime, in.EndTime, in.TimeZone, in.RepeatRule, "", "", humanReadable)
		if err != nil {
			return fmt.Errorf("create schedule: %w", err)
		}

		starts := []time.Time{in.StartTime}
		if rule != nil {
			// The PHP expands at most 100 occurrences; keeping that bound means
			// an unbounded rule cannot fill the table.
			starts = rule.Occurrences(in.StartTime, 100)
			if len(starts) == 0 {
				starts = []time.Time{in.StartTime}
			}
		}
		duration := in.EndTime.Sub(in.StartTime)
		description := mdconv.ToMarkdown(in.DescriptionHTML)

		for i, start := range starts {
			id, err := s.addOccurrence(ctx, tx, gScheduleID, organizer, in, start, start.Add(duration), description)
			if err != nil {
				return err
			}
			if i == 0 {
				meetingID = id
			}
		}
		return nil
	})
	return meetingID, err
}

func (s *Service) addOccurrence(ctx context.Context, tx *sql.Tx, gScheduleID int64,
	organizer identity, in NewMeeting, start, end time.Time, description string) (int64, error) {
	gMeetingID, err := store.Insert(ctx, tx,
		`INSERT INTO "g_meeting" ("g_schedule", "title", "start_time", "end_time", "time_zone", `+
			`"is_all_day", "organizer_email", "organizer_name", "description", "description_html", `+
			`"created_at") VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		gScheduleID, in.Title, start, end, in.TimeZone, in.IsAllDay,
		organizer.Email, organizer.Name, description, in.DescriptionHTML, s.db.Now())
	if err != nil {
		return 0, fmt.Errorf("create meeting: %w", err)
	}

	// The organizer attends their own meeting.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO "g_attendee" ("g_meeting_id", "email", "will_attend") VALUES (?,?,'yes')`,
		gMeetingID, organizer.Email); err != nil {
		return 0, fmt.Errorf("add organizer as attendee: %w", err)
	}
	for _, email := range in.Attendees {
		if email == "" || email == organizer.Email {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO "g_attendee" ("g_meeting_id", "email") VALUES (?,?)`,
			gMeetingID, email); err != nil {
			return 0, fmt.Errorf("add attendee %s: %w", email, err)
		}
	}

	var meetingID int64
	projected := map[int64]bool{}
	for _, email := range append([]string{organizer.Email}, in.Attendees...) {
		if email == "" {
			continue
		}
		mailboxID, err := store.SelectInt(ctx, tx,
			`SELECT "mailbox_id" FROM "alias" WHERE "email" = ? LIMIT 1`, email)
		if err != nil {
			return 0, fmt.Errorf("look up attendee mailbox: %w", err)
		}
		if mailboxID == 0 || projected[mailboxID] {
			continue
		}
		projected[mailboxID] = true
		attendeeCustomerID, err := store.SelectInt(ctx, tx,
			`SELECT "customer_id" FROM "mailbox" WHERE "id" = ? LIMIT 1`, mailboxID)
		if err != nil {
			return 0, fmt.Errorf("look up attendee customer: %w", err)
		}
		if attendeeCustomerID == 0 {
			continue
		}

		var scheduleID any
		var willAttend any
		if email == organizer.Email {
			id, err := store.Insert(ctx, tx,
				`INSERT INTO "schedule" ("customer_id", "g_schedule_id", "mailbox_id") VALUES (?,?,?)`,
				attendeeCustomerID, gScheduleID, mailboxID)
			if err != nil {
				return 0, fmt.Errorf("create schedule projection: %w", err)
			}
			scheduleID = id
			willAttend = "yes"
		}
		id, err := store.Insert(ctx, tx,
			`INSERT INTO "meeting" ("customer_id", "g_meeting_id", "mailbox_id", "schedule_id", "will_attend") `+
				"VALUES (?,?,?,?,?)",
			attendeeCustomerID, gMeetingID, mailboxID, scheduleID, willAttend)
		if err != nil {
			return 0, fmt.Errorf("create meeting projection: %w", err)
		}
		if email == organizer.Email {
			meetingID = id
		}
	}
	return meetingID, nil
}

type identity struct {
	Email string
	Name  string
}

func senderIdentity(ctx context.Context, q store.Queryer, customerID, mailboxID int64) (identity, error) {
	var id identity
	err := q.QueryRowContext(ctx,
		`SELECT "alias"."email", "alias"."name" FROM "mailbox" `+
			`JOIN "alias" ON "alias"."id" = "mailbox"."default_alias_id" `+
			`WHERE "mailbox"."customer_id" = ? AND "mailbox"."id" = ? LIMIT 1`,
		customerID, mailboxID).Scan(&id.Email, &id.Name)
	if err == sql.ErrNoRows {
		return identity{}, nil
	}
	if err != nil {
		return identity{}, fmt.Errorf("look up organizer alias: %w", err)
	}
	return id, nil
}

// SetRSVP records an attendee's response. It writes a new g_attendee row as the
// audit trail and updates the projection to the effective answer.
func (s *Service) SetRSVP(ctx context.Context, customerID, meetingID int64, willAttend string) (bool, error) {
	switch willAttend {
	case "yes", "no", "maybe":
	default:
		return false, nil
	}
	var ok bool
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		var gMeetingID, mailboxID int64
		err := tx.QueryRowContext(ctx,
			`SELECT "g_meeting_id", "mailbox_id" FROM "meeting" WHERE "customer_id" = ? AND "id" = ? LIMIT 1`,
			customerID, meetingID).Scan(&gMeetingID, &mailboxID)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return fmt.Errorf("look up meeting: %w", err)
		}
		attendee, err := senderIdentity(ctx, tx, customerID, mailboxID)
		if err != nil {
			return err
		}
		if attendee.Email == "" {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE "meeting" SET "will_attend" = ? WHERE "customer_id" = ? AND "id" = ?`,
			willAttend, customerID, meetingID); err != nil {
			return fmt.Errorf("update rsvp projection: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO "g_attendee" ("g_meeting_id", "email", "will_attend") VALUES (?,?,?)`,
			gMeetingID, attendee.Email, willAttend); err != nil {
			return fmt.Errorf("record rsvp: %w", err)
		}
		ok = true
		return nil
	})
	return ok, err
}

// DeleteMeeting removes the customer's projection of a meeting. The canonical
// meeting stays for the other attendees.
func (s *Service) DeleteMeeting(ctx context.Context, customerID, meetingID int64) (bool, error) {
	n, err := store.Affected(ctx, s.db,
		`DELETE FROM "meeting" WHERE "customer_id" = ? AND "id" = ?`, customerID, meetingID)
	if err != nil {
		return false, fmt.Errorf("delete meeting: %w", err)
	}
	return n > 0, nil
}

// AddAttendee invites an address to a meeting and gives it a projection.
func (s *Service) AddAttendee(ctx context.Context, customerID, meetingID int64, email string) (bool, error) {
	email = store.Address(email)
	var ok bool
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		gMeetingID, err := store.SelectInt(ctx, tx,
			`SELECT "g_meeting_id" FROM "meeting" WHERE "customer_id" = ? AND "id" = ? LIMIT 1`,
			customerID, meetingID)
		if err != nil {
			return fmt.Errorf("look up meeting: %w", err)
		}
		if gMeetingID == 0 {
			return nil
		}
		existing, err := store.SelectInt(ctx, tx,
			`SELECT "id" FROM "g_attendee" WHERE "g_meeting_id" = ? AND "email" = ? LIMIT 1`,
			gMeetingID, email)
		if err != nil {
			return fmt.Errorf("look up attendee: %w", err)
		}
		if existing == 0 {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO "g_attendee" ("g_meeting_id", "email") VALUES (?,?)`,
				gMeetingID, email); err != nil {
				return fmt.Errorf("add attendee: %w", err)
			}
		}
		mailboxID, err := store.SelectInt(ctx, tx,
			`SELECT "mailbox_id" FROM "alias" WHERE "email" = ? LIMIT 1`, email)
		if err != nil {
			return fmt.Errorf("look up attendee mailbox: %w", err)
		}
		if mailboxID != 0 {
			attendeeCustomerID, err := store.SelectInt(ctx, tx,
				`SELECT "customer_id" FROM "mailbox" WHERE "id" = ? LIMIT 1`, mailboxID)
			if err != nil {
				return fmt.Errorf("look up attendee customer: %w", err)
			}
			present, err := store.SelectInt(ctx, tx,
				`SELECT "id" FROM "meeting" WHERE "customer_id" = ? AND "g_meeting_id" = ? LIMIT 1`,
				attendeeCustomerID, gMeetingID)
			if err != nil {
				return fmt.Errorf("look up meeting projection: %w", err)
			}
			if attendeeCustomerID != 0 && present == 0 {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO "meeting" ("customer_id", "g_meeting_id", "mailbox_id", "schedule_id") `+
						"VALUES (?,?,?,NULL)",
					attendeeCustomerID, gMeetingID, mailboxID); err != nil {
					return fmt.Errorf("create meeting projection: %w", err)
				}
			}
		}
		ok = true
		return nil
	})
	return ok, err
}

// GetDocument returns a meeting document, if the customer attends the meeting.
func (s *Service) GetDocument(ctx context.Context, customerID, documentID int64) (*Document, error) {
	var d Document
	var hash string
	err := s.db.QueryRowContext(ctx,
		`SELECT "g_document"."file_name", "g_document"."mime_type", "g_document"."file_size", `+
			`"g_document"."hash_md5" FROM "g_document" `+
			`JOIN "meeting" ON "meeting"."g_meeting_id" = "g_document"."g_meeting_id" `+
			`WHERE "g_document"."id" = ? AND "meeting"."customer_id" = ? LIMIT 1`,
		documentID, customerID).Scan(&d.FileName, &d.MimeType, &d.FileSize, &hash)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look up document: %w", err)
	}
	data, err := s.blob.Get(hash)
	if err != nil {
		return nil, err
	}
	d.FileData = data
	return &d, nil
}

// AddDocument attaches a file to a meeting.
func (s *Service) AddDocument(ctx context.Context, customerID, meetingID int64,
	fileName, mimeType string, data []byte) (int64, error) {
	gMeetingID, err := store.SelectInt(ctx, s.db,
		`SELECT "g_meeting_id" FROM "meeting" WHERE "customer_id" = ? AND "id" = ? LIMIT 1`,
		customerID, meetingID)
	if err != nil {
		return 0, fmt.Errorf("look up meeting: %w", err)
	}
	if gMeetingID == 0 {
		return 0, nil
	}
	hash, err := s.blob.Put(data)
	if err != nil {
		return 0, err
	}
	id, err := store.Insert(ctx, s.db,
		`INSERT INTO "g_document" ("g_meeting_id", "file_name", "mime_type", "file_size", "hash_md5") `+
			"VALUES (?,?,?,?,?)",
		gMeetingID, fileName, mimeType, len(data), hash)
	if err != nil {
		return 0, fmt.Errorf("create document: %w", err)
	}
	return id, nil
}

// DeleteDocument removes a document, if the customer attends its meeting.
func (s *Service) DeleteDocument(ctx context.Context, customerID, documentID int64) (bool, error) {
	gMeetingID, err := store.SelectInt(ctx, s.db,
		`SELECT "g_document"."g_meeting_id" FROM "g_document" `+
			`JOIN "meeting" ON "meeting"."g_meeting_id" = "g_document"."g_meeting_id" `+
			`WHERE "g_document"."id" = ? AND "meeting"."customer_id" = ? LIMIT 1`,
		documentID, customerID)
	if err != nil {
		return false, fmt.Errorf("look up document: %w", err)
	}
	if gMeetingID == 0 {
		return false, nil
	}
	n, err := store.Affected(ctx, s.db, `DELETE FROM "g_document" WHERE "id" = ?`, documentID)
	if err != nil {
		return false, fmt.Errorf("delete document: %w", err)
	}
	return n > 0, nil
}
