package calendar

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/103mail/webmail-server/internal/federation"
	"github.com/103mail/webmail-server/internal/rrule"
	"github.com/103mail/webmail-server/internal/store"
)

// given is one answer from g_meeting_rsvp.
type given struct {
	answer   string
	revision int
	at       time.Time
	id       int64
}

func (g given) laterThan(o given) bool {
	if !g.at.Equal(o.at) {
		return g.at.After(o.at)
	}
	return g.id > o.id
}

// answers are each attendee's latest answers: for the whole meeting, and for
// each occurrence they answered on its own.
type answers map[string]*attendeeAnswers

type attendeeAnswers struct {
	whole       *given
	occurrences map[string]given
}

// effective is an attendee's answer for one occurrence: the later of their
// answer for it and their answer for the whole meeting, so that answering the
// whole meeting again also answers every occurrence (RFC 15.5).
func (a answers) effective(email, occurrence string) (given, bool) {
	mine := a[strings.ToLower(email)]
	if mine == nil {
		return given{}, false
	}
	one, hasOne := mine.occurrences[occurrence]
	switch {
	case mine.whole == nil:
		return one, hasOne
	case !hasOne || mine.whole.laterThan(one):
		return *mine.whole, true
	}
	return one, true
}

// answersOf reads the latest answers of everyone on a meeting. An earlier
// answer that arrived late is in the table and changes nothing here.
func answersOf(ctx context.Context, q store.Queryer, gMeetingID int64) (answers, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT "id", "email", "will_attend", "revision", "occurrence", "answered_at" FROM "g_meeting_rsvp" `+
			`WHERE "g_meeting_id" = ?`, gMeetingID)
	if err != nil {
		return nil, fmt.Errorf("list answers: %w", err)
	}
	defer rows.Close()
	out := answers{}
	for rows.Next() {
		var g given
		var email string
		var occurrence sql.NullString
		if err := rows.Scan(&g.id, &email, &g.answer, &g.revision, &occurrence, &g.at); err != nil {
			return nil, fmt.Errorf("scan answer: %w", err)
		}
		email = strings.ToLower(email)
		mine := out[email]
		if mine == nil {
			mine = &attendeeAnswers{occurrences: map[string]given{}}
			out[email] = mine
		}
		if !occurrence.Valid || occurrence.String == "" {
			if mine.whole == nil || g.laterThan(*mine.whole) {
				g := g
				mine.whole = &g
			}
			continue
		}
		if prev, ok := mine.occurrences[occurrence.String]; !ok || g.laterThan(prev) {
			mine.occurrences[occurrence.String] = g
		}
	}
	return out, rows.Err()
}

// SetRSVP records the customer's answer to a meeting, or to one occurrence of
// a series when occurrence names it, and sends it to every other Domain with
// an attendee (RFC 15.5). The organizer does not answer their own meeting,
// and someone the organizer removed cannot answer.
func (s *Service) SetRSVP(ctx context.Context, customerID, meetingID int64, willAttend, occurrence string) (bool, error) {
	switch willAttend {
	case "yes", "no", "maybe":
	default:
		return false, nil
	}
	var ok bool
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		ok = false
		p, err := projectionOf(ctx, tx, customerID, meetingID)
		if err != nil || p == nil {
			return err
		}
		v, err := s.viewOf(ctx, tx, p.gMeetingID, p.aliases)
		if err != nil || v == nil || v.removed || v.self == "" || strings.EqualFold(v.self, v.meeting.organizer) {
			return err
		}
		var key any
		if occurrence != "" && v.revision.repeat != nil {
			k, err := s.occurrenceKey(v.revision, occurrence)
			if err != nil {
				return err
			}
			key = k
		}
		now := s.db.Now()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO "g_meeting_rsvp" ("g_meeting_id", "email", "will_attend", "revision", "occurrence", `+
				`"answered_at", "created_at") VALUES (?,?,?,?,?,?,?)`,
			p.gMeetingID, v.self, willAttend, v.revision.number, key, now, now); err != nil {
			return fmt.Errorf("record answer: %w", err)
		}
		if err := markSeen(ctx, tx, meetingID, v.revision.number); err != nil {
			return err
		}
		ok = true
		k, _ := key.(string)
		return s.sendAnswer(ctx, tx, v.meeting, v.attendees, v.self, nil, given{
			answer: willAttend, revision: v.revision.number, at: now,
		}, k)
	})
	return ok, err
}

