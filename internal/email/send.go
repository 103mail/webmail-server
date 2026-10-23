package email

import (
	"context"
	crand "crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/103mail/webmail-server/internal/federation"
	"github.com/103mail/webmail-server/internal/mdconv"
	"github.com/103mail/webmail-server/internal/store"
)

// AddThread starts a new conversation in a folder and delivers the first
// message to every subscriber. It returns the sender's thread projection id.
func (s *Service) AddThread(ctx context.Context, customerID, folderID int64,
	subscribers []string, subject, bodyHTML string, attachments []NewAttachment) (int64, error) {
	if err := s.checkAttachments(attachments); err != nil {
		return 0, err
	}
	var threadID int64
	for i := range subscribers {
		subscribers[i] = store.Address(subscribers[i])
	}
	var onboarded []invitee
	var sender identity
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		mailboxID, err := store.SelectInt(ctx, tx,
			`SELECT "mailbox_id" FROM "folder" WHERE "customer_id" = ? AND "id" = ? LIMIT 1`,
			customerID, folderID)
		if err != nil {
			return fmt.Errorf("look up folder: %w", err)
		}
		if mailboxID == 0 {
			return nil
		}
		sender, err = senderIdentity(ctx, tx, customerID, mailboxID)
		if err != nil {
			return err
		}
		if sender.Email == "" {
			return nil
		}
		if err := s.refuseUnknown(ctx, tx, subscribers); err != nil {
			return err
		}

		gThreadID, err := store.Insert(ctx, tx,
			`INSERT INTO "g_thread" ("subject_line") VALUES (?)`, subject)
		if err != nil {
			return fmt.Errorf("create thread: %w", err)
		}
		threadID, err = store.Insert(ctx, tx,
			`INSERT INTO "thread" ("customer_id", "folder_id", "g_thread_id") VALUES (?,?,?)`,
			customerID, folderID, gThreadID)
		if err != nil {
			return fmt.Errorf("create thread projection: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO "g_subscription" ("g_thread_id", "email") VALUES (?,?) ON CONFLICT DO NOTHING`,
			gThreadID, sender.Email); err != nil {
			return fmt.Errorf("subscribe sender: %w", err)
		}
		for _, email := range subscribers {
			if email == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO "g_subscription" ("g_thread_id", "email") VALUES (?,?) ON CONFLICT DO NOTHING`,
				gThreadID, email); err != nil {
				return fmt.Errorf("subscribe %s: %w", email, err)
			}
		}
		_, err = s.deliver(ctx, tx, customerID, gThreadID, sender, bodyHTML, attachments, &onboarded, nil)
		return err
	})
	if err != nil {
		return 0, err
	}
	s.invite(ctx, sender, subject, onboarded)
	return threadID, nil
}

// AddMessage appends a reply to a thread and delivers it to every subscriber.
// It returns the sender's message projection id.
func (s *Service) AddMessage(ctx context.Context, customerID, threadID int64,
	bodyHTML string, attachments []NewAttachment) (int64, error) {
	if err := s.checkAttachments(attachments); err != nil {
		return 0, err
	}
	var messageID int64
	var onboarded []invitee
	var sender identity
	var subject string
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		var gThreadID, folderID int64
		err := tx.QueryRowContext(ctx,
			`SELECT "g_thread_id", "folder_id" FROM "thread" WHERE "customer_id" = ? AND "id" = ? LIMIT 1`,
			customerID, threadID).Scan(&gThreadID, &folderID)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return fmt.Errorf("look up thread: %w", err)
		}
		subject, err = store.SelectString(ctx, tx,
			`SELECT "subject_line" FROM "g_thread" WHERE "id" = ? LIMIT 1`, gThreadID)
		if err != nil {
			return fmt.Errorf("look up subject: %w", err)
		}
		mailboxID, err := store.SelectInt(ctx, tx,
			`SELECT "mailbox_id" FROM "folder" WHERE "customer_id" = ? AND "id" = ? LIMIT 1`,
			customerID, folderID)
		if err != nil {
			return fmt.Errorf("look up folder: %w", err)
		}
		sender, err = senderIdentity(ctx, tx, customerID, mailboxID)
		if err != nil {
			return err
		}
		if sender.Email == "" {
			return nil
		}
		messageID, err = s.deliver(ctx, tx, customerID, gThreadID, sender, bodyHTML, attachments, &onboarded, nil)
		return err
	})
	if err != nil {
		return 0, err
	}
	s.invite(ctx, sender, subject, onboarded)
	return messageID, nil
}

// AddSubscriber adds an address to a thread and posts a system message saying
// so. Adding someone is the only way visibility grows: there is no forwarding.
func (s *Service) AddSubscriber(ctx context.Context, customerID, threadID int64, email string) (int64, error) {
	email = store.Address(email)
	var messageID int64
	var onboarded []invitee
	var sender identity
	var subject string
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		var gThreadID, folderID int64
		err := tx.QueryRowContext(ctx,
			`SELECT "g_thread_id", "folder_id" FROM "thread" WHERE "customer_id" = ? AND "id" = ? LIMIT 1`,
			customerID, threadID).Scan(&gThreadID, &folderID)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return fmt.Errorf("look up thread: %w", err)
		}
		subject, err = store.SelectString(ctx, tx,
			`SELECT "subject_line" FROM "g_thread" WHERE "id" = ? LIMIT 1`, gThreadID)
		if err != nil {
			return fmt.Errorf("look up subject: %w", err)
		}
		mailboxID, err := store.SelectInt(ctx, tx,
			`SELECT "mailbox_id" FROM "folder" WHERE "customer_id" = ? AND "id" = ? LIMIT 1`,
			customerID, folderID)
		if err != nil {
			return fmt.Errorf("look up folder: %w", err)
		}
		sender, err = senderIdentity(ctx, tx, customerID, mailboxID)
		if err != nil {
			return err
		}
		if sender.Email == "" {
			return nil
		}
		if err := s.refuseUnknown(ctx, tx, []string{email}); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO "g_subscription" ("g_thread_id", "email") VALUES (?,?) ON CONFLICT DO NOTHING`,
			gThreadID, email); err != nil {
			return fmt.Errorf("add subscriber: %w", err)
		}
		// Inviting someone who left or was refused makes them a member again.
		if _, err := tx.ExecContext(ctx,
			`UPDATE "g_subscription" SET "left_at" = NULL, "refused_at" = NULL WHERE "g_thread_id" = ? AND "email" = ?`,
			gThreadID, email); err != nil {
			return fmt.Errorf("add subscriber: %w", err)
		}
		bodyHTML := fmt.Sprintf(
			"<html><body><p><i>%s has added %s to this thread's subscribers</i></p></body></html>",
			sender.Email, email)
		messageID, err = s.deliver(ctx, tx, customerID, gThreadID, sender, bodyHTML, nil, &onboarded, nil)
		return err
	})
	if err != nil {
		return 0, err
	}
	s.invite(ctx, sender, subject, onboarded)
	return messageID, nil
}

