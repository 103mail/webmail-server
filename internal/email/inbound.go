package email

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/mevdschee/webmail-server/internal/store"
)

// ListDomains returns every domain this deployment is authoritative for.
func (s *Service) ListDomains(ctx context.Context) ([]string, error) {
	domains, err := store.SelectStrings(ctx, s.db, "SELECT `name` FROM `domain`")
	if err != nil {
		return nil, fmt.Errorf("list domains: %w", err)
	}
	return domains, nil
}

// IsLocalDomain reports whether an address belongs to this deployment. Inbound
// mail from a local address is our own outbound coming back, and accepting it
// would loop.
func (s *Service) IsLocalDomain(ctx context.Context, email string) (bool, error) {
	domains, err := s.ListDomains(ctx)
	if err != nil {
		return false, err
	}
	domain := strings.ToLower(DomainOf(email))
	for _, d := range domains {
		if strings.ToLower(d) == domain {
			return true, nil
		}
	}
	return false, nil
}

// DomainOf returns the part of an address after the last @, or the whole string
// when there is none.
func DomainOf(email string) string {
	i := strings.LastIndex(email, "@")
	if i <= 0 {
		return email
	}
	return email[i+1:]
}

var numericEntity = regexp.MustCompile(`&#([0-9]{2,4});?`)

// DecodeNumericEntities replaces &#NNN; escapes in an address with the
// character they denote, restricted to characters that can appear in an
// address.
//
// Senders use these to obfuscate addresses from naive filters. A genuine
// address cannot contain "&#", so this is a no-op for well-formed input. It is
// the port of EmailClient::numericEntityDecode and exists so that both backends
// derive the same address from the same message.
func DecodeNumericEntities(s string) string {
	return numericEntity.ReplaceAllStringFunc(s, func(match string) string {
		groups := numericEntity.FindStringSubmatch(match)
		code, err := strconv.Atoi(groups[1])
		if err != nil || code <= 0 || code > 0x10FFFF {
			return match
		}
		r := rune(code)
		if r > 127 {
			// Non-ASCII cannot be transliterated safely here; the PHP maps
			// anything it cannot represent to an underscore.
			return "_"
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '-' || r == '_' {
			return string(r)
		}
		return "_"
	})
}

// AddExternalThread records a message that arrived from outside as a new
// thread, with the sender and every recipient subscribed.
//
// It returns the canonical thread id, or 0 when the message was not accepted
// because no recipient has a mailbox here.
func (s *Service) AddExternalThread(ctx context.Context, senderEmail, senderName string,
	subscribers []string, subject, bodyHTML string, attachments []NewAttachment) (int64, error) {
	if senderEmail == "" {
		return 0, nil
	}
	var gThreadID int64
	var onboarded []invitee
	sender := identity{Email: senderEmail, Name: senderName}
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		gThreadID, err = store.Insert(ctx, tx,
			"INSERT INTO `g_thread` (`subject_line`) VALUES (?)", subject)
		if err != nil {
			return fmt.Errorf("create thread: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT IGNORE INTO `g_subscription` (`g_thread_id`, `email`) VALUES (?,?)",
			gThreadID, senderEmail); err != nil {
			return fmt.Errorf("subscribe sender: %w", err)
		}
		for _, email := range subscribers {
			if email == "" || email == senderEmail {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				"INSERT IGNORE INTO `g_subscription` (`g_thread_id`, `email`) VALUES (?,?)",
				gThreadID, email); err != nil {
				return fmt.Errorf("subscribe %s: %w", email, err)
			}
		}
		// Customer id 0: the sender arrived from outside and has no tenancy
		// here. That is what stops this path federating onward, which would
		// make this Server a relay signing arbitrary internet mail as its own
		// Domain. See deliver.
		_, err = s.deliver(ctx, tx, 0, gThreadID, sender, bodyHTML, attachments, &onboarded)
		return err
	})
	if err != nil {
		return 0, err
	}
	s.invite(ctx, sender, subject, onboarded)
	return gThreadID, nil
}

// SeenInbound records that a provider message id has been processed and reports
// whether it had been seen before.
//
// SNS delivers at least once, so the same message can arrive twice. The insert
// is what claims the id: two concurrent deliveries race on the primary key and
// exactly one of them wins.
func (s *Service) SeenInbound(ctx context.Context, providerID string) (bool, error) {
	if providerID == "" {
		// Without an id there is nothing to deduplicate on. Accepting is the
		// safer failure: a duplicate thread is recoverable, a dropped message
		// is not.
		return false, nil
	}
	res, err := s.db.ExecContext(ctx,
		"INSERT IGNORE INTO `inbound_email` (`provider_id`) VALUES (?)", providerID)
	if err != nil {
		return false, fmt.Errorf("record inbound message: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record inbound message: %w", err)
	}
	return affected == 0, nil
}
