// Package email implements the mail domain: mailboxes, folders, threads,
// subscribers, messages, attachments and contacts. It is the Go port of
// MintyPHP's EmailClient and carries the same projection invariants: canonical
// content lives in the g_* tables and every user sees it through their own
// projection rows.
package email

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"strings"
	"time"

	crand "crypto/rand"

	"github.com/mevdschee/webmail-server/internal/blob"
	"github.com/mevdschee/webmail-server/internal/federation"
	"github.com/mevdschee/webmail-server/internal/store"
)

// Notifier sends one notification email. It is what *notify.Service satisfies,
// declared here so the mail domain does not depend on the outbound package.
//
// It takes a template name and parameters, never a body: per ADR 4 no message
// content may leave by email, and this signature is what makes that impossible
// to get wrong from here.
type Notifier interface {
	Send(ctx context.Context, customerID *int64, toName, toEmail, templateName,
		preferredLanguage string, params map[string]string, bccEmail string) error
}

// Federator decides which recipients are reachable locally and queues the rest
// for delivery to their own Domain. It is what *federation.Service satisfies.
//
// Declared here for the same reason Notifier is: the mail domain states what it
// needs rather than depending on a concrete implementation. The types it
// exchanges belong to the federation package, which owns the wire, so the
// dependency runs one way and there is no cycle.
type Federator interface {
	// IsLocal reports whether a DNS domain is one this Server is authoritative
	// for. With federation switched off it answers true for everything, which
	// is exactly the single-domain behaviour this replaces.
	IsLocal(domain string) bool
	// OriginFor returns the local Domain an address sends as, and false if the
	// address is not on a Domain hosted here.
	OriginFor(senderEmail string) (*federation.LocalDomain, bool)
	// Enqueue writes an envelope to the outbox in the caller's transaction.
	Enqueue(ctx context.Context, tx *sql.Tx, out federation.Outbound) error
	// MaxAttachmentBytes is the largest attachment the Domain accepts, which
	// is also the largest its own people may send (RFC 4.2, 7).
	MaxAttachmentBytes() int64
}

// Service implements the mail domain against a database and a blob store.
type Service struct {
	db   *store.DB
	blob *blob.Store
	// notifier is nil when outbound mail is not configured, which disables
	// onboarding invitations rather than failing delivery.
	notifier Notifier
	// federation is nil when no Domain is configured, in which case every
	// address is delivered locally and unknown recipients are onboarded, which
	// is what this backend did before federation existed.
	federation Federator
	log        *slog.Logger
}

// New returns a Service. attachments is the directory holding attachment bytes.
// notifier and federator may be nil.
func New(db *store.DB, attachments *blob.Store, notifier Notifier, federator Federator,
	log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Service{db: db, blob: attachments, notifier: notifier, federation: federator, log: log}
}

// maxAttachmentBytes is the largest file a message may carry: the Domain's
// advertised cap, or the RFC's default without federation.
func (s *Service) maxAttachmentBytes() int64 {
	if s.federation == nil {
		return federation.DefaultMaxAttachmentBytes
	}
	return s.federation.MaxAttachmentBytes()
}

// checkAttachments refuses a message with a file over the limit before
// anything is written. A peer would refuse it anyway, and a message that
// reaches some members but not others is worse than one that is refused.
func (s *Service) checkAttachments(attachments []NewAttachment) error {
	limit := s.maxAttachmentBytes()
	for _, a := range attachments {
		if int64(len(a.FileData)) > limit {
			return &TooLargeError{FileName: a.FileName, Limit: limit}
		}
	}
	return nil
}

// TooLargeError refuses an attachment over the Domain's limit.
type TooLargeError struct {
	FileName string
	Limit    int64
}

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("%s is larger than the %d MB limit", e.FileName, e.Limit>>20)
}

// isLocal reports whether an address is delivered on this Server.
func (s *Service) isLocal(address string) bool {
	if s.federation == nil {
		return true
	}
	return s.federation.IsLocal(federation.DomainOf(address))
}

// Party is an email address with the display name the viewing customer knows
// it by, which may be empty.
type Party struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

