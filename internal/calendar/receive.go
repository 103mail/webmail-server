package calendar

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/103mail/webmail-server/internal/federation"
	"github.com/103mail/webmail-server/internal/rrule"
	"github.com/103mail/webmail-server/internal/store"
)

// DeliverMeeting stores a revision accepted from the organizer's Domain,
// satisfying federation.CalendarDeliverer.
//
// Every revision is kept, in its place: one that arrives late is stored and
// changes nothing shown, since what is shown is the highest held (RFC 15.3,
// ADR 22). A meeting not held yet is created for those recipients that are
// attendees, whatever the revision. A recipient the revision leaves out keeps
// the copy they had, shown as cancelled. It returns how many recipients have
// an account here, and the ones that do not.
func (s *Service) DeliverMeeting(ctx context.Context, in federation.MeetingInbound) (int, []string, error) {
	// Document bytes go to the blob store before the transaction; a blob is
	// keyed by its content, so storing one twice stores it once.
	var docs []document
	for _, a := range in.Attachments {
		md5, err := s.blob.Put(a.Data)
		if err != nil {
			return 0, nil, err
		}
		sum := sha256.Sum256(a.Data)
		docs = append(docs, document{fedID: a.FederationID, name: a.FileName, mime: a.MimeType,
			size: int64(len(a.Data)), md5: md5, sha256: hex.EncodeToString(sum[:])})
	}
	r, err := fromWire(in.Meeting)
	if err != nil {
		return 0, nil, err
	}

	var delivered int
	var refused []string
	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		delivered, refused = 0, nil
		m, err := s.inboundMeeting(ctx, tx, in.Meeting)
		if err != nil {
			return err
		}
		prev, err := latestRevision(ctx, tx, m.id)
		if err != nil {
			return err
		}
		var prevAttendees []string
		if prev != nil {
			if prevAttendees, err = attendeesOf(ctx, tx, prev.id); err != nil {
				return err
			}
		}
		held, err := store.SelectInt(ctx, tx,
			`SELECT COUNT(*) FROM "g_meeting_revision" WHERE "g_meeting_id" = ? AND "revision" = ?`, m.id, r.number)
		if err != nil {
			return fmt.Errorf("look up meeting revision: %w", err)
		}
		if held == 0 {
			if err := s.writeRevision(ctx, tx, m.id, r, in.Meeting.Attendees, docs); err != nil {
				return err
			}
		}
		highest := prev == nil || r.number > prev.number

		for _, address := range in.Recipients {
			attending := contains(in.Meeting.Attendees, address)
			var has bool
			if attending {
				// A new highest revision brings back a deleted copy (ADR 22).
				has, err = project(ctx, tx, m.id, address, highest)
			} else {
				has, err = hasAccount(ctx, tx, address)
			}
			if err != nil {
				return err
			}
			if !has {
				refused = append(refused, address)
				continue
			}
			delivered++
		}
		if !highest || prev == nil {
			return nil
		}
		// Whoever this revision invites hears this Server's own answers from
		// it, as from every Domain that holds the meeting (RFC 15.4).
		added := without(in.Meeting.Attendees, prevAttendees)
		return s.sendAnswersTo(ctx, tx, m, in.Meeting.Attendees, added)
	})
	return delivered, refused, err
}

// inboundMeeting finds the meeting a revision is for, or creates it. A
// meeting held here under another organizer is refused, so that no Domain
// but the organizer's can change it (RFC 15.3).
func (s *Service) inboundMeeting(ctx context.Context, tx *sql.Tx, w federation.Meeting) (*meetingRow, error) {
	gMeetingID, err := store.SelectInt(ctx, tx,
		`SELECT "id" FROM "g_meeting" WHERE "federation_id" = ? LIMIT 1`, w.FederationID)
	if err != nil {
		return nil, fmt.Errorf("look up meeting: %w", err)
	}
	if gMeetingID != 0 {
		m, err := loadMeeting(ctx, tx, gMeetingID)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(m.organizer, w.Organizer.Email) {
			return nil, fmt.Errorf("meeting %s is organized by %s: %w", w.FederationID, m.organizer,
				federation.ErrWrongOrganizer)
		}
		return m, nil
	}
	m := &meetingRow{fedID: w.FederationID, organizer: strings.ToLower(w.Organizer.Email), organizerName: w.Organizer.Name}
	if m.id, err = store.Insert(ctx, tx,
		`INSERT INTO "g_meeting" ("federation_id", "organizer_email", "organizer_name", "created_at") VALUES (?,?,?,?)`,
		m.fedID, m.organizer, m.organizerName, s.db.Now()); err != nil {
		return nil, fmt.Errorf("create meeting: %w", err)
	}
	return m, nil
}

// fromWire reads a validated meeting object into a revision.
func fromWire(w federation.Meeting) (*revision, error) {
	start, err := rrule.ParseInstant(w.Start)
	if err != nil {
		return nil, err
	}
	end, err := rrule.ParseInstant(w.End)
	if err != nil {
		return nil, err
	}
	return &revision{
		number:      w.Revision,
		status:      w.Status,
		title:       w.Title,
		description: w.Description,
		html:        w.DescriptionHTML,
		start:       start,
		end:         end,
		timeZone:    w.TimeZone,
		allDay:      w.AllDay,
		repeat:      w.Repeat,
		overrides:   w.Overrides,
	}, nil
}

func hasAccount(ctx context.Context, q store.Queryer, address string) (bool, error) {
	n, err := store.SelectInt(ctx, q, `SELECT COUNT(*) FROM "alias" WHERE "email" = ?`, address)
	if err != nil {
		return false, fmt.Errorf("look up recipient: %w", err)
	}
	return n > 0, nil
}

