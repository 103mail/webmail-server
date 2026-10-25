package calendar

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/103mail/webmail-server/internal/federation"
	"github.com/103mail/webmail-server/internal/mdconv"
	"github.com/103mail/webmail-server/internal/rrule"
	"github.com/103mail/webmail-server/internal/store"
)

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

// Change is what the organizer changes about a meeting. A nil field stays as
// it is. With Occurrence set, the change is to that occurrence alone, as an
// override, and only Status, Title, DescriptionHTML, StartTime and EndTime
// apply (RFC 15.6).
type Change struct {
	Occurrence      string
	Status          *string
	Title           *string
	DescriptionHTML *string
	StartTime       *time.Time
	EndTime         *time.Time
	TimeZone        *string
	IsAllDay        *bool
	RepeatRule      *string
}

// writeRevision stores a revision with its attendees and documents. The row
// is never changed afterwards (ADR 22).
func (s *Service) writeRevision(ctx context.Context, tx *sql.Tx, gMeetingID int64, r *revision,
	attendees []string, docs []document) error {
	series, err := r.series()
	if err != nil {
		return err
	}
	first, last, bounded := series.Bounds()
	var lastEnd any
	if bounded {
		lastEnd = last
	}
	var repeat, overrides any
	if r.repeat != nil {
		raw, err := json.Marshal(r.repeat)
		if err != nil {
			return fmt.Errorf("serialize repeat: %w", err)
		}
		repeat = string(raw)
	}
	if len(r.overrides) > 0 {
		raw, err := json.Marshal(r.overrides)
		if err != nil {
			return fmt.Errorf("serialize overrides: %w", err)
		}
		overrides = string(raw)
	}
	var html any
	if r.html != "" {
		html = r.html
	}
	now := s.db.Now()
	r.id, err = store.Insert(ctx, tx,
		`INSERT INTO "g_meeting_revision" ("g_meeting_id", "revision", "status", "title", "description", `+
			`"description_html", "start_time", "end_time", "time_zone", "is_all_day", "repeat", "overrides", `+
			`"first_start", "last_end", "changed_at", "created_at") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		gMeetingID, r.number, r.status, r.title, r.description, html, r.start, r.end, r.timeZone, r.allDay,
		repeat, overrides, first, lastEnd, now, now)
	if err != nil {
		return fmt.Errorf("create meeting revision: %w", err)
	}
	var seen []string
	for _, a := range attendees {
		a = store.Address(a)
		if a == "" || contains(seen, a) {
			continue
		}
		seen = append(seen, a)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO "g_meeting_attendee" ("g_meeting_revision_id", "email") VALUES (?,?)`, r.id, a); err != nil {
			return fmt.Errorf("add attendee %s: %w", a, err)
		}
	}
	for _, d := range docs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO "g_meeting_document" ("g_meeting_revision_id", "federation_id", "file_name", "mime_type", `+
				`"file_size", "hash_md5", "hash_sha256") VALUES (?,?,?,?,?,?,?)`,
			r.id, d.fedID, d.name, d.mime, d.size, d.md5, d.sha256); err != nil {
			return fmt.Errorf("add document: %w", err)
		}
	}
	return nil
}

// project gives an address with an account here its copy of a meeting, and
// with revive brings back a copy it deleted, as a new revision does (ADR 22).
// It reports whether the address has an account.
func project(ctx context.Context, tx *sql.Tx, gMeetingID int64, address string, revive bool) (bool, error) {
	mailboxID, err := store.SelectInt(ctx, tx,
		`SELECT "mailbox_id" FROM "alias" WHERE "email" = ? LIMIT 1`, address)
	if err != nil {
		return false, fmt.Errorf("look up attendee mailbox: %w", err)
	}
	if mailboxID == 0 {
		return false, nil
	}
	customerID, err := store.SelectInt(ctx, tx,
		`SELECT "customer_id" FROM "mailbox" WHERE "id" = ? LIMIT 1`, mailboxID)
	if err != nil {
		return false, fmt.Errorf("look up attendee customer: %w", err)
	}
	if customerID == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO "meeting" ("customer_id", "g_meeting_id", "mailbox_id") VALUES (?,?,?) `+
			`ON CONFLICT ("mailbox_id", "g_meeting_id") DO NOTHING`, customerID, gMeetingID, mailboxID); err != nil {
		return false, fmt.Errorf("create meeting projection: %w", err)
	}
	if revive {
		if _, err := tx.ExecContext(ctx,
			`UPDATE "meeting" SET "deleted_at" = NULL WHERE "mailbox_id" = ? AND "g_meeting_id" = ? `+
				`AND "deleted_at" IS NOT NULL`, mailboxID, gMeetingID); err != nil {
			return false, fmt.Errorf("restore deleted meeting: %w", err)
		}
	}
	return true, nil
}