// Mailbox is a user's mailbox with its default sending identity.
type Mailbox struct {
	MailboxID   int64  `json:"mailboxId"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	UnreadCount int64  `json:"unreadCount"`
	IsDefault   bool   `json:"isDefault"`
	IsShared    bool   `json:"isShared"`
	SenderEmail string `json:"senderEmail"`
	SenderName  string `json:"senderName"`
}

// Folder organises threads inside a mailbox.
type Folder struct {
	FolderID    int64  `json:"folderId"`
	Name        string `json:"name"`
	UnreadCount int64  `json:"unreadCount"`
	IsDefault   bool   `json:"isDefault"`
}

// Thread is a conversation as one customer sees it.
type Thread struct {
	ThreadID        int64   `json:"threadId"`
	Subject         string  `json:"subject"`
	Summary         string  `json:"summary"`
	UnreadCount     int64   `json:"unreadCount"`
	Subscribers     []Party `json:"subscribers"`
	AttachmentCount int64   `json:"attachmentCount"`
	UpdatedAt       string  `json:"updatedAt"`
}

// AttachmentRef is attachment metadata as listed with a message.
type AttachmentRef struct {
	AttachmentID int64  `json:"attachmentId"`
	FileName     string `json:"fileName"`
	MimeType     string `json:"mimeType"`
	FileSize     int64  `json:"fileSize"`
}

// Message is one message in a thread as one customer sees it.
type Message struct {
	MessageID   int64           `json:"messageId"`
	SenderEmail string          `json:"senderEmail"`
	SenderName  string          `json:"senderName"`
	Recipients  []Party         `json:"recipients"`
	Body        string          `json:"body"`
	SentAt      string          `json:"sentAt"`
	ReadAt      *string         `json:"readAt"`
	Attachments []AttachmentRef `json:"attachments"`
}

// AttachmentData is an attachment with its bytes, base64 encoded by the API.
type AttachmentData struct {
	FileName string `json:"fileName"`
	MimeType string `json:"mimeType"`
	FileSize int64  `json:"fileSize"`
	FileData []byte `json:"fileData"`
}

// Contact is an address book entry.
type Contact struct {
	ContactID int64  `json:"contactId"`
	Email     string `json:"email"`
	Name      string `json:"name"`
}

// NewAttachment is an attachment supplied when composing.
type NewAttachment struct {
	FileName string
	MimeType string
	FileData []byte
}

// rfc3339 formats a timestamp the way PHP's date('c') does, with an explicit
// +00:00 offset rather than Go's "Z", so responses stay byte-identical to the
// PHP backend while both are in service.
func rfc3339(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05-07:00") }

func rfc3339Ptr(t sql.NullTime) *string {
	if !t.Valid {
		return nil
	}
	s := rfc3339(t.Time)
	return &s
}

// threadMatch is the search condition for a thread: its subject or the body or
// sender of one of its messages. It expects g_thread and g_message joined.
func (s *Service) threadMatch(search string) (string, []any) {
	q := store.ParseQuery(search)
	subject, args := s.db.Match("g_thread", q)
	message, messageArgs := s.db.Match("g_message", q)
	return "(" + subject + " OR " + message + ")", append(args, messageArgs...)
}

// DefaultMailboxID returns the customer's default mailbox, or 0.
func (s *Service) DefaultMailboxID(ctx context.Context, customerID int64) (int64, error) {
	return store.SelectInt(ctx, s.db,
		`SELECT "default_mailbox_id" FROM "customer" WHERE "id" = ? LIMIT 1`, customerID)
}

// DefaultFolderID returns a mailbox's default folder, or 0.
func (s *Service) DefaultFolderID(ctx context.Context, customerID, mailboxID int64) (int64, error) {
	return store.SelectInt(ctx, s.db,
		`SELECT "default_folder_id" FROM "mailbox" WHERE "customer_id" = ? AND "id" = ? LIMIT 1`,
		customerID, mailboxID)
}

// MailboxIDForFolder returns the mailbox a folder belongs to, or 0 when the
// folder does not belong to this customer.
func (s *Service) MailboxIDForFolder(ctx context.Context, customerID, folderID int64) (int64, error) {
	return store.SelectInt(ctx, s.db,
		`SELECT "mailbox_id" FROM "folder" WHERE "customer_id" = ? AND "id" = ? LIMIT 1`,
		customerID, folderID)
}

// MailboxExists reports whether the mailbox belongs to the customer.
func (s *Service) MailboxExists(ctx context.Context, customerID, mailboxID int64) (bool, error) {
	id, err := store.SelectInt(ctx, s.db,
		`SELECT "id" FROM "mailbox" WHERE "customer_id" = ? AND "id" = ? LIMIT 1`, customerID, mailboxID)
	return id != 0, err
}

// ListMailboxes returns the customer's mailboxes with unread counts. A search
// term restricts the counts to matching threads.
func (s *Service) ListMailboxes(ctx context.Context, customerID int64, search string) ([]Mailbox, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT "mailbox"."id", "mailbox"."name", "mailbox"."color", `+
		`"mailbox"."user_id", "alias"."email", "alias"."name" `+
		`FROM "mailbox", "alias" `+
		`WHERE "mailbox"."default_alias_id" = "alias"."id" AND "mailbox"."customer_id" = ? `+
		`ORDER BY "mailbox"."id"`, customerID)
	if err != nil {
		return nil, fmt.Errorf("list mailboxes: %w", err)
	}
	defer rows.Close()

	var mailboxes []Mailbox
	var ids []int64
	for rows.Next() {
		var m Mailbox
		var userID sql.NullInt64
		var name sql.NullString
		if err := rows.Scan(&m.MailboxID, &name, &m.Color, &userID, &m.SenderEmail, &m.SenderName); err != nil {
			return nil, fmt.Errorf("scan mailbox: %w", err)
		}
		m.Name = name.String
		if m.Name == "" {
			m.Name = m.SenderEmail
		}
		m.IsShared = !userID.Valid
		mailboxes = append(mailboxes, m)
		ids = append(ids, m.MailboxID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list mailboxes: %w", err)
	}
	if len(mailboxes) == 0 {
		return []Mailbox{}, nil
	}

	counts, err := s.mailboxCounts(ctx, customerID, ids, search)
	if err != nil {
		return nil, err
	}
	defaultID, err := s.DefaultMailboxID(ctx, customerID)
	if err != nil {
		return nil, err
	}
	for i := range mailboxes {
		mailboxes[i].UnreadCount = counts[mailboxes[i].MailboxID]
		mailboxes[i].IsDefault = mailboxes[i].MailboxID == defaultID
	}
	return mailboxes, nil
}

func (s *Service) mailboxCounts(ctx context.Context, customerID int64, ids []int64, search string) (map[int64]int64, error) {
	in := store.Placeholders(len(ids))
	if search != "" {
		match, matchArgs := s.threadMatch(search)
		args := append([]any{customerID}, store.Args(ids)...)
		args = append(args, matchArgs...)
		counts, err := store.SelectCounts(ctx, s.db,
			`SELECT "mailbox_id", COUNT(DISTINCT "thread"."id") FROM "thread" `+
				`LEFT JOIN "folder" ON "thread"."folder_id" = "folder"."id" `+
				`LEFT JOIN "g_thread" ON "thread"."g_thread_id" = "g_thread"."id" `+
				`LEFT JOIN "g_message" ON "g_thread"."id" = "g_message"."g_thread_id" `+
				`WHERE "thread"."customer_id" = ? AND "mailbox_id" IN (`+in+") AND "+match+" "+
				`GROUP BY "mailbox_id"`, args...)
		if err != nil {
			return nil, fmt.Errorf("count matching threads per mailbox: %w", err)
		}
		return counts, nil
	}
	args := append([]any{customerID}, store.Args(ids)...)
	counts, err := store.SelectCounts(ctx, s.db,
		`SELECT "mailbox_id", COUNT("thread"."id") - COUNT("read_at") FROM "thread" `+
			`LEFT JOIN "folder" ON "thread"."folder_id" = "folder"."id" `+
			`WHERE "thread"."customer_id" = ? AND "mailbox_id" IN (`+in+") "+
			`GROUP BY "mailbox_id"`, args...)
	if err != nil {
		return nil, fmt.Errorf("count unread threads per mailbox: %w", err)
	}
	return counts, nil
}