// UndeliveredMeeting lists the recipients of a revision that have no account
// here, which is what its first delivery refused (RFC 5.4).
func (s *Service) UndeliveredMeeting(ctx context.Context, in federation.MeetingInbound) ([]string, error) {
	var refused []string
	for _, address := range in.Recipients {
		has, err := hasAccount(ctx, s.db, address)
		if err != nil {
			return nil, err
		}
		if !has {
			refused = append(refused, address)
		}
	}
	return refused, nil
}

// ReceiveRSVP records an answer from an attendee's own Domain (RFC 15.5). An
// answer for a meeting not held here, or from someone the held revision does
// not list yet, is refused as not_a_member: the revision that invites them
// may still be on its way. An answer from someone a later revision removed
// changes nothing.
func (s *Service) ReceiveRSVP(ctx context.Context, in federation.RSVPInbound) error {
	return s.db.Tx(ctx, func(tx *sql.Tx) error {
		gMeetingID, err := store.SelectInt(ctx, tx,
			`SELECT "id" FROM "g_meeting" WHERE "federation_id" = ? LIMIT 1`, in.MeetingFedID)
		if err != nil {
			return fmt.Errorf("look up meeting: %w", err)
		}
		if gMeetingID == 0 {
			return fmt.Errorf("meeting %s is not held here: %w", in.MeetingFedID, federation.ErrNotMember)
		}
		m, err := loadMeeting(ctx, tx, gMeetingID)
		if err != nil {
			return err
		}
		if !strings.EqualFold(m.organizer, in.OrganizerEmail) {
			return fmt.Errorf("meeting %s is organized by %s: %w", in.MeetingFedID, m.organizer,
				federation.ErrWrongOrganizer)
		}
		latest, err := latestRevision(ctx, tx, gMeetingID)
		if err != nil || latest == nil {
			return err
		}
		attendees, err := attendeesOf(ctx, tx, latest.id)
		if err != nil {
			return err
		}
		if !contains(attendees, in.AttendeeEmail) {
			if in.Revision > latest.number {
				return fmt.Errorf("%s is not invited in revision %d of %s: %w", in.AttendeeEmail, latest.number,
					in.MeetingFedID, federation.ErrNotMember)
			}
			return nil
		}
		var occurrence any
		if in.Occurrence != "" {
			occurrence = in.Occurrence
		}
		// The same answer delivered twice, through two envelopes, is one.
		dup, err := store.SelectInt(ctx, tx,
			`SELECT COUNT(*) FROM "g_meeting_rsvp" WHERE "g_meeting_id" = ? AND "email" = ? `+
				`AND COALESCE("occurrence", '') = ? AND "answered_at" = ? AND "will_attend" = ?`,
			gMeetingID, in.AttendeeEmail, in.Occurrence, in.AnsweredAt, in.Answer)
		if err != nil {
			return fmt.Errorf("look up answer: %w", err)
		}
		if dup > 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO "g_meeting_rsvp" ("g_meeting_id", "email", "will_attend", "revision", "occurrence", `+
				`"answered_at", "created_at") VALUES (?,?,?,?,?,?,?)`,
			gMeetingID, in.AttendeeEmail, in.Answer, in.Revision, occurrence, in.AnsweredAt, s.db.Now()); err != nil {
			return fmt.Errorf("record answer: %w", err)
		}
		return nil
	})
}

// MeetingUndelivered hears from the outbox that a revision did not reach
// some attendees. Those whose Domain refused them as unknown, or does not do
// calendar, are dropped by a new revision, so the other Domains stop listing
// them, and are not named as removed, since they never had the meeting (RFC
// 15.1, 15.4). The organizer is told in their inbox in every case.
func (s *Service) MeetingUndelivered(ctx context.Context, b federation.MeetingBounce) error {
	if b.GMeetingID == 0 || len(b.Recipients) == 0 {
		return nil
	}
	m, err := loadMeeting(ctx, s.db, b.GMeetingID)
	if err != nil || m == nil || s.remote(m.organizer) {
		return err
	}
	who := strings.Join(b.Recipients, ", ")
	if b.Refused || b.Unreachable {
		err := s.db.Tx(ctx, func(tx *sql.Tx) error {
			latest, err := latestRevision(ctx, tx, m.id)
			if err != nil || latest == nil {
				return err
			}
			attendees, err := attendeesOf(ctx, tx, latest.id)
			if err != nil {
				return err
			}
			gone := without(attendees, without(attendees, b.Recipients))
			if len(gone) == 0 {
				return nil
			}
			docs, err := documentsOf(ctx, tx, latest.id)
			if err != nil {
				return err
			}
			next := *latest
			return s.revise(ctx, tx, m, latest, &next, without(attendees, gone), docs, nil, nil)
		})
		if err != nil {
			return err
		}
	}
	var body string
	switch {
	case b.Refused:
		body = fmt.Sprintf("The invitation to \"%s\" could not be delivered to %s: %s does not know them "+
			"(answered %d %s). They are no longer invited.\n", b.Title, who, b.PeerDomain, b.HTTPStatus, b.ErrorCode)
	case b.Unreachable:
		body = fmt.Sprintf("%s cannot be invited to \"%s\": %s does not take meetings from other Domains. "+
			"They are no longer invited.\n", who, b.Title, b.PeerDomain)
	case b.Backfill:
		body = fmt.Sprintf("\"%s\" has not reached %s yet: %s did not answer. "+
			"It will be sent again when %s can be reached.\n", b.Title, who, b.PeerDomain, b.PeerDomain)
	default:
		body = fmt.Sprintf("\"%s\" could not be delivered to %s (%s answered %d %s).\n",
			b.Title, who, b.PeerDomain, b.HTTPStatus, b.ErrorCode)
	}
	s.notice(ctx, m.organizer, b.Title, body)
	return nil
}