// revise stacks a new revision written by the organizer on the latest one,
// shows it to the attendees here and sends it to the others (RFC 15.3).
// removed are the addresses it takes out, which are told so; added are the
// ones it invites.
func (s *Service) revise(ctx context.Context, tx *sql.Tx, m *meetingRow, prev, next *revision,
	attendees []string, docs []document, removed, added []string) error {
	next.number = prev.number + 1
	if err := s.writeRevision(ctx, tx, m.id, next, attendees, docs); err != nil {
		return err
	}
	if err := s.show(ctx, tx, m, next, attendees); err != nil {
		return err
	}
	return s.publish(ctx, tx, m, next, attendees, removed, added)
}

// show projects a revision written here for its local attendees, and marks
// it seen by the organizer, who wrote it.
func (s *Service) show(ctx context.Context, tx *sql.Tx, m *meetingRow, r *revision, attendees []string) error {
	for _, a := range attendees {
		if s.remote(a) {
			continue
		}
		if _, err := project(ctx, tx, m.id, a, true); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE "meeting" SET "seen_revision" = ? WHERE "g_meeting_id" = ? AND "seen_revision" < ? `+
			`AND "mailbox_id" IN (SELECT "mailbox_id" FROM "alias" WHERE "email" = ?)`,
		r.number, m.id, r.number, m.organizer); err != nil {
		return fmt.Errorf("mark meeting seen: %w", err)
	}
	return nil
}

// AddMeeting creates a meeting as its first revision, organized by the
// calendar's owner, and gives every attendee here a copy. Attendees on a
// Domain that does not do calendar, or on none, are left out, and the
// organizer is told in their inbox (RFC 15.1). It returns the organizer's
// meeting id.
func (s *Service) AddMeeting(ctx context.Context, customerID, calendarID int64, in NewMeeting) (int64, error) {
	rule, err := rrule.Parse(in.RepeatRule)
	if err != nil {
		return 0, fmt.Errorf("parse repeat rule: %w", err)
	}
	repeat, err := rrule.FromRule(rule)
	if err != nil {
		return 0, fmt.Errorf("parse repeat rule: %w", err)
	}
	organizer, err := senderIdentity(ctx, s.db, customerID, calendarID)
	if err != nil || organizer.Email == "" {
		return 0, err
	}
	organizer.Email = store.Address(organizer.Email)
	r := &revision{
		number:      1,
		status:      federation.StatusConfirmed,
		title:       in.Title,
		description: mdconv.ToMarkdown(in.DescriptionHTML),
		html:        in.DescriptionHTML,
		start:       in.StartTime.UTC(),
		end:         in.EndTime.UTC(),
		timeZone:    in.TimeZone,
		allDay:      in.IsAllDay,
		repeat:      repeat,
	}
	if err := normalize(r); err != nil {
		return 0, err
	}
	var invitees []string
	for _, a := range in.Attendees {
		if a = store.Address(a); a != "" && a != organizer.Email && !contains(invitees, a) {
			invitees = append(invitees, a)
		}
	}
	invitees, unreachable := s.reachable(ctx, invitees)

	fedID, err := federation.NewUUID()
	if err != nil {
		return 0, err
	}
	var meetingID int64
	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		gMeetingID, err := store.Insert(ctx, tx,
			`INSERT INTO "g_meeting" ("federation_id", "organizer_email", "organizer_name", "created_at") `+
				"VALUES (?,?,?,?)", fedID, organizer.Email, organizer.Name, s.db.Now())
		if err != nil {
			return fmt.Errorf("create meeting: %w", err)
		}
		m := &meetingRow{id: gMeetingID, fedID: fedID, organizer: organizer.Email, organizerName: organizer.Name}
		attendees := append([]string{organizer.Email}, invitees...)
		if err := s.writeRevision(ctx, tx, gMeetingID, r, attendees, nil); err != nil {
			return err
		}
		if err := s.show(ctx, tx, m, r, attendees); err != nil {
			return err
		}
		if err := s.publish(ctx, tx, m, r, attendees, nil, nil); err != nil {
			return err
		}
		meetingID, err = store.SelectInt(ctx, tx,
			`SELECT "id" FROM "meeting" WHERE "mailbox_id" = ? AND "g_meeting_id" = ? LIMIT 1`, calendarID, gMeetingID)
		return err
	})
	if err != nil {
		return 0, err
	}
	s.tellUnreachable(ctx, organizer.Email, in.Title, unreachable)
	return meetingID, nil
}

// normalize holds a revision to the rules of RFC 15.2 and 15.6 before it is
// written: an IANA zone, an end after the start, and for an all-day meeting
// whole days from midnight to midnight in its zone.
func normalize(r *revision) error {
	loc, rejection := federation.LoadTimeZone(r.timeZone)
	if rejection != nil {
		return badInput("unknown time zone %q", r.timeZone)
	}
	if len(r.title) > 255 {
		return badInput("the title is over 255 bytes")
	}
	if r.allDay {
		start, end := r.start.In(loc), r.end.In(loc)
		r.start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc).UTC()
		last := time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, loc)
		if !federation.Midnight(r.end, loc) {
			last = last.AddDate(0, 0, 1)
		}
		r.end = last.UTC()
		if !r.end.After(r.start) {
			r.end = r.start.In(loc).AddDate(0, 0, 1).UTC()
		}
	}
	if !r.end.After(r.start) {
		return badInput("the meeting ends before it starts")
	}
	if r.repeat != nil {
		if err := r.repeat.Validate(); err != nil {
			return badInput("%v", err)
		}
	}
	return nil
}

// organizing is a meeting as its organizer is about to change it.
type organizing struct {
	projection *projection
	meeting    *meetingRow
	latest     *revision
	attendees  []string
	docs       []document
}

// organize reads a meeting for a change by the customer, who must be its
// organizer on a Domain hosted here (RFC 15.4, ADR 22). A meeting the
// customer does not have is nil.
func (s *Service) organize(ctx context.Context, q store.Queryer, customerID, meetingID int64) (*organizing, error) {
	p, err := projectionOf(ctx, q, customerID, meetingID)
	if err != nil || p == nil {
		return nil, err
	}
	m, err := loadMeeting(ctx, q, p.gMeetingID)
	if err != nil || m == nil {
		return nil, err
	}
	if !contains(p.aliases, m.organizer) || s.remote(m.organizer) {
		return nil, ErrNotOrganizer
	}
	o := &organizing{projection: p, meeting: m}
	if o.latest, err = latestRevision(ctx, q, m.id); err != nil || o.latest == nil {
		return nil, err
	}
	if o.attendees, err = attendeesOf(ctx, q, o.latest.id); err != nil {
		return nil, err
	}
	if o.docs, err = documentsOf(ctx, q, o.latest.id); err != nil {
		return nil, err
	}
	return o, nil
}

// next is a copy of the latest revision to change into the next one.
func (o *organizing) next() *revision {
	r := *o.latest
	r.overrides = append([]rrule.Override(nil), o.latest.overrides...)
	return &r
}

// UpdateMeeting writes the organizer's change as a new revision: moving,
// renaming, cancelling or confirming the meeting again, changing its rule,
// or with Occurrence, changing or cancelling one occurrence (RFC 15.3, 15.6).
func (s *Service) UpdateMeeting(ctx context.Context, customerID, meetingID int64, ch Change) (bool, error) {
	var ok bool
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		ok = false
		o, err := s.organize(ctx, tx, customerID, meetingID)
		if err != nil || o == nil {
			return err
		}
		next := o.next()
		if ch.Status != nil && *ch.Status != federation.StatusConfirmed && *ch.Status != federation.StatusCancelled {
			return badInput("status is confirmed or cancelled")
		}
		if ch.Occurrence != "" {
			if err := s.changeOccurrence(next, ch); err != nil {
				return err
			}
		} else if err := s.changeMeeting(next, ch); err != nil {
			return err
		}
		if err := s.revise(ctx, tx, o.meeting, o.latest, next, o.attendees, o.docs, nil, nil); err != nil {
			return err
		}
		ok = true
		return nil
	})
	return ok, err
}

func (s *Service) changeMeeting(r *revision, ch Change) error {
	moved := false
	if ch.Status != nil {
		r.status = *ch.Status
	}
	if ch.Title != nil {
		r.title = *ch.Title
	}
	if ch.DescriptionHTML != nil {
		r.html = *ch.DescriptionHTML
		r.description = mdconv.ToMarkdown(*ch.DescriptionHTML)
	}
	if ch.TimeZone != nil {
		r.timeZone, moved = *ch.TimeZone, true
	}
	if ch.IsAllDay != nil {
		r.allDay, moved = *ch.IsAllDay, true
	}
	duration := r.end.Sub(r.start)
	if ch.StartTime != nil {
		r.start, moved = ch.StartTime.UTC(), true
		if ch.EndTime == nil {
			r.end = r.start.Add(duration)
		}
	}
	if ch.EndTime != nil {
		r.end = ch.EndTime.UTC()
	}
	if ch.RepeatRule != nil {
		rule, err := rrule.Parse(*ch.RepeatRule)
		if err != nil {
			return fmt.Errorf("parse repeat rule: %w", err)
		}
		if r.repeat, err = rrule.FromRule(rule); err != nil {
			return fmt.Errorf("parse repeat rule: %w", err)
		}
		moved = true
	}
	if err := normalize(r); err != nil {
		return err
	}
	if r.repeat == nil {
		r.overrides = nil
	}
	if moved && len(r.overrides) > 0 {
		// An override for an occurrence the new rule no longer gives is
		// left out of the next revision (RFC 15.6).
		series, err := r.series()
		if err != nil {
			return err
		}
		var horizon time.Time
		for _, ov := range r.overrides {
			if at, err := rrule.ParseInstant(ov.Occurrence); err == nil && at.After(horizon) {
				horizon = at
			}
		}
		keys := map[string]bool{}
		for _, occurrence := range series.Expand(horizon.Add(time.Second)) {
			keys[occurrence.Key] = true
		}
		var kept []rrule.Override
		for _, ov := range r.overrides {
			if keys[ov.Occurrence] {
				kept = append(kept, ov)
			}
		}
		r.overrides = kept
	}
	return nil
}

func (s *Service) changeOccurrence(r *revision, ch Change) error {
	if r.repeat == nil {
		return badInput("a single meeting has no occurrences to change")
	}
	if ch.TimeZone != nil || ch.IsAllDay != nil || ch.RepeatRule != nil {
		return badInput("the time zone, all-day and rule belong to the whole meeting")
	}
	key, err := s.occurrenceKey(r, ch.Occurrence)
	if err != nil {
		return err
	}
	i := -1
	for j := range r.overrides {
		if r.overrides[j].Occurrence == key {
			i = j
		}
	}
	if i < 0 {
		r.overrides = append(r.overrides, rrule.Override{Occurrence: key})
		i = len(r.overrides) - 1
	}
	ov := &r.overrides[i]
	if ch.Status != nil {
		ov.Status = *ch.Status
	}
	if ch.Title != nil {
		title := *ch.Title
		if len(title) > 255 {
			return badInput("the title is over 255 bytes")
		}
		ov.Title = &title
	}
	if ch.DescriptionHTML != nil {
		html, text := *ch.DescriptionHTML, mdconv.ToMarkdown(*ch.DescriptionHTML)
		ov.DescriptionHTML, ov.Description = &html, &text
	}
	if ch.StartTime != nil {
		ov.Start = rrule.FormatInstant(*ch.StartTime)
	}
	if ch.EndTime != nil {
		ov.End = rrule.FormatInstant(*ch.EndTime)
	}
	if ov.Start != "" && ov.End != "" {
		start, _ := rrule.ParseInstant(ov.Start)
		end, _ := rrule.ParseInstant(ov.End)
		if !end.After(start) {
			return badInput("the occurrence ends before it starts")
		}
	}
	return nil
}

// AddAttendee invites an address to a meeting, by a new revision that lists
// it. Only the organizer invites (RFC 15.4). An address the meeting cannot
// reach is not invited, and the organizer is told in their inbox.
func (s *Service) AddAttendee(ctx context.Context, customerID, meetingID int64, email string) (bool, error) {
	email = store.Address(email)
	o, err := s.organize(ctx, s.db, customerID, meetingID)
	if err != nil || o == nil {
		return false, err
	}
	if contains(o.attendees, email) {
		return true, nil
	}
	if _, unreachable := s.reachable(ctx, []string{email}); len(unreachable) > 0 {
		s.tellUnreachable(ctx, o.meeting.organizer, o.latest.title, unreachable)
		return false, nil
	}
	var ok bool
	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		ok = false
		o, err := s.organize(ctx, tx, customerID, meetingID)
		if err != nil || o == nil {
			return err
		}
		if contains(o.attendees, email) {
			ok = true
			return nil
		}
		attendees := append(append([]string{}, o.attendees...), email)
		if err := s.revise(ctx, tx, o.meeting, o.latest, o.next(), attendees, o.docs, nil, []string{email}); err != nil {
			return err
		}
		ok = true
		return nil
	})
	return ok, err
}

// RemoveAttendee takes an address out of a meeting by a new revision that
// leaves it out and names it as removed, which its Domain receives so that it
// shows them the meeting as cancelled (RFC 15.4). The organizer cannot be
// removed from their own meeting.
func (s *Service) RemoveAttendee(ctx context.Context, customerID, meetingID int64, email string) (bool, error) {
	email = store.Address(email)
	var ok bool
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		ok = false
		o, err := s.organize(ctx, tx, customerID, meetingID)
		if err != nil || o == nil {
			return err
		}
		if email == o.meeting.organizer {
			return badInput("the organizer attends their own meeting")
		}
		if !contains(o.attendees, email) {
			return nil
		}
		removed := []string{email}
		if err := s.revise(ctx, tx, o.meeting, o.latest, o.next(), without(o.attendees, removed), o.docs,
			removed, nil); err != nil {
			return err
		}
		ok = true
		return nil
	})
	return ok, err
}

// AddDocument attaches a file to a meeting, by a new revision that carries
// it with the documents it had (RFC 15.3). Only the organizer does. It
// returns the document's id in that revision.
func (s *Service) AddDocument(ctx context.Context, customerID, meetingID int64,
	fileName, mimeType string, data []byte) (int64, error) {
	md5, err := s.blob.Put(data)
	if err != nil {
		return 0, err
	}
	sum := sha256.Sum256(data)
	fedID, err := federation.NewUUID()
	if err != nil {
		return 0, err
	}
	var documentID int64
	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		documentID = 0
		o, err := s.organize(ctx, tx, customerID, meetingID)
		if err != nil || o == nil {
			return err
		}
		docs := append(append([]document{}, o.docs...), document{
			fedID: fedID, name: fileName, mime: mimeType, size: int64(len(data)), md5: md5,
			sha256: hex.EncodeToString(sum[:]),
		})
		next := o.next()
		if err := s.revise(ctx, tx, o.meeting, o.latest, next, o.attendees, docs, nil, nil); err != nil {
			return err
		}
		documentID, err = store.SelectInt(ctx, tx,
			`SELECT "id" FROM "g_meeting_document" WHERE "g_meeting_revision_id" = ? AND "federation_id" = ? LIMIT 1`,
			next.id, fedID)
		return err
	})
	return documentID, err
}

// DeleteDocument takes a document out of a meeting, by a new revision that no
// longer carries it. The revisions before keep it, as history (ADR 22).
func (s *Service) DeleteDocument(ctx context.Context, customerID, documentID int64) (bool, error) {
	var ok bool
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		ok = false
		var fedID string
		var gMeetingID int64
		err := tx.QueryRowContext(ctx,
			`SELECT "g_meeting_document"."federation_id", "g_meeting_revision"."g_meeting_id" FROM "g_meeting_document" `+
				`JOIN "g_meeting_revision" ON "g_meeting_revision"."id" = "g_meeting_document"."g_meeting_revision_id" `+
				`WHERE "g_meeting_document"."id" = ? LIMIT 1`, documentID).Scan(&fedID, &gMeetingID)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return fmt.Errorf("look up document: %w", err)
		}
		meetingID, err := store.SelectInt(ctx, tx,
			`SELECT "id" FROM "meeting" WHERE "customer_id" = ? AND "g_meeting_id" = ? ORDER BY "id" LIMIT 1`,
			customerID, gMeetingID)
		if err != nil || meetingID == 0 {
			return err
		}
		o, err := s.organize(ctx, tx, customerID, meetingID)
		if err != nil || o == nil {
			return err
		}
		var docs []document
		for _, d := range o.docs {
			if d.fedID != fedID {
				docs = append(docs, d)
			}
		}
		if len(docs) == len(o.docs) {
			return nil
		}
		if err := s.revise(ctx, tx, o.meeting, o.latest, o.next(), o.attendees, docs, nil, nil); err != nil {
			return err
		}
		ok = true
		return nil
	})
	return ok, err
}

// tellUnreachable writes the organizer the mailer-daemon message RFC 15.1
// asks for: the meeting, and who it cannot reach. It names nothing else of
// the meeting, and never leaves this Server.
func (s *Service) tellUnreachable(ctx context.Context, organizer, title string, addresses []string) {
	if len(addresses) == 0 {
		return
	}
	s.notice(ctx, organizer, title, fmt.Sprintf(
		"These addresses could not be invited to \"%s\": %s.\n\n"+
			"Their Domain does not take meetings from other Domains, or they are not on a Webmail Domain. "+
			"Nothing about the meeting was sent to them.\n", title, strings.Join(addresses, ", ")))
}

func (s *Service) notice(ctx context.Context, organizer, title, body string) {
	if s.notices == nil {
		return
	}
	// A notice that cannot be written is not worth failing the change for:
	// the meeting is stored either way.
	_ = s.notices.Notice(ctx, federation.DomainOf(organizer), organizer, "Invitation failed: "+title, body)
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