// ListFolders returns the folders of a mailbox with unread counts.
func (s *Service) ListFolders(ctx context.Context, customerID, mailboxID int64, search string) ([]Folder, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT "id", "name" FROM "folder" WHERE "customer_id" = ? AND "deleted_at" IS NULL AND "mailbox_id" = ? ORDER BY "id"`,
		customerID, mailboxID)
	if err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	defer rows.Close()

	var folders []Folder
	var ids []int64
	for rows.Next() {
		var f Folder
		if err := rows.Scan(&f.FolderID, &f.Name); err != nil {
			return nil, fmt.Errorf("scan folder: %w", err)
		}
		folders = append(folders, f)
		ids = append(ids, f.FolderID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	if len(folders) == 0 {
		return []Folder{}, nil
	}

	in := store.Placeholders(len(ids))
	var counts map[int64]int64
	if search != "" {
		match, matchArgs := s.threadMatch(search)
		args := append([]any{customerID}, store.Args(ids)...)
		args = append(args, matchArgs...)
		counts, err = store.SelectCounts(ctx, s.db,
			`SELECT "folder_id", COUNT(DISTINCT "thread"."id") FROM "thread" `+
				`LEFT JOIN "g_thread" ON "thread"."g_thread_id" = "g_thread"."id" `+
				`LEFT JOIN "g_message" ON "g_thread"."id" = "g_message"."g_thread_id" `+
				`WHERE "thread"."customer_id" = ? AND "folder_id" IN (`+in+") AND "+match+" "+
				`GROUP BY "folder_id"`, args...)
	} else {
		args := append([]any{customerID}, store.Args(ids)...)
		counts, err = store.SelectCounts(ctx, s.db,
			`SELECT "folder_id", COUNT("id") - COUNT("read_at") FROM "thread" `+
				`WHERE "customer_id" = ? AND "folder_id" IN (`+in+`) GROUP BY "folder_id"`, args...)
	}
	if err != nil {
		return nil, fmt.Errorf("count threads per folder: %w", err)
	}

	defaultID, err := s.DefaultFolderID(ctx, customerID, mailboxID)
	if err != nil {
		return nil, err
	}
	for i := range folders {
		folders[i].UnreadCount = counts[folders[i].FolderID]
		folders[i].IsDefault = folders[i].FolderID == defaultID
	}
	return folders, nil
}

// ListThreads returns the threads in a folder, most recently updated first.
func (s *Service) ListThreads(ctx context.Context, customerID, folderID int64, search string) ([]Thread, error) {
	var rows *sql.Rows
	var err error
	// NULLS LAST keeps MySQL's order, where NULL sorts lowest; PostgreSQL would
	// put threads with no update time first. The id breaks ties, so both
	// engines list threads updated in the same second in the same order.
	if search != "" {
		match, matchArgs := s.threadMatch(search)
		rows, err = s.db.QueryContext(ctx,
			`SELECT "thread"."id", "thread"."g_thread_id", "g_thread"."subject_line", `+
				`"g_thread"."body_summary", "g_thread"."updated_at" FROM "thread" `+
				`LEFT JOIN "g_thread" ON "thread"."g_thread_id" = "g_thread"."id" `+
				`LEFT JOIN "g_message" ON "g_thread"."id" = "g_message"."g_thread_id" `+
				`WHERE "thread"."customer_id" = ? AND "thread"."deleted_at" IS NULL AND "folder_id" = ? AND `+
				match+" "+
				`GROUP BY "thread"."id", "g_thread"."id" `+
				`ORDER BY "g_thread"."updated_at" DESC NULLS LAST, "thread"."id" DESC`,
			append([]any{customerID, folderID}, matchArgs...)...)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT "thread"."id", "thread"."g_thread_id", "g_thread"."subject_line", `+
				`"g_thread"."body_summary", "g_thread"."updated_at" FROM "thread" `+
				`LEFT JOIN "g_thread" ON "thread"."g_thread_id" = "g_thread"."id" `+
				`WHERE "customer_id" = ? AND "deleted_at" IS NULL AND "folder_id" = ? `+
				`ORDER BY "g_thread"."updated_at" DESC NULLS LAST, "thread"."id" DESC`,
			customerID, folderID)
	}
	if err != nil {
		return nil, fmt.Errorf("list threads: %w", err)
	}
	defer rows.Close()

	var threads []Thread
	var threadIDs, gThreadIDs []int64
	gThreadByThread := map[int64]int64{}
	for rows.Next() {
		var t Thread
		var gThreadID int64
		var updated sql.NullTime
		if err := rows.Scan(&t.ThreadID, &gThreadID, &t.Subject, &t.Summary, &updated); err != nil {
			return nil, fmt.Errorf("scan thread: %w", err)
		}
		if updated.Valid {
			t.UpdatedAt = rfc3339(updated.Time)
		}
		t.Subscribers = []Party{}
		threads = append(threads, t)
		threadIDs = append(threadIDs, t.ThreadID)
		gThreadIDs = append(gThreadIDs, gThreadID)
		gThreadByThread[t.ThreadID] = gThreadID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list threads: %w", err)
	}
	if len(threads) == 0 {
		return []Thread{}, nil
	}

	counts, err := s.threadMessageCounts(ctx, customerID, threadIDs, search)
	if err != nil {
		return nil, err
	}

	inT := store.Placeholders(len(threadIDs))
	attachmentCounts, err := store.SelectCounts(ctx, s.db,
		`SELECT "thread_id", COUNT("attachment"."id") FROM "attachment" `+
			`LEFT JOIN "message" ON "message"."id" = "attachment"."message_id" `+
			`WHERE "attachment"."customer_id" = ? AND "attachment"."deleted_at" IS NULL `+
			`AND "thread_id" IN (`+inT+`) GROUP BY "thread_id"`,
		append([]any{customerID}, store.Args(threadIDs)...)...)
	if err != nil {
		return nil, fmt.Errorf("count thread attachments: %w", err)
	}

	subscribers, err := s.subscribersByThread(ctx, customerID, gThreadIDs)
	if err != nil {
		return nil, err
	}

	for i := range threads {
		threads[i].UnreadCount = counts[threads[i].ThreadID]
		threads[i].AttachmentCount = attachmentCounts[threads[i].ThreadID]
		if subs := subscribers[gThreadByThread[threads[i].ThreadID]]; subs != nil {
			threads[i].Subscribers = subs
		}
	}
	return threads, nil
}