// occurrenceKey reads an occurrence the API names and checks that the
// meeting has it.
func (s *Service) occurrenceKey(r *revision, occurrence string) (string, error) {
	at, err := parseInstant(occurrence)
	if err != nil {
		return "", badInput("occurrence %q is not a time", occurrence)
	}
	series, err := r.series()
	if err != nil {
		return "", err
	}
	key := rrule.FormatInstant(at)
	for _, o := range series.Expand(at.Add(time.Second)) {
		if o.Key == key {
			return key, nil
		}
	}
	return "", badInput("the meeting has no occurrence at %s", key)
}

// parseInstant reads a time the API is given, in any RFC 3339 offset.
func parseInstant(v string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// sendAnswer queues one answer for the Domains of the attendees, each Domain
// with all of its attendees as recipients. With to set, it goes only to those
// addresses, which is how earlier answers reach someone just invited (RFC
// 15.4). Domains hosted here are not sent anything: they share these tables.
func (s *Service) sendAnswer(ctx context.Context, tx *sql.Tx, m *meetingRow, attendees []string, attendee string,
	to []string, g given, occurrence string) error {
	if s.fed == nil {
		return nil
	}
	origin, ok := s.fed.OriginFor(attendee)
	if !ok {
		return nil
	}
	recipients := attendees
	if to != nil {
		recipients = to
	}
	byDomain := s.remoteByDomain(recipients)
	for _, domain := range sortedKeys(byDomain) {
		for _, chunk := range chunks(byDomain[domain], 100) {
			if err := s.fed.EnqueueRSVP(ctx, tx, federation.RSVPOutbound{
				PeerDomain:     domain,
				OriginDomain:   origin.Domain,
				MeetingFedID:   m.fedID,
				OrganizerEmail: m.organizer,
				AttendeeEmail:  attendee,
				Answer:         g.answer,
				Revision:       g.revision,
				AnsweredAt:     g.at,
				Occurrence:     occurrence,
				Recipients:     chunk,
				RelatedID:      m.id,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// sendAnswersTo sends the people just invited the latest answers of this
// Server's own attendees, from each attendee's own Domain. Every Domain that
// holds the meeting does the same for its users, so between them the
// newcomers see every answer given so far (RFC 15.4).
func (s *Service) sendAnswersTo(ctx context.Context, tx *sql.Tx, m *meetingRow, attendees, added []string) error {
	if s.fed == nil || len(s.remoteByDomain(added)) == 0 {
		return nil
	}
	all, err := answersOf(ctx, tx, m.id)
	if err != nil {
		return err
	}
	for _, attendee := range attendees {
		mine := all[strings.ToLower(attendee)]
		if mine == nil || s.remote(attendee) || strings.EqualFold(attendee, m.organizer) || contains(added, attendee) {
			continue
		}
		if mine.whole != nil {
			if err := s.sendAnswer(ctx, tx, m, attendees, attendee, added, *mine.whole, ""); err != nil {
				return err
			}
		}
		for _, occurrence := range sortedKeys(mine.occurrences) {
			if err := s.sendAnswer(ctx, tx, m, attendees, attendee, added, mine.occurrences[occurrence], occurrence); err != nil {
				return err
			}
		}
	}
	return nil
}

// projection is a customer's meeting row and who they are in it.
type projection struct {
	id, gMeetingID, mailboxID int64
	aliases                   []string
}

func projectionOf(ctx context.Context, q store.Queryer, customerID, meetingID int64) (*projection, error) {
	p := projection{id: meetingID}
	err := q.QueryRowContext(ctx,
		`SELECT "g_meeting_id", "mailbox_id" FROM "meeting" WHERE "customer_id" = ? AND "id" = ? LIMIT 1`,
		customerID, meetingID).Scan(&p.gMeetingID, &p.mailboxID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look up meeting: %w", err)
	}
	if p.aliases, err = aliasesOf(ctx, q, p.mailboxID); err != nil {
		return nil, err
	}
	return &p, nil
}

func markSeen(ctx context.Context, tx *sql.Tx, meetingID int64, number int) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE "meeting" SET "seen_revision" = ? WHERE "id" = ? AND "seen_revision" < ?`,
		number, meetingID, number); err != nil {
		return fmt.Errorf("mark meeting seen: %w", err)
	}
	return nil
}

func stringArgs(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}
