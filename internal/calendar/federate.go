package calendar

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/103mail/webmail-server/internal/federation"
)

// Federator is what the calendar domain needs of the federation service to
// reach attendees on other Domains (RFC 15). The types are the federation
// package's, which owns the wire, so the dependency runs one way.
type Federator interface {
	// IsLocal reports whether a DNS domain is one this Server hosts.
	IsLocal(domain string) bool
	// OriginFor returns the local Domain an address sends as.
	OriginFor(address string) (*federation.LocalDomain, bool)
	// Calendar reports whether a Domain implements the calendar extension.
	Calendar(ctx context.Context, domain string) (bool, error)
	// EnqueueMeeting and EnqueueRSVP write an envelope to the outbox in the
	// caller's transaction.
	EnqueueMeeting(ctx context.Context, tx *sql.Tx, out federation.MeetingOutbound) error
	EnqueueRSVP(ctx context.Context, tx *sql.Tx, out federation.RSVPOutbound) error
}

// Federate lets meetings reach attendees on other Domains. Without it every
// meeting stays on this Server.
func (s *Service) Federate(f Federator) { s.fed = f }

// remote reports whether an address lives on another Domain.
func (s *Service) remote(address string) bool {
	return s.fed != nil && !s.fed.IsLocal(federation.DomainOf(address))
}

// remoteByDomain groups the addresses that live elsewhere by their Domain,
// which is who each envelope goes to.
func (s *Service) remoteByDomain(addresses []string) map[string][]string {
	out := map[string][]string{}
	for _, a := range addresses {
		if s.remote(a) && !federation.IsMailerDaemon(a) {
			domain := federation.DomainOf(a)
			if !contains(out[domain], a) {
				out[domain] = append(out[domain], a)
			}
		}
	}
	return out
}

// discoveryTimeout bounds how long inviting someone waits on their Domain's
// descriptor. It is ours, not the RFC's.
const discoveryTimeout = 5 * time.Second

// reachable splits invitees into those a meeting can reach and those it
// cannot: addresses on a Domain that does not advertise calendar, and
// addresses on no Webmail Domain at all, which this version cannot invite
// without putting the meeting in an email (RFC 15.1, 15.8). It is called
// before the transaction, because it may wait on the network.
func (s *Service) reachable(ctx context.Context, addresses []string) ([]string, []string) {
	if s.fed == nil {
		return addresses, nil
	}
	known := map[string]bool{}
	var ok, unreachable []string
	for _, a := range addresses {
		if !s.remote(a) {
			ok = append(ok, a)
			continue
		}
		domain := federation.DomainOf(a)
		calendar, seen := known[domain]
		if !seen {
			check, cancel := context.WithTimeout(ctx, discoveryTimeout)
			has, err := s.fed.Calendar(check, domain)
			cancel()
			calendar = err == nil && has
			known[domain] = calendar
		}
		if calendar {
			ok = append(ok, a)
		} else {
			unreachable = append(unreachable, a)
		}
	}
	return ok, unreachable
}

// publish queues a revision written here for the Domain of every attendee and
// of everyone it removes, each Domain with only its own addresses as
// recipients, in envelopes of at most 100 (RFC 15.2, 15.3). The Domains of
// people it adds get this Server's own answers too (RFC 15.4).
func (s *Service) publish(ctx context.Context, tx *sql.Tx, m *meetingRow, r *revision,
	attendees, removed, added []string) error {
	if s.fed == nil {
		return nil
	}
	byDomain := s.remoteByDomain(append(append([]string{}, attendees...), removed...))
	if len(byDomain) > 0 {
		origin, ok := s.fed.OriginFor(m.organizer)
		if !ok {
			return fmt.Errorf("cannot federate a meeting organized by %s: it is not a Domain hosted here", m.organizer)
		}
		docs, err := documentsOf(ctx, tx, r.id)
		if err != nil {
			return err
		}
		attachments := make([]federation.OutboundAttachment, 0, len(docs))
		for _, d := range docs {
			data, err := s.blob.Get(d.md5)
			if err != nil {
				return err
			}
			attachments = append(attachments, federation.OutboundAttachment{
				FederationID: d.fedID, FileName: d.name, MimeType: d.mime, Data: data,
			})
		}
		wire := toWire(m, r, attendees, removed)
		for _, domain := range sortedKeys(byDomain) {
			for _, chunk := range chunks(byDomain[domain], 100) {
				if err := s.fed.EnqueueMeeting(ctx, tx, federation.MeetingOutbound{
					PeerDomain:   domain,
					OriginDomain: origin.Domain,
					Meeting:      wire,
					Attachments:  attachments,
					Recipients:   chunk,
					RelatedID:    m.id,
				}); err != nil {
					return err
				}
			}
		}
	}
	return s.sendAnswersTo(ctx, tx, m, attendees, added)
}

// toWire is a revision as the meeting object of its envelope.
func toWire(m *meetingRow, r *revision, attendees, removed []string) federation.Meeting {
	return federation.Meeting{
		FederationID:    m.fedID,
		Revision:        r.number,
		Status:          r.status,
		Organizer:       federation.Organizer{Email: m.organizer, Name: m.organizerName},
		Attendees:       attendees,
		Removed:         removed,
		Title:           r.title,
		Description:     r.description,
		DescriptionHTML: r.html,
		Start:           federation.FormatTimestamp(r.start),
		End:             federation.FormatTimestamp(r.end),
		TimeZone:        r.timeZone,
		AllDay:          r.allDay,
		Repeat:          r.repeat,
		Overrides:       r.overrides,
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func chunks(list []string, size int) [][]string {
	var out [][]string
	for len(list) > size {
		out = append(out, list[:size])
		list = list[size:]
	}
	if len(list) > 0 {
		out = append(out, list)
	}
	return out
}