func (s *Service) threadMessageCounts(ctx context.Context, customerID int64, threadIDs []int64, search string) (map[int64]int64, error) {
	in := store.Placeholders(len(threadIDs))
	if search != "" {
		match, matchArgs := s.db.Match("g_message", store.ParseQuery(search))
		args := append([]any{customerID}, store.Args(threadIDs)...)
		args = append(args, matchArgs...)
		counts, err := store.SelectCounts(ctx, s.db,
			`SELECT "thread_id", COUNT("message"."id") FROM "message" `+
				`LEFT JOIN "g_message" ON "g_message"."id" = "message"."g_message_id" `+
				`WHERE "customer_id" = ? AND "thread_id" IN (`+in+") AND "+match+" "+
				`GROUP BY "thread_id"`, args...)
		if err != nil {
			return nil, fmt.Errorf("count matching messages per thread: %w", err)
		}
		return counts, nil
	}
	counts, err := store.SelectCounts(ctx, s.db,
		`SELECT "thread_id", COUNT("id") - COUNT("read_at") FROM "message" `+
			`WHERE "customer_id" = ? AND "thread_id" IN (`+in+`) GROUP BY "thread_id"`,
		append([]any{customerID}, store.Args(threadIDs)...)...)
	if err != nil {
		return nil, fmt.Errorf("count unread messages per thread: %w", err)
	}
	return counts, nil
}

// subscribersByThread returns the subscribers of each canonical thread, with
// the customer's own alias addresses removed so a thread lists the other party
// rather than the viewer.
func (s *Service) subscribersByThread(ctx context.Context, customerID int64, gThreadIDs []int64) (map[int64][]Party, error) {
	if len(gThreadIDs) == 0 {
		return map[int64][]Party{}, nil
	}
	in := store.Placeholders(len(gThreadIDs))
	rows, err := s.db.QueryContext(ctx,
		`SELECT "g_subscription"."g_thread_id", "g_subscription"."email", "contact"."name" `+
			`FROM "g_subscription" `+
			`LEFT JOIN "contact" ON "g_subscription"."email" = "contact"."email" `+
			`AND "contact"."customer_id" = ? `+
			`WHERE "g_thread_id" IN (`+in+`) AND `+isMember,
		append([]any{customerID}, store.Args(gThreadIDs)...)...)
	if err != nil {
		return nil, fmt.Errorf("list thread subscribers: %w", err)
	}
	defer rows.Close()

	aliases, err := s.aliasSet(ctx, customerID)
	if err != nil {
		return nil, err
	}
	out := map[int64][]Party{}
	for rows.Next() {
		var gThreadID int64
		var email string
		var name sql.NullString
		if err := rows.Scan(&gThreadID, &email, &name); err != nil {
			return nil, fmt.Errorf("scan subscriber: %w", err)
		}
		if aliases[email] {
			continue
		}
		out[gThreadID] = append(out[gThreadID], Party{Email: email, Name: name.String})
	}
	return out, rows.Err()
}

func (s *Service) aliasSet(ctx context.Context, customerID int64) (map[string]bool, error) {
	emails, err := store.SelectStrings(ctx, s.db,
		`SELECT "email" FROM "alias" WHERE "customer_id" = ?`, customerID)
	if err != nil {
		return nil, fmt.Errorf("list aliases: %w", err)
	}
	set := make(map[string]bool, len(emails))
	for _, e := range emails {
		set[e] = true
	}
	return set, nil
}

// ListMessages returns the messages of a thread, newest first.
// isMember is the condition on "g_subscription" for an address that is on
// the thread: not refused as unknown and not departed (RFC 5.6).
const isMember = `"refused_at" IS NULL AND "left_at" IS NULL`

// messageOrder lists a thread newest first in the order every Domain shares:
// send time in whole seconds, then federation id (RFC 5.5). Messages from
// before federation have no federation id and fall back to their row order.
const messageOrder = `ORDER BY "g_message"."sent_at" DESC NULLS LAST, ` +
	`"g_message"."federation_id" DESC NULLS LAST, "message"."id" DESC`