// identity is the alias a message is sent as.
type identity struct {
	Email string
	Name  string
}

// invitee is an address that had no account until this delivery created one.
// The invitation is sent after the transaction commits, never inside it: a
// publish inside a transaction would hold locks across a network round trip
// and could not be rolled back if the transaction failed.
type invitee struct {
	Email      string
	CustomerID int64
	UserID     int64
	Token      string
	// Domain is the hosted Domain the new account lives on, which is where
	// the invitation links to. It is not the domain of Email.
	Domain string
}

// senderIdentity returns the default alias of a mailbox.
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
		return identity{}, fmt.Errorf("look up sender alias: %w", err)
	}
	return id, nil
}

// deliver writes the canonical message and fans out a projection to every
// subscriber. It returns the sender's own message projection id.
//
// The sender must already be subscribed; this is what stops a thread from being
// used to inject a message on someone else's behalf.
//
// left names members leaving with this message: the leaver may send it, and
// is told to the thread in thread.left rather than as a member (RFC 5.6).
func (s *Service) deliver(ctx context.Context, tx *sql.Tx, senderCustomerID, gThreadID int64,
	sender identity, bodyHTML string, attachments []NewAttachment, onboarded *[]invitee,
	left []string) (int64, error) {
	subscribers, err := store.SelectStrings(ctx, tx,
		`SELECT "email" FROM "g_subscription" WHERE "g_thread_id" = ? AND `+isMember, gThreadID)
	if err != nil {
		return 0, fmt.Errorf("list subscribers: %w", err)
	}
	if !contains(subscribers, sender.Email) && !contains(left, sender.Email) {
		return 0, &NotMemberError{Address: sender.Email}
	}

	body := mdconv.ToMarkdown(bodyHTML)
	summary := mdconv.Summary(body)
	now := s.db.Now()
	if _, err := tx.ExecContext(ctx,
		`UPDATE "g_thread" SET "body_summary" = ?, "updated_at" = ? WHERE "id" = ?`,
		summary, now, gThreadID); err != nil {
		return 0, fmt.Errorf("update thread summary: %w", err)
	}

	// The federation id is assigned now, with the send time, because together
	// they order the thread on every Domain (RFC 5.5).
	messageFedID, err := federation.NewUUID()
	if err != nil {
		return 0, err
	}
	gMessageID, err := store.Insert(ctx, tx,
		`INSERT INTO "g_message" ("g_thread_id", "sender_email", "sender_name", "body", "body_html", `+
			`"created_at", "sent_at", "federation_id") VALUES (?,?,?,?,?,?,?,?)`,
		gThreadID, sender.Email, sender.Name, body, bodyHTML, now, now, messageFedID)
	if err != nil {
		return 0, fmt.Errorf("create message: %w", err)
	}

	gAttachmentIDs, err := s.storeAttachments(ctx, tx, gMessageID, attachments)
	if err != nil {
		return 0, err
	}

	// Federation splits the fan-out in two (ADR 17). A subscriber on a Domain
	// this Server hosts is delivered the way it always was: a projection row
	// written here, inside this transaction. A subscriber on another Domain is
	// not onboarded locally, because that would hand them an address here that
	// is not theirs; their envelope is queued instead, in this same
	// transaction, and a worker carries it across.
	remote := map[string][]string{}
	var messageID int64
	deliveredTo := map[int64]bool{}
	for _, email := range subscribers {
		if email != sender.Email && !s.isLocal(email) {
			domain := federation.DomainOf(email)
			remote[domain] = append(remote[domain], email)
			// The sender still sees them as a contact, so autocomplete works,
			// but no local mailbox is created for them (design 8).
			if senderCustomerID != 0 {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO "contact" ("customer_id", "email", "name") VALUES (?,?,?) ON CONFLICT DO NOTHING`,
					senderCustomerID, email, ""); err != nil {
					return 0, fmt.Errorf("record remote subscriber as contact: %w", err)
				}
			}
			continue
		}
		if email != sender.Email {
			// An address on one of our own Domains either has an account or
			// was refused when the message was written. Only someone outside
			// them is onboarded, which happens with federation off: that is
			// how this Server reaches people elsewhere until it is a
			// Registrar (RFC 8).
			own, err := s.ownDomain(ctx, tx, email)
			if err != nil {
				return 0, err
			}
			if !own {
				if err := s.ensureCustomer(ctx, tx, email, "", onboarded); err != nil {
					return 0, err
				}
			}
		}
		mailboxID, err := store.SelectInt(ctx, tx,
			`SELECT "mailbox_id" FROM "alias" WHERE "email" = ? LIMIT 1`, email)
		if err != nil {
			return 0, fmt.Errorf("look up subscriber mailbox: %w", err)
		}
		// An address with no mailbox has not completed onboarding, and a
		// mailbox already delivered to must not receive the message twice.
		if mailboxID == 0 || deliveredTo[mailboxID] {
			continue
		}
		deliveredTo[mailboxID] = true

		folderID, err := store.SelectInt(ctx, tx,
			`SELECT "default_folder_id" FROM "mailbox" WHERE "id" = ? LIMIT 1`, mailboxID)
		if err != nil {
			return 0, fmt.Errorf("look up subscriber folder: %w", err)
		}
		subCustomerID, err := store.SelectInt(ctx, tx,
			`SELECT "customer_id" FROM "folder" WHERE "id" = ? LIMIT 1`, folderID)
		if err != nil {
			return 0, fmt.Errorf("look up subscriber customer: %w", err)
		}
		if folderID == 0 || subCustomerID == 0 {
			continue
		}

		threadID, err := store.SelectInt(ctx, tx,
			`SELECT "id" FROM "thread" WHERE "customer_id" = ? AND "g_thread_id" = ? LIMIT 1`,
			subCustomerID, gThreadID)
		if err != nil {
			return 0, fmt.Errorf("look up subscriber thread: %w", err)
		}
		if threadID == 0 {
			threadID, err = store.Insert(ctx, tx,
				`INSERT INTO "thread" ("customer_id", "folder_id", "g_thread_id") VALUES (?,?,?)`,
				subCustomerID, folderID, gThreadID)
			if err != nil {
				return 0, fmt.Errorf("create subscriber thread: %w", err)
			}
			if err := s.projectHistory(ctx, tx, subCustomerID, threadID, gThreadID, gMessageID); err != nil {
				return 0, err
			}
		}
		projectionID, err := store.Insert(ctx, tx,
			`INSERT INTO "message" ("customer_id", "thread_id", "g_message_id") VALUES (?,?,?)`,
			subCustomerID, threadID, gMessageID)
		if err != nil {
			return 0, fmt.Errorf("create subscriber message: %w", err)
		}

		if email == sender.Email {
			if err := s.MarkThreadRead(ctx, tx, subCustomerID, threadID); err != nil {
				return 0, err
			}
			messageID = projectionID
		} else if _, err := tx.ExecContext(ctx,
			`UPDATE "thread" SET "read_at" = NULL WHERE "id" = ?`, threadID); err != nil {
			return 0, fmt.Errorf("mark subscriber thread unread: %w", err)
		}

		for _, gAttachmentID := range gAttachmentIDs {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO "attachment" ("customer_id", "message_id", "g_attachment_id") VALUES (?,?,?)`,
				subCustomerID, projectionID, gAttachmentID); err != nil {
				return 0, fmt.Errorf("create subscriber attachment: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO "contact" ("customer_id", "email", "name") VALUES (?,?,?) ON CONFLICT DO NOTHING`,
			subCustomerID, sender.Email, sender.Name); err != nil {
			return 0, fmt.Errorf("record sender as contact: %w", err)
		}
		// And the sender sees the recipient as a contact, as for a remote
		// subscriber above, so writing to someone is enough to find them again.
		if email != sender.Email && senderCustomerID != 0 {
			name, err := store.SelectString(ctx, tx,
				`SELECT "name" FROM "alias" WHERE "email" = ? LIMIT 1`, email)
			if err != nil {
				return 0, fmt.Errorf("look up subscriber name: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO "contact" ("customer_id", "email", "name") VALUES (?,?,?) ON CONFLICT DO NOTHING`,
				senderCustomerID, email, name); err != nil {
				return 0, fmt.Errorf("record local subscriber as contact: %w", err)
			}
		}
	}

	// senderCustomerID is zero only on the path that accepts mail from outside
	// the system, where the sender has no tenancy here and therefore no Domain
	// to sign as. Such a message is delivered to its local recipients and goes
	// no further: relaying it on would mean signing someone else's content as
	// this Domain.
	if senderCustomerID != 0 {
		if err := s.federate(ctx, tx, gThreadID, gMessageID, now, sender, subscribers, nil, left, remote,
			body, bodyHTML, attachments); err != nil {
			return 0, err
		}
	} else if len(remote) > 0 {
		s.log.Info("not relaying externally received mail to another Domain",
			"sender", sender.Email, "domains", len(remote))
	}
	return messageID, nil
}

// storeAttachments writes attachment bytes to the blob store and records their
// canonical metadata.
func (s *Service) storeAttachments(ctx context.Context, tx *sql.Tx, gMessageID int64,
	attachments []NewAttachment) ([]int64, error) {
	var ids []int64
	for _, a := range attachments {
		hash, err := s.blob.Put(a.FileData)
		if err != nil {
			return nil, err
		}
		id, err := store.Insert(ctx, tx,
			`INSERT INTO "g_attachment" ("g_message_id", "file_name", "mime_type", "file_size", "hash_md5") `+
				"VALUES (?,?,?,?,?)",
			gMessageID, a.FileName, a.MimeType, len(a.FileData), hash)
		if err != nil {
			return nil, fmt.Errorf("create attachment: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// ensureCustomer onboards an unknown address, creating a customer, a user
// without a password and a mailbox.
func (s *Service) ensureCustomer(ctx context.Context, tx *sql.Tx, email, name string,
	onboarded *[]invitee) error {
	existing, err := store.SelectInt(ctx, tx,
		`SELECT "customer_id" FROM "alias" WHERE "email" = ? LIMIT 1`, email)
	if err != nil {
		return fmt.Errorf("look up alias: %w", err)
	}
	if existing != 0 {
		return nil
	}
	customerID, err := store.Insert(ctx, tx,
		`INSERT INTO "customer" ("name") VALUES (?)`, name)
	if err != nil {
		return fmt.Errorf("create customer: %w", err)
	}
	userID, err := store.Insert(ctx, tx,
		`INSERT INTO "user" ("customer_id", "username", "password", "name") VALUES (?,?,?,?)`,
		customerID, email, "", name)
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	if _, err := addMailboxTx(ctx, tx, customerID, "", "red", email, name); err != nil {
		return err
	}
	domain, err := s.DefaultDomain(ctx, tx)
	if err != nil {
		return err
	}

	// The invitation link is the only way in for someone with no password, so
	// it gets a single-use, time-bound token per ADR 6.
	token, err := newInviteToken()
	if err != nil {
		return err
	}
	if _, err := store.Insert(ctx, tx,
		`INSERT INTO "token" ("customer_id", "user_id", "name", "value", "valid_until") `+
			"VALUES (?,?,?,?,?)",
		customerID, userID, "Invite", token, s.db.Now().AddDate(0, 0, inviteValidDays)); err != nil {
		return fmt.Errorf("create invite token: %w", err)
	}
	if onboarded != nil {
		*onboarded = append(*onboarded, invitee{
			Email: email, CustomerID: customerID, UserID: userID, Token: token, Domain: domain,
		})
	}
	return nil
}

// inviteLink is the way in an invitation carries: the Federation hostname (RFC
// 1.3) of the Domain the new account lives on. The invitee's own address is on
// some other domain, which is why they were onboarded. The URL holds nothing
// but the token (ADR 6).
func inviteLink(i invitee) string {
	return federation.BaseURL(i.Domain) + "/invite/" + i.Token
}

// inviteValidDays is how long an onboarding link stays usable.
const inviteValidDays = 30

// InviteTemplate is the email_template row used to invite an external
// recipient. It carries no message content: only who wrote, and the link.
const InviteTemplate = "invite"

func newInviteToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := crand.Read(buf); err != nil {
		return "", fmt.Errorf("generate invite token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// invite mails the people this delivery onboarded. It runs after the
// transaction has committed, so a mail failure can never undo a delivery: the
// message is already in the recipient's mailbox either way, and the invitation
// is what tells them to come and read it.
func (s *Service) invite(ctx context.Context, sender identity, subject string, invitees []invitee) {
	if s.notifier == nil || len(invitees) == 0 {
		return
	}
	for _, i := range invitees {
		customerID := i.CustomerID
		err := s.notifier.Send(ctx, &customerID, "", i.Email, InviteTemplate, "en",
			map[string]string{
				"senderName":  sender.Name,
				"senderEmail": sender.Email,
				"subject":     subject,
				"token":       i.Token,
				"link":        inviteLink(i),
			}, "")
		if err != nil {
			// An invitation that did not go out is a person who never learns
			// they have mail, so it has to be visible.
			s.log.Error("invitation not sent", "email", i.Email, "error", err)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// UnknownRecipientsError refuses a message to addresses on this Server's own
// Domains that have no account. Nothing is written when it is returned.
type UnknownRecipientsError struct {
	Addresses []string
}

func (e *UnknownRecipientsError) Error() string {
	return "no account for " + strings.Join(e.Addresses, ", ")
}

// refuseUnknown returns an UnknownRecipientsError naming every address on one
// of this Server's own Domains that has no account. Such an address is never
// onboarded: on our own Domain there is nobody else to invite, so the sender
// is told now rather than by a bounce later.
func (s *Service) refuseUnknown(ctx context.Context, tx *sql.Tx, addresses []string) error {
	var unknown []string
	for _, address := range addresses {
		if address == "" || slices.Contains(unknown, address) {
			continue
		}
		own, err := s.ownDomain(ctx, tx, address)
		if err != nil {
			return err
		}
		if !own {
			continue
		}
		known, err := store.SelectInt(ctx, tx,
			`SELECT COUNT(*) FROM "alias" WHERE "email" = ?`, address)
		if err != nil {
			return fmt.Errorf("look up %s: %w", address, err)
		}
		if known == 0 {
			unknown = append(unknown, address)
		}
	}
	if len(unknown) > 0 {
		return &UnknownRecipientsError{Addresses: unknown}
	}
	return nil
}

// ownDomain reports whether an address is on a Domain this Server hosts. With
// federation those are the configured Domains. Without it the Server knows no
// peers, so every domain row is one of its own, as before federation.
func (s *Service) ownDomain(ctx context.Context, q store.Queryer, address string) (bool, error) {
	domain := federation.DomainOf(address)
	if s.federation != nil {
		return s.federation.IsLocal(domain), nil
	}
	n, err := store.SelectInt(ctx, q, `SELECT COUNT(*) FROM "domain" WHERE "name" = ?`, domain)
	if err != nil {
		return false, fmt.Errorf("look up domain %s: %w", domain, err)
	}
	return n > 0, nil
}

// NotMemberError refuses a message from someone who is not on the thread,
// for example because they left it.
type NotMemberError struct {
	Address string
}

func (e *NotMemberError) Error() string { return e.Address + " is not a member of this thread" }

// Is lets the federation layer recognise the refusal without importing this
// package, and answer a peer with not_a_member.
func (e *NotMemberError) Is(target error) bool { return target == federation.ErrNotMember }

// OnlyYourselfError refuses removing someone else from a thread: only a
// member can leave, nobody can be removed (RFC 5.6).
type OnlyYourselfError struct {
	Address string
}

func (e *OnlyYourselfError) Error() string { return "only " + e.Address + " can remove themselves" }

// Leave takes one of the customer's own addresses off a thread. The thread is
// told the way an invitation is told: a message from the leaver, sent to
// every other member and to their Domains with the address in thread.left.
// The leaver keeps the history they had and gets nothing after it.
func (s *Service) Leave(ctx context.Context, customerID, threadID int64, email string) (bool, error) {
	email = store.Address(email)
	var onboarded []invitee
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		var name string
		err := tx.QueryRowContext(ctx,
			`SELECT "name" FROM "alias" WHERE "customer_id" = ? AND "email" = ? LIMIT 1`,
			customerID, email).Scan(&name)
		if err == sql.ErrNoRows {
			return &OnlyYourselfError{Address: email}
		}
		if err != nil {
			return fmt.Errorf("look up leaver: %w", err)
		}
		gThreadID, err := store.SelectInt(ctx, tx,
			`SELECT "g_thread_id" FROM "thread" WHERE "customer_id" = ? AND "id" = ? LIMIT 1`,
			customerID, threadID)
		if err != nil {
			return fmt.Errorf("look up thread: %w", err)
		}
		member, err := store.SelectInt(ctx, tx,
			`SELECT COUNT(*) FROM "g_subscription" WHERE "g_thread_id" = ? AND "email" = ? AND `+isMember,
			gThreadID, email)
		if err != nil {
			return fmt.Errorf("look up membership: %w", err)
		}
		if gThreadID == 0 || member == 0 {
			return &NotMemberError{Address: email}
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE "g_subscription" SET "left_at" = ? WHERE "g_thread_id" = ? AND "email" = ?`,
			s.db.Now(), gThreadID, email); err != nil {
			return fmt.Errorf("leave thread: %w", err)
		}
		bodyHTML := fmt.Sprintf("<html><body><p><i>%s left this thread</i></p></body></html>", email)
		_, err = s.deliver(ctx, tx, customerID, gThreadID, identity{Email: email, Name: name},
			bodyHTML, nil, &onboarded, []string{email})
		return err
	})
	return err == nil, err
}

// projectHistory gives a member who has just been given the thread on this
// Server its earlier messages, so an invitation brings the history (RFC 5.6).
// except is the message being delivered, which the caller projects.
func (s *Service) projectHistory(ctx context.Context, tx *sql.Tx, customerID, threadID, gThreadID,
	except int64) error {
	earlier, err := store.SelectInts(ctx, tx,
		`SELECT "id" FROM "g_message" WHERE "g_thread_id" = ? AND "id" <> ? ORDER BY "id"`, gThreadID, except)
	if err != nil {
		return fmt.Errorf("list earlier messages: %w", err)
	}
	for _, gMessageID := range earlier {
		projectionID, err := store.Insert(ctx, tx,
			`INSERT INTO "message" ("customer_id", "thread_id", "g_message_id") VALUES (?,?,?)`,
			customerID, threadID, gMessageID)
		if err != nil {
			return fmt.Errorf("project an earlier message: %w", err)
		}
		attachments, err := store.SelectInts(ctx, tx,
			`SELECT "id" FROM "g_attachment" WHERE "g_message_id" = ?`, gMessageID)
		if err != nil {
			return fmt.Errorf("list earlier attachments: %w", err)
		}
		for _, gAttachmentID := range attachments {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO "attachment" ("customer_id", "message_id", "g_attachment_id") VALUES (?,?,?)`,
				customerID, projectionID, gAttachmentID); err != nil {
				return fmt.Errorf("project an earlier attachment: %w", err)
			}
		}
	}
	return nil
}