func (s *Service) ListMessages(ctx context.Context, customerID, threadID int64, search string) ([]Message, error) {
	var rows *sql.Rows
	var err error
	if search != "" {
		match, matchArgs := s.db.Match("g_message", store.ParseQuery(search))
		rows, err = s.db.QueryContext(ctx,
			`SELECT "message"."id", "message"."read_at", "g_message"."id", "g_message"."sender_email", `+
				`"g_message"."sender_name", "g_message"."body", "g_message"."sent_at" FROM "message" `+
				`LEFT JOIN "g_message" ON "message"."g_message_id" = "g_message"."id" `+
				`WHERE "customer_id" = ? AND "thread_id" = ? AND `+match+" "+
				messageOrder,
			append([]any{customerID, threadID}, matchArgs...)...)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT "message"."id", "message"."read_at", "g_message"."id", "g_message"."sender_email", `+
				`"g_message"."sender_name", "g_message"."body", "g_message"."sent_at" FROM "message" `+
				`LEFT JOIN "g_message" ON "message"."g_message_id" = "g_message"."id" `+
				`WHERE "customer_id" = ? AND "message"."deleted_at" IS NULL AND "thread_id" = ? `+
				messageOrder,
			customerID, threadID)
	}
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	var messages []Message
	var messageIDs []int64
	for rows.Next() {
		var m Message
		var gMessageID int64
		var readAt, sentAt sql.NullTime
		if err := rows.Scan(&m.MessageID, &readAt, &gMessageID, &m.SenderEmail,
			&m.SenderName, &m.Body, &sentAt); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		m.ReadAt = rfc3339Ptr(readAt)
		if sentAt.Valid {
			m.SentAt = rfc3339(sentAt.Time)
		}
		m.Recipients = []Party{}
		m.Attachments = []AttachmentRef{}
		messages = append(messages, m)
		messageIDs = append(messageIDs, m.MessageID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	if len(messages) == 0 {
		return []Message{}, nil
	}

	gThreadID, err := store.SelectInt(ctx, s.db,
		`SELECT "g_thread_id" FROM "thread" WHERE "customer_id" = ? AND "deleted_at" IS NULL AND "id" = ? LIMIT 1`,
		customerID, threadID)
	if err != nil {
		return nil, fmt.Errorf("look up thread: %w", err)
	}

	// Contact names the viewing customer has for the people in this thread.
	names, err := store.SelectPairs(ctx, s.db,
		`SELECT "g_subscription"."email", "contact"."name" FROM "g_subscription" `+
			`LEFT JOIN "contact" ON "g_subscription"."email" = "contact"."email" `+
			`AND "contact"."customer_id" = ? WHERE "g_thread_id" = ? AND `+isMember,
		customerID, gThreadID)
	if err != nil {
		return nil, fmt.Errorf("list subscriber names: %w", err)
	}

	attachments, err := s.attachmentsByMessage(ctx, customerID, messageIDs)
	if err != nil {
		return nil, err
	}

	for i := range messages {
		if name := names[messages[i].SenderEmail]; name != "" {
			messages[i].SenderName = name
		}
		// Recipients are the subscribers other than the sender.
		for email, name := range names {
			if email == messages[i].SenderEmail {
				continue
			}
			messages[i].Recipients = append(messages[i].Recipients, Party{Email: email, Name: name})
		}
		if refs := attachments[messages[i].MessageID]; refs != nil {
			messages[i].Attachments = refs
		}
	}
	return messages, nil
}

func (s *Service) attachmentsByMessage(ctx context.Context, customerID int64, messageIDs []int64) (map[int64][]AttachmentRef, error) {
	in := store.Placeholders(len(messageIDs))
	rows, err := s.db.QueryContext(ctx,
		`SELECT "attachment"."message_id", "attachment"."id", "g_attachment"."file_name", `+
			`"g_attachment"."mime_type", "g_attachment"."file_size" FROM "attachment" `+
			`LEFT JOIN "g_attachment" ON "g_attachment"."id" = "attachment"."g_attachment_id" `+
			`WHERE "attachment"."customer_id" = ? AND "attachment"."deleted_at" IS NULL `+
			`AND "attachment"."message_id" IN (`+in+`) ORDER BY "attachment"."id"`,
		append([]any{customerID}, store.Args(messageIDs)...)...)
	if err != nil {
		return nil, fmt.Errorf("list attachments: %w", err)
	}
	defer rows.Close()

	out := map[int64][]AttachmentRef{}
	for rows.Next() {
		var messageID int64
		var a AttachmentRef
		if err := rows.Scan(&messageID, &a.AttachmentID, &a.FileName, &a.MimeType, &a.FileSize); err != nil {
			return nil, fmt.Errorf("scan attachment: %w", err)
		}
		out[messageID] = append(out[messageID], a)
	}
	return out, rows.Err()
}

// GetAttachment returns an attachment with its bytes and marks it read.
func (s *Service) GetAttachment(ctx context.Context, customerID, attachmentID int64) (*AttachmentData, error) {
	var a AttachmentData
	var hash string
	err := s.db.QueryRowContext(ctx,
		`SELECT "g_attachment"."file_name", "g_attachment"."mime_type", "g_attachment"."file_size", `+
			`"g_attachment"."hash_md5" FROM "attachment" `+
			`LEFT JOIN "g_attachment" ON "g_attachment"."id" = "attachment"."g_attachment_id" `+
			`WHERE "attachment"."customer_id" = ? AND "attachment"."deleted_at" IS NULL `+
			`AND "attachment"."id" = ? LIMIT 1`,
		customerID, attachmentID).Scan(&a.FileName, &a.MimeType, &a.FileSize, &hash)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look up attachment: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE "attachment" SET "read_at" = ? WHERE "customer_id" = ? AND "deleted_at" IS NULL AND "id" = ?`,
		s.db.Now(), customerID, attachmentID); err != nil {
		return nil, fmt.Errorf("mark attachment read: %w", err)
	}
	data, err := s.blob.Get(hash)
	if err != nil {
		return nil, err
	}
	a.FileData = data
	return &a, nil
}

// DeleteAttachment soft-deletes the customer's projection of an attachment.
func (s *Service) DeleteAttachment(ctx context.Context, customerID, attachmentID int64) (bool, error) {
	n, err := store.Affected(ctx, s.db,
		`UPDATE "attachment" SET "deleted_at" = ? `+
			`WHERE "customer_id" = ? AND "deleted_at" IS NULL AND "id" = ?`,
		s.db.Now(), customerID, attachmentID)
	if err != nil {
		return false, fmt.Errorf("delete attachment: %w", err)
	}
	return n > 0, nil
}

// ListContacts returns the customer's address book.
func (s *Service) ListContacts(ctx context.Context, customerID int64, search string) ([]Contact, error) {
	var rows *sql.Rows
	var err error
	if search != "" {
		match, matchArgs := s.db.Match("contact", store.ParseQuery(search))
		rows, err = s.db.QueryContext(ctx,
			`SELECT "id", "email", "name" FROM "contact" WHERE "customer_id" = ? AND "deleted_at" IS NULL AND `+
				match+` ORDER BY "id"`, append([]any{customerID}, matchArgs...)...)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT "id", "email", "name" FROM "contact" WHERE "customer_id" = ? AND "deleted_at" IS NULL ORDER BY "id"`,
			customerID)
	}
	if err != nil {
		return nil, fmt.Errorf("list contacts: %w", err)
	}
	defer rows.Close()

	contacts := []Contact{}
	for rows.Next() {
		var c Contact
		if err := rows.Scan(&c.ContactID, &c.Email, &c.Name); err != nil {
			return nil, fmt.Errorf("scan contact: %w", err)
		}
		contacts = append(contacts, c)
	}
	return contacts, rows.Err()
}

// AddContact creates a contact, returning the existing id when the address is
// already known.
func (s *Service) AddContact(ctx context.Context, customerID int64, email, name string) (int64, error) {
	email = store.Address(email)
	id, err := store.SelectInt(ctx, s.db,
		`SELECT "id" FROM "contact" WHERE "customer_id" = ? AND "email" = ? LIMIT 1`, customerID, email)
	if err != nil {
		return 0, fmt.Errorf("look up contact: %w", err)
	}
	if id != 0 {
		return id, nil
	}
	id, err = store.Insert(ctx, s.db,
		`INSERT INTO "contact" ("customer_id", "email", "name") VALUES (?,?,?)`, customerID, email, name)
	if err != nil {
		return 0, fmt.Errorf("create contact: %w", err)
	}
	return id, nil
}

// EditContact updates an address or name. It refuses an address already used by
// a different contact.
func (s *Service) EditContact(ctx context.Context, customerID, contactID int64, email, name string) (bool, error) {
	email = store.Address(email)
	if email != "" {
		other, err := store.SelectInt(ctx, s.db,
			`SELECT "id" FROM "contact" WHERE "customer_id" = ? AND "email" = ? LIMIT 1`, customerID, email)
		if err != nil {
			return false, fmt.Errorf("look up contact: %w", err)
		}
		if other != 0 && other != contactID {
			return false, nil
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE "contact" SET "email" = ? WHERE "customer_id" = ? AND "id" = ?`,
			email, customerID, contactID); err != nil {
			return false, fmt.Errorf("update contact email: %w", err)
		}
	}
	if name != "" {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE "contact" SET "name" = ? WHERE "customer_id" = ? AND "id" = ?`,
			name, customerID, contactID); err != nil {
			return false, fmt.Errorf("update contact name: %w", err)
		}
	}
	return true, nil
}

// DeleteContact soft-deletes a contact.
func (s *Service) DeleteContact(ctx context.Context, customerID, contactID int64) (bool, error) {
	n, err := store.Affected(ctx, s.db,
		`UPDATE "contact" SET "deleted_at" = ? WHERE "customer_id" = ? AND "id" = ?`,
		s.db.Now(), customerID, contactID)
	if err != nil {
		return false, fmt.Errorf("delete contact: %w", err)
	}
	return n > 0, nil
}

// MarkThreadRead marks a thread and all its messages read for one customer.
func (s *Service) MarkThreadRead(ctx context.Context, q store.Queryer, customerID, threadID int64) error {
	if _, err := q.ExecContext(ctx,
		`UPDATE "message" SET "read_at" = ? WHERE "customer_id" = ? AND "thread_id" = ? AND "read_at" IS NULL`,
		s.db.Now(), customerID, threadID); err != nil {
		return fmt.Errorf("mark messages read: %w", err)
	}
	if _, err := q.ExecContext(ctx,
		`UPDATE "thread" SET "read_at" = ? WHERE "customer_id" = ? AND "id" = ? AND "read_at" IS NULL`,
		s.db.Now(), customerID, threadID); err != nil {
		return fmt.Errorf("mark thread read: %w", err)
	}
	return nil
}

// MarkMessageRead marks one message, and its thread, read.
func (s *Service) MarkMessageRead(ctx context.Context, customerID, messageID int64) (bool, error) {
	threadID, err := store.SelectInt(ctx, s.db,
		`SELECT "thread_id" FROM "message" WHERE "customer_id" = ? AND "id" = ? LIMIT 1`, customerID, messageID)
	if err != nil {
		return false, fmt.Errorf("look up message: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE "message" SET "read_at" = ? WHERE "customer_id" = ? AND "id" = ?`,
		s.db.Now(), customerID, messageID); err != nil {
		return false, fmt.Errorf("mark message read: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE "thread" SET "read_at" = ? WHERE "customer_id" = ? AND "id" = ?`,
		s.db.Now(), customerID, threadID); err != nil {
		return false, fmt.Errorf("mark thread read: %w", err)
	}
	return true, nil
}

// EditThread moves a thread to another of the customer's folders.
func (s *Service) EditThread(ctx context.Context, customerID, threadID, folderID int64) (bool, error) {
	if folderID == 0 {
		return true, nil
	}
	found, err := store.SelectInt(ctx, s.db,
		`SELECT "id" FROM "folder" WHERE "customer_id" = ? AND "id" = ?`, customerID, folderID)
	if err != nil {
		return false, fmt.Errorf("look up folder: %w", err)
	}
	if found == 0 {
		return false, nil
	}
	n, err := store.Affected(ctx, s.db,
		`UPDATE "thread" SET "folder_id" = ? WHERE "customer_id" = ? AND "id" = ?`,
		folderID, customerID, threadID)
	if err != nil {
		return false, fmt.Errorf("move thread: %w", err)
	}
	return n > 0, nil
}

// DeleteThread soft-deletes a thread and its messages and attachments for one
// customer. The canonical content is untouched.
func (s *Service) DeleteThread(ctx context.Context, customerID, threadID int64) (bool, error) {
	var deleted bool
	now := s.db.Now()
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		n, err := store.Affected(ctx, tx,
			`UPDATE "thread" SET "deleted_at" = ? `+
				`WHERE "customer_id" = ? AND "deleted_at" IS NULL AND "id" = ?`,
			now, customerID, threadID)
		if err != nil {
			return fmt.Errorf("delete thread: %w", err)
		}
		if n == 0 {
			return nil
		}
		deleted = true
		if _, err := tx.ExecContext(ctx,
			`UPDATE "message" SET "deleted_at" = ? `+
				`WHERE "customer_id" = ? AND "thread_id" = ? AND "deleted_at" IS NULL`,
			now, customerID, threadID); err != nil {
			return fmt.Errorf("delete thread messages: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE "attachment" SET "deleted_at" = ? WHERE "deleted_at" IS NULL AND "message_id" IN `+
				`(SELECT "id" FROM "message" WHERE "customer_id" = ? AND "thread_id" = ?)`,
			now, customerID, threadID); err != nil {
			return fmt.Errorf("delete thread attachments: %w", err)
		}
		return nil
	})
	return deleted, err
}

// AddFolder creates a folder, making it the default when it is the mailbox's
// first.
func (s *Service) AddFolder(ctx context.Context, customerID, mailboxID int64, name string) (int64, error) {
	var folderID int64
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		folderID, err = store.Insert(ctx, tx,
			`INSERT INTO "folder" ("customer_id", "mailbox_id", "name") VALUES (?,?,?)`,
			customerID, mailboxID, name)
		if err != nil {
			return fmt.Errorf("create folder: %w", err)
		}
		count, err := store.SelectInt(ctx, tx,
			`SELECT COUNT("id") FROM "folder" WHERE "customer_id" = ? AND "mailbox_id" = ?`,
			customerID, mailboxID)
		if err != nil {
			return fmt.Errorf("count folders: %w", err)
		}
		if count == 1 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE "mailbox" SET "default_folder_id" = ? WHERE "customer_id" = ? AND "id" = ?`,
				folderID, customerID, mailboxID); err != nil {
				return fmt.Errorf("set default folder: %w", err)
			}
		}
		return nil
	})
	return folderID, err
}

// EditFolder renames a folder and optionally makes it the mailbox default.
func (s *Service) EditFolder(ctx context.Context, customerID, folderID int64, name string, isDefault bool) (bool, error) {
	if name != "" {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE "folder" SET "name" = ? WHERE "customer_id" = ? AND "id" = ?`,
			name, customerID, folderID); err != nil {
			return false, fmt.Errorf("rename folder: %w", err)
		}
	}
	if isDefault {
		mailboxID, err := s.MailboxIDForFolder(ctx, customerID, folderID)
		if err != nil {
			return false, err
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE "mailbox" SET "default_folder_id" = ? WHERE "customer_id" = ? AND "id" = ?`,
			folderID, customerID, mailboxID); err != nil {
			return false, fmt.Errorf("set default folder: %w", err)
		}
	}
	return true, nil
}

// DeleteFolder removes a folder, moving its threads to the mailbox default. The
// default folder itself cannot be deleted.
func (s *Service) DeleteFolder(ctx context.Context, customerID, folderID int64) (bool, error) {
	mailboxID, err := s.MailboxIDForFolder(ctx, customerID, folderID)
	if err != nil {
		return false, err
	}
	defaultFolderID, err := s.DefaultFolderID(ctx, customerID, mailboxID)
	if err != nil {
		return false, err
	}
	if defaultFolderID == folderID {
		return false, nil
	}
	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE "thread" SET "folder_id" = ? WHERE "customer_id" = ? AND "folder_id" = ?`,
			defaultFolderID, customerID, folderID); err != nil {
			return fmt.Errorf("move threads out of folder: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM "folder" WHERE "customer_id" = ? AND "id" = ?`,
			customerID, folderID); err != nil {
			return fmt.Errorf("delete folder: %w", err)
		}
		return nil
	})
	return err == nil, err
}

// defaultDomainQuery picks the Domain generated aliases go on: a Domain this
// Server hosts, never a peer that federation discovered. Peers have no owning
// customer either, which is why the old "customer_id IS NULL" test now finds
// them. Without federation nothing is marked local and every row is our own.
const defaultDomainQuery = `SELECT "name" FROM "domain" ORDER BY "is_local" DESC, "id" LIMIT 1`

// DefaultDomain returns the domain used for generated aliases.
func (s *Service) DefaultDomain(ctx context.Context, q store.Queryer) (string, error) {
	return store.SelectString(ctx, q, defaultDomainQuery)
}

// AddMailbox creates a mailbox with an Inbox, a generated alias on the default
// domain and the given sending identity.
func (s *Service) AddMailbox(ctx context.Context, customerID int64, name, color, senderEmail, senderName string) (int64, error) {
	senderEmail = store.Address(senderEmail)
	var mailboxID int64
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		mailboxID, err = addMailboxTx(ctx, tx, customerID, name, color, senderEmail, senderName)
		return err
	})
	return mailboxID, err
}

func addMailboxTx(ctx context.Context, tx *sql.Tx, customerID int64, name, color, senderEmail, senderName string) (int64, error) {
	domain, err := store.SelectString(ctx, tx, defaultDomainQuery)
	if err != nil {
		return 0, fmt.Errorf("look up default domain: %w", err)
	}
	if domain == "" {
		// Without a domain there is no address to generate, so the mailbox
		// would be unusable.
		return 0, nil
	}
	mailboxID, err := store.Insert(ctx, tx,
		`INSERT INTO "mailbox" ("customer_id", "name", "color") VALUES (?,?,?)`, customerID, name, color)
	if err != nil {
		return 0, fmt.Errorf("create mailbox: %w", err)
	}
	folderID, err := store.Insert(ctx, tx,
		`INSERT INTO "folder" ("customer_id", "mailbox_id", "name") VALUES (?,?,?)`,
		customerID, mailboxID, "Inbox")
	if err != nil {
		return 0, fmt.Errorf("create inbox: %w", err)
	}
	generated, err := generatedAddress(domain)
	if err != nil {
		return 0, err
	}
	aliasID, err := store.Insert(ctx, tx,
		`INSERT INTO "alias" ("customer_id", "email", "name", "mailbox_id") VALUES (?,?,?,?)`,
		customerID, generated, senderName, mailboxID)
	if err != nil {
		return 0, fmt.Errorf("create alias: %w", err)
	}
	if senderEmail != "" {
		existing, err := store.SelectInt(ctx, tx,
			`SELECT "id" FROM "alias" WHERE "email" = ? AND "name" = ? LIMIT 1`, senderEmail, senderName)
		if err != nil {
			return 0, fmt.Errorf("look up sender alias: %w", err)
		}
		if existing == 0 {
			if _, err := store.Insert(ctx, tx,
				`INSERT INTO "alias" ("customer_id", "email", "name", "mailbox_id") VALUES (?,?,?,?)`,
				customerID, senderEmail, senderName, mailboxID); err != nil {
				return 0, fmt.Errorf("create sender alias: %w", err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE "mailbox" SET "default_folder_id" = ?, "default_alias_id" = ? WHERE "id" = ?`,
		folderID, aliasID, mailboxID); err != nil {
		return 0, fmt.Errorf("set mailbox defaults: %w", err)
	}
	count, err := store.SelectInt(ctx, tx,
		`SELECT COUNT("id") FROM "mailbox" WHERE "customer_id" = ?`, customerID)
	if err != nil {
		return 0, fmt.Errorf("count mailboxes: %w", err)
	}
	if count == 1 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE "customer" SET "default_mailbox_id" = ? WHERE "id" = ?`, mailboxID, customerID); err != nil {
			return 0, fmt.Errorf("set default mailbox: %w", err)
		}
	}
	return mailboxID, nil
}

// generatedAddress returns a random 7-digit local part on the given domain,
// matching the addresses the PHP backend generates.
func generatedAddress(domain string) (string, error) {
	const min = 1000 * 1000
	n, err := crand.Int(crand.Reader, big.NewInt(min*10-1-min))
	if err != nil {
		return "", fmt.Errorf("generate address: %w", err)
	}
	return fmt.Sprintf("%d@%s", n.Int64()+min, domain), nil
}

// EditMailbox updates a mailbox's name, colour, default and shared flags, and
// sending identity.
func (s *Service) EditMailbox(ctx context.Context, customerID, userID, mailboxID int64,
	name, color string, isDefault, isShared *bool, senderEmail, senderName string) (bool, error) {
	senderEmail = store.Address(senderEmail)
	return true, s.db.Tx(ctx, func(tx *sql.Tx) error {
		if name != "" {
			if _, err := tx.ExecContext(ctx,
				`UPDATE "mailbox" SET "name" = ?, "color" = ? WHERE "customer_id" = ? AND "id" = ?`,
				name, color, customerID, mailboxID); err != nil {
				return fmt.Errorf("rename mailbox: %w", err)
			}
		}
		if isDefault != nil && *isDefault {
			if _, err := tx.ExecContext(ctx,
				`UPDATE "customer" SET "default_mailbox_id" = ? WHERE "id" = ?`,
				mailboxID, customerID); err != nil {
				return fmt.Errorf("set default mailbox: %w", err)
			}
		}
		if isShared != nil {
			// The PHP writes `WHERE id = customerId` here, which edits the
			// wrong row. The mailbox being edited is the intended target.
			if *isShared {
				if _, err := tx.ExecContext(ctx,
					`UPDATE "mailbox" SET "user_id" = NULL WHERE "customer_id" = ? AND "id" = ?`,
					customerID, mailboxID); err != nil {
					return fmt.Errorf("share mailbox: %w", err)
				}
			} else {
				if _, err := tx.ExecContext(ctx,
					`UPDATE "mailbox" SET "user_id" = ? WHERE "customer_id" = ? AND "id" = ?`,
					userID, customerID, mailboxID); err != nil {
					return fmt.Errorf("unshare mailbox: %w", err)
				}
			}
		}
		if senderEmail != "" {
			aliasID, err := store.SelectInt(ctx, tx,
				`SELECT "id" FROM "alias" WHERE "customer_id" = ? AND "email" = ? LIMIT 1`,
				customerID, senderEmail)
			if err != nil {
				return fmt.Errorf("look up alias: %w", err)
			}
			if aliasID == 0 {
				aliasID, err = store.Insert(ctx, tx,
					`INSERT INTO "alias" ("customer_id", "email", "name", "mailbox_id") VALUES (?,?,?,?)`,
					customerID, senderEmail, senderName, mailboxID)
				if err != nil {
					return fmt.Errorf("create alias: %w", err)
				}
			} else if _, err := tx.ExecContext(ctx,
				`UPDATE "alias" SET "name" = ? WHERE "customer_id" = ? AND "id" = ? AND "name" <> ?`,
				senderName, customerID, aliasID, senderName); err != nil {
				return fmt.Errorf("rename alias: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE "mailbox" SET "default_alias_id" = ? WHERE "customer_id" = ? AND "id" = ?`,
				aliasID, customerID, mailboxID); err != nil {
				return fmt.Errorf("set default alias: %w", err)
			}
		}
		return nil
	})
}

// DeleteMailbox removes a mailbox, moving its aliases and folders to the
// default mailbox. The default mailbox itself cannot be deleted.
func (s *Service) DeleteMailbox(ctx context.Context, customerID, mailboxID int64) (bool, error) {
	defaultMailboxID, err := s.DefaultMailboxID(ctx, customerID)
	if err != nil {
		return false, err
	}
	if defaultMailboxID == mailboxID {
		return false, nil
	}
	err = s.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE "alias" SET "mailbox_id" = ? WHERE "customer_id" = ? AND "mailbox_id" = ?`,
			defaultMailboxID, customerID, mailboxID); err != nil {
			return fmt.Errorf("move aliases: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE "folder" SET "mailbox_id" = ? WHERE "customer_id" = ? AND "mailbox_id" = ?`,
			defaultMailboxID, customerID, mailboxID); err != nil {
			return fmt.Errorf("move folders: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM "mailbox" WHERE "customer_id" = ? AND "id" = ?`,
			customerID, mailboxID); err != nil {
			return fmt.Errorf("delete mailbox: %w", err)
		}
		return nil
	})
	return err == nil, err
}

// StoredFile is one attachment's bytes as this Server holds them: how many
// attachments refer to them and whether they are on disk. For the read-only
// controls (ADR 20); it carries no content and no file names.
type StoredFile struct {
	HashMD5     string `json:"hashMd5"`
	FileSize    int64  `json:"fileSize"`
	Attachments int64  `json:"attachments"`
	Stored      bool   `json:"stored"`
}

// StoredFiles lists the stored bytes whose MD5 is hashMD5. The blob store is
// addressed by content, so the same bytes attached twice are one file.
func (s *Service) StoredFiles(ctx context.Context, hashMD5 string) ([]StoredFile, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT "hash_md5", MAX("file_size"), COUNT(*) FROM "g_attachment" WHERE "hash_md5" = ? `+
			`GROUP BY "hash_md5"`, strings.ToLower(hashMD5))
	if err != nil {
		return nil, fmt.Errorf("list stored files: %w", err)
	}
	defer rows.Close()
	files := []StoredFile{}
	for rows.Next() {
		var f StoredFile
		if err := rows.Scan(&f.HashMD5, &f.FileSize, &f.Attachments); err != nil {
			return nil, fmt.Errorf("scan stored file: %w", err)
		}
		_, statErr := os.Stat(s.blob.Path(f.HashMD5))
		f.Stored = statErr == nil
		files = append(files, f)
	}
	return files, rows.Err()
}

// ThreadMembers is a thread's federated identity and who is on it. For the
// read-only controls (ADR 20); it carries no subject and no content.
type ThreadMembers struct {
	FederationID string   `json:"federationId"`
	Members      []string `json:"members"`
}

// ThreadsOf lists the federated threads an address is a member of.
func (s *Service) ThreadsOf(ctx context.Context, address string) ([]ThreadMembers, error) {
	ids, err := store.SelectStrings(ctx, s.db,
		`SELECT "g_thread"."federation_id" FROM "g_thread" `+
			`JOIN "g_subscription" ON "g_subscription"."g_thread_id" = "g_thread"."id" `+
			`WHERE "g_subscription"."email" = ? AND "g_thread"."federation_id" IS NOT NULL `+
			`ORDER BY "g_thread"."id"`, store.Address(address))
	if err != nil {
		return nil, fmt.Errorf("list threads of %s: %w", address, err)
	}
	out := []ThreadMembers{}
	for _, id := range ids {
		members, err := store.SelectStrings(ctx, s.db,
			`SELECT "email" FROM "g_subscription" JOIN "g_thread" ON "g_thread"."id" = "g_subscription"."g_thread_id" `+
				`WHERE "g_thread"."federation_id" = ? AND `+isMember+` ORDER BY "email"`, id)
		if err != nil {
			return nil, fmt.Errorf("list members of %s: %w", id, err)
		}
		out = append(out, ThreadMembers{FederationID: id, Members: members})
	}
	return out, nil
}
