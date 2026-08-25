// Package chat implements the chat domain: chats, participants, messages,
// attachments, presence and typing indicators.
//
// Presence and typing are implemented against the presence_status,
// typing_indicator and message_status tables. The PHP ChatClient returns
// hard-coded placeholders for those three, which is why the OpenAPI marks them
// "501 Not yet implemented"; the tables exist, so this package uses them.
package chat

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/mevdschee/webmail-server/internal/blob"
	"github.com/mevdschee/webmail-server/internal/store"
)

// EditWindow is how long after sending a message may still be edited or
// deleted by its author.
const EditWindow = 15 * time.Minute

// TypingTTL is how long a typing indicator stays visible without a keystroke.
const TypingTTL = 3 * time.Second

// Service implements the chat domain.
type Service struct {
	db   *store.DB
	blob *blob.Store
}

// New returns a Service. attachments is the directory holding attachment bytes.
func New(db *store.DB, attachments *blob.Store) *Service {
	return &Service{db: db, blob: attachments}
}

// ParticipantRef is a chat member as listed on a chat.
type ParticipantRef struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	Role  string `json:"role"`
}

// Chat is a conversation as one customer sees it.
type Chat struct {
	ChatID                 int64            `json:"chatId"`
	Type                   string           `json:"type"`
	Name                   *string          `json:"name"`
	UnreadCount            int64            `json:"unreadCount"`
	Muted                  bool             `json:"muted"`
	NotificationPreference string           `json:"notificationPreference"`
	Participants           []ParticipantRef `json:"participants"`
	CreatedAt              string           `json:"createdAt"`
	// LastMessageAt is when the newest message in the chat was sent, or empty
	// for a chat with none. A chat list sorts by recent activity, and creation
	// time is not that: a long-running conversation would sink below one nobody
	// has spoken in since it was opened.
	LastMessageAt string `json:"lastMessageAt"`
}

// Participant is a chat member with presence and membership dates.
type Participant struct {
	ParticipantID int64   `json:"participantId"`
	Email         string  `json:"email"`
	Name          string  `json:"name"`
	Role          string  `json:"role"`
	Status        string  `json:"status"`
	JoinedAt      string  `json:"joinedAt"`
	LeftAt        *string `json:"leftAt"`
}

// AttachmentRef is chat attachment metadata.
type AttachmentRef struct {
	AttachmentID int64  `json:"attachmentId"`
	FileName     string `json:"fileName"`
	MimeType     string `json:"mimeType"`
	FileSize     int64  `json:"fileSize"`
}

// DeliveryStatus counts how far a message has travelled to its recipients.
type DeliveryStatus struct {
	Sent      int64 `json:"sent"`
	Delivered int64 `json:"delivered"`
	Read      int64 `json:"read"`
}

// Message is a chat message as one customer sees it.
type Message struct {
	MessageID      int64           `json:"messageId"`
	SenderEmail    string          `json:"senderEmail"`
	SenderName     string          `json:"senderName"`
	Body           string          `json:"body"`
	BodyHTML       string          `json:"bodyHtml"`
	CreatedAt      string          `json:"createdAt"`
	EditedAt       *string         `json:"editedAt"`
	DeletedAt      *string         `json:"deletedAt"`
	DeliveredAt    *string         `json:"deliveredAt"`
	ReadAt         *string         `json:"readAt"`
	Attachments    []AttachmentRef `json:"attachments"`
	DeliveryStatus *DeliveryStatus `json:"deliveryStatus"`
}

// AttachmentData is a chat attachment with its bytes.
type AttachmentData struct {
	FileName string `json:"fileName"`
	MimeType string `json:"mimeType"`
	FileSize int64  `json:"fileSize"`
	FileData []byte `json:"fileData"`
}

// Presence is a user's availability.
type Presence struct {
	UserID         int64  `json:"userId"`
	Status         string `json:"status"`
	LastActivityAt string `json:"lastActivityAt"`
}

// TypingIndicator is an active typing notice in a chat.
type TypingIndicator struct {
	UserID    int64  `json:"userId"`
	UserName  string `json:"userName"`
	StartedAt string `json:"startedAt"`
	ExpiresAt string `json:"expiresAt"`
}

// NewAttachment is an attachment supplied when sending.
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

// ListChats returns the chats visible in a mailbox.
func (s *Service) ListChats(ctx context.Context, customerID, mailboxID int64, search string) ([]Chat, error) {
	var rows *sql.Rows
	var err error
	if search != "" {
		rows, err = s.db.QueryContext(ctx,
			"SELECT `chat`.`id`, `chat`.`g_chat_id`, `chat`.`muted`, `chat`.`notification_preference`, "+
				"`g_chat`.`type`, `g_chat`.`name`, `g_chat`.`created_at`, "+
				"MAX(`g_chat_message`.`created_at`) FROM `chat` "+
				"LEFT JOIN `g_chat` ON `chat`.`g_chat_id` = `g_chat`.`id` "+
				"LEFT JOIN `g_chat_message` ON `g_chat`.`id` = `g_chat_message`.`g_chat_id` "+
				"WHERE `chat`.`customer_id` = ? AND `chat`.`mailbox_id` = ? AND `chat`.`deleted_at` IS NULL AND "+
				"(MATCH(`g_chat_message`.`body`) AGAINST (? IN BOOLEAN MODE) "+
				"OR MATCH(`g_chat_message`.`sender_name`) AGAINST (? IN BOOLEAN MODE)) "+
				"GROUP BY `g_chat`.`id` ORDER BY MAX(`g_chat_message`.`created_at`) DESC, "+
				"`g_chat`.`created_at` DESC",
			customerID, mailboxID, search, search)
	} else {
		rows, err = s.db.QueryContext(ctx,
			"SELECT `chat`.`id`, `chat`.`g_chat_id`, `chat`.`muted`, `chat`.`notification_preference`, "+
				"`g_chat`.`type`, `g_chat`.`name`, `g_chat`.`created_at`, "+
				"MAX(`g_chat_message`.`created_at`) FROM `chat` "+
				"LEFT JOIN `g_chat` ON `chat`.`g_chat_id` = `g_chat`.`id` "+
				"LEFT JOIN `g_chat_message` ON `g_chat`.`id` = `g_chat_message`.`g_chat_id` "+
				"WHERE `chat`.`customer_id` = ? AND `chat`.`mailbox_id` = ? AND `chat`.`deleted_at` IS NULL "+
				"GROUP BY `g_chat`.`id` ORDER BY MAX(`g_chat_message`.`created_at`) DESC, "+
				"`g_chat`.`created_at` DESC",
			customerID, mailboxID)
	}
	if err != nil {
		return nil, fmt.Errorf("list chats: %w", err)
	}
	defer rows.Close()

	var chats []Chat
	var chatIDs, gChatIDs []int64
	gChatByChat := map[int64]int64{}
	for rows.Next() {
		var c Chat
		var gChatID int64
		var name sql.NullString
		var createdAt, lastMessageAt sql.NullTime
		if err := rows.Scan(&c.ChatID, &gChatID, &c.Muted, &c.NotificationPreference,
			&c.Type, &name, &createdAt, &lastMessageAt); err != nil {
			return nil, fmt.Errorf("scan chat: %w", err)
		}
		if lastMessageAt.Valid {
			c.LastMessageAt = rfc3339(lastMessageAt.Time)
		}
		if name.Valid {
			v := name.String
			c.Name = &v
		}
		if createdAt.Valid {
			c.CreatedAt = rfc3339(createdAt.Time)
		}
		c.Participants = []ParticipantRef{}
		chats = append(chats, c)
		chatIDs = append(chatIDs, c.ChatID)
		gChatIDs = append(gChatIDs, gChatID)
		gChatByChat[c.ChatID] = gChatID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list chats: %w", err)
	}
	if len(chats) == 0 {
		return []Chat{}, nil
	}

	inC := store.Placeholders(len(chatIDs))
	unread, err := store.SelectCounts(ctx, s.db,
		"SELECT `chat`.`id`, COUNT(`chat_message`.`id`) FROM `chat_message` "+
			"LEFT JOIN `chat` ON `chat_message`.`chat_id` = `chat`.`id` "+
			"WHERE `chat`.`customer_id` = ? AND `chat`.`id` IN ("+inC+") AND `chat_message`.`read_at` IS NULL "+
			"GROUP BY `chat`.`id`",
		append([]any{customerID}, store.Args(chatIDs)...)...)
	if err != nil {
		return nil, fmt.Errorf("count unread chat messages: %w", err)
	}

	participants, err := s.participantsByChat(ctx, customerID, gChatIDs)
	if err != nil {
		return nil, err
	}
	for i := range chats {
		chats[i].UnreadCount = unread[chats[i].ChatID]
		if p := participants[gChatByChat[chats[i].ChatID]]; p != nil {
			chats[i].Participants = p
		}
	}
	return chats, nil
}

func (s *Service) participantsByChat(ctx context.Context, customerID int64, gChatIDs []int64) (map[int64][]ParticipantRef, error) {
	if len(gChatIDs) == 0 {
		return map[int64][]ParticipantRef{}, nil
	}
	in := store.Placeholders(len(gChatIDs))
	rows, err := s.db.QueryContext(ctx,
		"SELECT `g_chat_participant`.`g_chat_id`, `g_chat_participant`.`email`, "+
			"`g_chat_participant`.`role`, `contact`.`name` FROM `g_chat_participant` "+
			"LEFT JOIN `contact` ON `g_chat_participant`.`email` = `contact`.`email` "+
			"AND `contact`.`customer_id` = ? "+
			"WHERE `g_chat_participant`.`g_chat_id` IN ("+in+") AND `g_chat_participant`.`left_at` IS NULL",
		append([]any{customerID}, store.Args(gChatIDs)...)...)
	if err != nil {
		return nil, fmt.Errorf("list chat participants: %w", err)
	}
	defer rows.Close()

	out := map[int64][]ParticipantRef{}
	for rows.Next() {
		var gChatID int64
		var p ParticipantRef
		var name sql.NullString
		if err := rows.Scan(&gChatID, &p.Email, &p.Role, &name); err != nil {
			return nil, fmt.Errorf("scan participant: %w", err)
		}
		p.Name = name.String
		out[gChatID] = append(out[gChatID], p)
	}
	return out, rows.Err()
}

// AddChat creates a chat and gives every participant a projection.
func (s *Service) AddChat(ctx context.Context, customerID, mailboxID int64,
	chatType string, name *string, participants []string) (int64, error) {
	var chatID int64
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		creator, err := store.SelectString(ctx, tx,
			"SELECT `email` FROM `alias` WHERE `customer_id` = ? AND `mailbox_id` = ? LIMIT 1",
			customerID, mailboxID)
		if err != nil {
			return fmt.Errorf("look up creator alias: %w", err)
		}
		if creator == "" {
			return nil
		}
		gChatID, err := store.Insert(ctx, tx,
			"INSERT INTO `g_chat` (`type`, `name`, `created_by_email`) VALUES (?,?,?)",
			chatType, name, creator)
		if err != nil {
			return fmt.Errorf("create chat: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO `g_chat_participant` (`g_chat_id`, `email`, `role`) VALUES (?,?,'creator')",
			gChatID, creator); err != nil {
			return fmt.Errorf("add creator: %w", err)
		}
		chatID, err = store.Insert(ctx, tx,
			"INSERT INTO `chat` (`customer_id`, `mailbox_id`, `g_chat_id`) VALUES (?,?,?)",
			customerID, mailboxID, gChatID)
		if err != nil {
			return fmt.Errorf("create chat projection: %w", err)
		}
		for _, email := range participants {
			if email == "" || email == creator {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO `g_chat_participant` (`g_chat_id`, `email`, `role`) VALUES (?,?,'member')",
				gChatID, email); err != nil {
				return fmt.Errorf("add participant %s: %w", email, err)
			}
			if err := projectChat(ctx, tx, gChatID, email); err != nil {
				return err
			}
		}
		return nil
	})
	return chatID, err
}

// projectChat gives an address its own view of a chat, when it has a mailbox.
func projectChat(ctx context.Context, tx *sql.Tx, gChatID int64, email string) error {
	var participantCustomerID, mailboxID int64
	err := tx.QueryRowContext(ctx,
		"SELECT `mailbox`.`customer_id`, `mailbox`.`id` FROM `alias` "+
			"JOIN `mailbox` ON `mailbox`.`id` = `alias`.`mailbox_id` "+
			"WHERE `alias`.`email` = ? LIMIT 1", email).Scan(&participantCustomerID, &mailboxID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("look up participant mailbox: %w", err)
	}
	existing, err := store.SelectInt(ctx, tx,
		"SELECT `id` FROM `chat` WHERE `customer_id` = ? AND `g_chat_id` = ? LIMIT 1",
		participantCustomerID, gChatID)
	if err != nil {
		return fmt.Errorf("look up chat projection: %w", err)
	}
	if existing != 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO `chat` (`customer_id`, `mailbox_id`, `g_chat_id`) VALUES (?,?,?)",
		participantCustomerID, mailboxID, gChatID); err != nil {
		return fmt.Errorf("create chat projection: %w", err)
	}
	return nil
}

// EditChat renames a chat or changes the caller's notification settings.
func (s *Service) EditChat(ctx context.Context, customerID, chatID int64,
	name *string, muted *bool, preference *string) (bool, error) {
	return true, s.db.Tx(ctx, func(tx *sql.Tx) error {
		if name != nil {
			gChatID, err := store.SelectInt(ctx, tx,
				"SELECT `g_chat_id` FROM `chat` WHERE `customer_id` = ? AND `id` = ? LIMIT 1",
				customerID, chatID)
			if err != nil {
				return fmt.Errorf("look up chat: %w", err)
			}
			if gChatID == 0 {
				return nil
			}
			if _, err := tx.ExecContext(ctx,
				"UPDATE `g_chat` SET `name` = ? WHERE `id` = ?", *name, gChatID); err != nil {
				return fmt.Errorf("rename chat: %w", err)
			}
		}
		if muted != nil {
			if _, err := tx.ExecContext(ctx,
				"UPDATE `chat` SET `muted` = ? WHERE `customer_id` = ? AND `id` = ?",
				*muted, customerID, chatID); err != nil {
				return fmt.Errorf("mute chat: %w", err)
			}
		}
		if preference != nil {
			if _, err := tx.ExecContext(ctx,
				"UPDATE `chat` SET `notification_preference` = ? WHERE `customer_id` = ? AND `id` = ?",
				*preference, customerID, chatID); err != nil {
				return fmt.Errorf("set notification preference: %w", err)
			}
		}
		return nil
	})
}

// DeleteChat hides a chat from the caller. Other participants keep theirs.
func (s *Service) DeleteChat(ctx context.Context, customerID, chatID int64) (bool, error) {
	n, err := store.Affected(ctx, s.db,
		"UPDATE `chat` SET `deleted_at` = NOW() WHERE `customer_id` = ? AND `id` = ? LIMIT 1",
		customerID, chatID)
	if err != nil {
		return false, fmt.Errorf("delete chat: %w", err)
	}
	return n > 0, nil
}

// ListParticipants returns the active members of a chat with their presence.
func (s *Service) ListParticipants(ctx context.Context, customerID, chatID int64) ([]Participant, error) {
	gChatID, err := store.SelectInt(ctx, s.db,
		"SELECT `g_chat_id` FROM `chat` WHERE `customer_id` = ? AND `id` = ? LIMIT 1", customerID, chatID)
	if err != nil {
		return nil, fmt.Errorf("look up chat: %w", err)
	}
	if gChatID == 0 {
		return []Participant{}, nil
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT `g_chat_participant`.`id`, `g_chat_participant`.`email`, `g_chat_participant`.`role`, "+
			"`g_chat_participant`.`joined_at`, `g_chat_participant`.`left_at`, `contact`.`name`, "+
			"`presence_status`.`status` FROM `g_chat_participant` "+
			"LEFT JOIN `contact` ON `g_chat_participant`.`email` = `contact`.`email` "+
			"AND `contact`.`customer_id` = ? "+
			"LEFT JOIN `user` ON `user`.`username` = `g_chat_participant`.`email` "+
			"LEFT JOIN `presence_status` ON `presence_status`.`user_id` = `user`.`id` "+
			"WHERE `g_chat_participant`.`g_chat_id` = ? AND `g_chat_participant`.`left_at` IS NULL "+
			"ORDER BY `g_chat_participant`.`id`",
		customerID, gChatID)
	if err != nil {
		return nil, fmt.Errorf("list participants: %w", err)
	}
	defer rows.Close()

	participants := []Participant{}
	for rows.Next() {
		var p Participant
		var joinedAt sql.NullTime
		var leftAt sql.NullTime
		var name, status sql.NullString
		if err := rows.Scan(&p.ParticipantID, &p.Email, &p.Role, &joinedAt, &leftAt, &name, &status); err != nil {
			return nil, fmt.Errorf("scan participant: %w", err)
		}
		p.Name = name.String
		p.Status = status.String
		if p.Status == "" {
			p.Status = "offline"
		}
		if joinedAt.Valid {
			p.JoinedAt = rfc3339(joinedAt.Time)
		}
		p.LeftAt = rfc3339Ptr(leftAt)
		participants = append(participants, p)
	}
	return participants, rows.Err()
}

// AddParticipant adds an address to a chat.
func (s *Service) AddParticipant(ctx context.Context, customerID, chatID int64, email, role string) (int64, error) {
	var participantID int64
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		gChatID, err := store.SelectInt(ctx, tx,
			"SELECT `g_chat_id` FROM `chat` WHERE `customer_id` = ? AND `id` = ? LIMIT 1", customerID, chatID)
		if err != nil {
			return fmt.Errorf("look up chat: %w", err)
		}
		if gChatID == 0 {
			return nil
		}
		participantID, err = store.Insert(ctx, tx,
			"INSERT INTO `g_chat_participant` (`g_chat_id`, `email`, `role`) VALUES (?,?,?)",
			gChatID, email, role)
		if err != nil {
			return fmt.Errorf("add participant: %w", err)
		}
		return projectChat(ctx, tx, gChatID, email)
	})
	return participantID, err
}

// RemoveParticipant marks a member as having left. Their history stays.
func (s *Service) RemoveParticipant(ctx context.Context, customerID, participantID int64) (bool, error) {
	// The participant must be in a chat the caller can see.
	n, err := store.Affected(ctx, s.db,
		"UPDATE `g_chat_participant` SET `left_at` = NOW() WHERE `id` = ? AND `left_at` IS NULL "+
			"AND `g_chat_id` IN (SELECT `g_chat_id` FROM `chat` WHERE `customer_id` = ?)",
		participantID, customerID)
	if err != nil {
		return false, fmt.Errorf("remove participant: %w", err)
	}
	return n > 0, nil
}

// ListMessages returns a chat's messages, newest first.
func (s *Service) ListMessages(ctx context.Context, customerID, chatID int64, search string) ([]Message, error) {
	var rows *sql.Rows
	var err error
	if search != "" {
		rows, err = s.db.QueryContext(ctx,
			"SELECT `chat_message`.`id`, `chat_message`.`delivered_at`, `chat_message`.`read_at`, "+
				"`g_chat_message`.`id`, `g_chat_message`.`sender_email`, `g_chat_message`.`sender_name`, "+
				"`g_chat_message`.`body`, `g_chat_message`.`body_html`, `g_chat_message`.`created_at`, "+
				"`g_chat_message`.`edited_at`, `g_chat_message`.`deleted_at` FROM `chat_message` "+
				"LEFT JOIN `g_chat_message` ON `chat_message`.`g_chat_message_id` = `g_chat_message`.`id` "+
				"WHERE `chat_message`.`customer_id` = ? AND `chat_message`.`chat_id` = ? "+
				"AND `g_chat_message`.`deleted_at` IS NULL AND "+
				"(MATCH(`g_chat_message`.`body`) AGAINST (? IN BOOLEAN MODE) "+
				"OR MATCH(`g_chat_message`.`sender_name`) AGAINST (? IN BOOLEAN MODE)) "+
				"ORDER BY `chat_message`.`id` DESC",
			customerID, chatID, search, search)
	} else {
		rows, err = s.db.QueryContext(ctx,
			"SELECT `chat_message`.`id`, `chat_message`.`delivered_at`, `chat_message`.`read_at`, "+
				"`g_chat_message`.`id`, `g_chat_message`.`sender_email`, `g_chat_message`.`sender_name`, "+
				"`g_chat_message`.`body`, `g_chat_message`.`body_html`, `g_chat_message`.`created_at`, "+
				"`g_chat_message`.`edited_at`, `g_chat_message`.`deleted_at` FROM `chat_message` "+
				"LEFT JOIN `g_chat_message` ON `chat_message`.`g_chat_message_id` = `g_chat_message`.`id` "+
				"WHERE `chat_message`.`customer_id` = ? AND `chat_message`.`chat_id` = ? "+
				"AND `g_chat_message`.`deleted_at` IS NULL ORDER BY `chat_message`.`id` DESC",
			customerID, chatID)
	}
	if err != nil {
		return nil, fmt.Errorf("list chat messages: %w", err)
	}
	defer rows.Close()

	var messages []Message
	var gMessageIDs []int64
	gByMessage := map[int64]int64{}
	for rows.Next() {
		var m Message
		var gMessageID int64
		var bodyHTML sql.NullString
		var createdAt sql.NullTime
		var editedAt, deletedAt, deliveredAt, readAt sql.NullTime
		if err := rows.Scan(&m.MessageID, &deliveredAt, &readAt, &gMessageID, &m.SenderEmail,
			&m.SenderName, &m.Body, &bodyHTML, &createdAt, &editedAt, &deletedAt); err != nil {
			return nil, fmt.Errorf("scan chat message: %w", err)
		}
		m.BodyHTML = bodyHTML.String
		if createdAt.Valid {
			m.CreatedAt = rfc3339(createdAt.Time)
		}
		m.EditedAt = rfc3339Ptr(editedAt)
		m.DeletedAt = rfc3339Ptr(deletedAt)
		m.DeliveredAt = rfc3339Ptr(deliveredAt)
		m.ReadAt = rfc3339Ptr(readAt)
		m.Attachments = []AttachmentRef{}
		messages = append(messages, m)
		gMessageIDs = append(gMessageIDs, gMessageID)
		gByMessage[m.MessageID] = gMessageID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list chat messages: %w", err)
	}
	if len(messages) == 0 {
		return []Message{}, nil
	}

	gChatID, err := store.SelectInt(ctx, s.db,
		"SELECT `g_chat_id` FROM `chat` WHERE `customer_id` = ? AND `id` = ? LIMIT 1", customerID, chatID)
	if err != nil {
		return nil, fmt.Errorf("look up chat: %w", err)
	}
	names, err := store.SelectPairs(ctx, s.db,
		"SELECT `g_chat_message`.`sender_email`, `contact`.`name` FROM `g_chat_message` "+
			"LEFT JOIN `contact` ON `g_chat_message`.`sender_email` = `contact`.`email` "+
			"AND `contact`.`customer_id` = ? WHERE `g_chat_message`.`g_chat_id` = ?",
		customerID, gChatID)
	if err != nil {
		return nil, fmt.Errorf("list sender names: %w", err)
	}

	attachments, err := s.attachmentsByMessage(ctx, gMessageIDs)
	if err != nil {
		return nil, err
	}
	statuses, err := s.deliveryStatuses(ctx, gMessageIDs)
	if err != nil {
		return nil, err
	}

	for i := range messages {
		if name := names[messages[i].SenderEmail]; name != "" {
			messages[i].SenderName = name
		}
		gID := gByMessage[messages[i].MessageID]
		if refs := attachments[gID]; refs != nil {
			messages[i].Attachments = refs
		}
		if st, ok := statuses[gID]; ok {
			s := st
			messages[i].DeliveryStatus = &s
		}
	}
	return messages, nil
}

func (s *Service) attachmentsByMessage(ctx context.Context, gMessageIDs []int64) (map[int64][]AttachmentRef, error) {
	in := store.Placeholders(len(gMessageIDs))
	rows, err := s.db.QueryContext(ctx,
		"SELECT `g_chat_message_id`, `id`, `file_name`, `mime_type`, `file_size` "+
			"FROM `g_chat_attachment` WHERE `g_chat_message_id` IN ("+in+") ORDER BY `id`",
		store.Args(gMessageIDs)...)
	if err != nil {
		return nil, fmt.Errorf("list chat attachments: %w", err)
	}
	defer rows.Close()

	out := map[int64][]AttachmentRef{}
	for rows.Next() {
		var gMessageID int64
		var a AttachmentRef
		if err := rows.Scan(&gMessageID, &a.AttachmentID, &a.FileName, &a.MimeType, &a.FileSize); err != nil {
			return nil, fmt.Errorf("scan chat attachment: %w", err)
		}
		out[gMessageID] = append(out[gMessageID], a)
	}
	return out, rows.Err()
}

// deliveryStatuses counts, per canonical message, how many recipients it
// reached and how many have read it.
func (s *Service) deliveryStatuses(ctx context.Context, gMessageIDs []int64) (map[int64]DeliveryStatus, error) {
	in := store.Placeholders(len(gMessageIDs))
	rows, err := s.db.QueryContext(ctx,
		"SELECT `g_chat_message_id`, COUNT(*), "+
			"COUNT(CASE WHEN `delivered_at` IS NOT NULL THEN 1 END), "+
			"COUNT(CASE WHEN `read_at` IS NOT NULL THEN 1 END) "+
			"FROM `chat_message` WHERE `g_chat_message_id` IN ("+in+") GROUP BY `g_chat_message_id`",
		store.Args(gMessageIDs)...)
	if err != nil {
		return nil, fmt.Errorf("count delivery status: %w", err)
	}
	defer rows.Close()

	out := map[int64]DeliveryStatus{}
	for rows.Next() {
		var gMessageID int64
		var st DeliveryStatus
		if err := rows.Scan(&gMessageID, &st.Sent, &st.Delivered, &st.Read); err != nil {
			return nil, fmt.Errorf("scan delivery status: %w", err)
		}
		out[gMessageID] = st
	}
	return out, rows.Err()
}

// AddMessage sends a message to every active participant of a chat.
func (s *Service) AddMessage(ctx context.Context, customerID, chatID int64,
	body, bodyHTML string, attachments []NewAttachment) (int64, error) {
	var messageID int64
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		var gChatID, mailboxID int64
		err := tx.QueryRowContext(ctx,
			"SELECT `g_chat_id`, `mailbox_id` FROM `chat` WHERE `customer_id` = ? AND `id` = ? LIMIT 1",
			customerID, chatID).Scan(&gChatID, &mailboxID)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return fmt.Errorf("look up chat: %w", err)
		}
		var senderEmail, senderName string
		err = tx.QueryRowContext(ctx,
			"SELECT `email`, `name` FROM `alias` WHERE `customer_id` = ? AND `mailbox_id` = ? LIMIT 1",
			customerID, mailboxID).Scan(&senderEmail, &senderName)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return fmt.Errorf("look up sender alias: %w", err)
		}

		participants, err := store.SelectStrings(ctx, tx,
			"SELECT `email` FROM `g_chat_participant` WHERE `g_chat_id` = ? AND `left_at` IS NULL", gChatID)
		if err != nil {
			return fmt.Errorf("list participants: %w", err)
		}
		if !contains(participants, senderEmail) {
			return nil
		}

		gMessageID, err := store.Insert(ctx, tx,
			"INSERT INTO `g_chat_message` (`g_chat_id`, `sender_email`, `sender_name`, `body`, `body_html`) "+
				"VALUES (?,?,?,?,?)",
			gChatID, senderEmail, senderName, body, nullable(bodyHTML))
		if err != nil {
			return fmt.Errorf("create chat message: %w", err)
		}
		for _, a := range attachments {
			hash, err := s.blob.Put(a.FileData)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO `g_chat_attachment` (`g_chat_message_id`, `file_name`, `mime_type`, "+
					"`file_size`, `hash_md5`) VALUES (?,?,?,?,?)",
				gMessageID, a.FileName, a.MimeType, len(a.FileData), hash); err != nil {
				return fmt.Errorf("create chat attachment: %w", err)
			}
		}

		for _, email := range participants {
			var participantCustomerID int64
			err := tx.QueryRowContext(ctx,
				"SELECT `customer_id` FROM `alias` WHERE `email` = ? LIMIT 1", email).Scan(&participantCustomerID)
			if err == sql.ErrNoRows {
				continue
			}
			if err != nil {
				return fmt.Errorf("look up participant customer: %w", err)
			}
			participantChatID, err := store.SelectInt(ctx, tx,
				"SELECT `id` FROM `chat` WHERE `customer_id` = ? AND `g_chat_id` = ? LIMIT 1",
				participantCustomerID, gChatID)
			if err != nil {
				return fmt.Errorf("look up participant chat: %w", err)
			}
			if participantChatID == 0 {
				continue
			}
			projectionID, err := store.Insert(ctx, tx,
				"INSERT INTO `chat_message` (`customer_id`, `chat_id`, `g_chat_message_id`, `delivered_at`) "+
					"VALUES (?,?,?,NOW())",
				participantCustomerID, participantChatID, gMessageID)
			if err != nil {
				return fmt.Errorf("create chat message projection: %w", err)
			}
			status := "delivered"
			if email == senderEmail {
				status = "read"
				if _, err := tx.ExecContext(ctx,
					"UPDATE `chat_message` SET `read_at` = NOW() WHERE `id` = ?", projectionID); err != nil {
					return fmt.Errorf("mark own message read: %w", err)
				}
				if _, err := tx.ExecContext(ctx,
					"UPDATE `chat` SET `last_read_message_id` = ? WHERE `id` = ?",
					gMessageID, participantChatID); err != nil {
					return fmt.Errorf("advance read marker: %w", err)
				}
				messageID = projectionID
			}
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO `message_status` (`g_chat_message_id`, `recipient_email`, `status`, `timestamp`) "+
					"VALUES (?,?,?,NOW())", gMessageID, email, status); err != nil {
				return fmt.Errorf("record message status: %w", err)
			}
		}
		return nil
	})
	return messageID, err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// EditMessage rewrites a message the caller sent, within the edit window.
func (s *Service) EditMessage(ctx context.Context, customerID, messageID int64, body, bodyHTML string) (bool, error) {
	gMessageID, err := s.editableMessage(ctx, customerID, messageID)
	if err != nil || gMessageID == 0 {
		return false, err
	}
	n, err := store.Affected(ctx, s.db,
		"UPDATE `g_chat_message` SET `body` = ?, `body_html` = ?, `edited_at` = NOW() WHERE `id` = ?",
		body, nullable(bodyHTML), gMessageID)
	if err != nil {
		return false, fmt.Errorf("edit chat message: %w", err)
	}
	return n > 0, nil
}

// DeleteMessage marks a message the caller sent as deleted, within the edit
// window. The body is retained for audit.
func (s *Service) DeleteMessage(ctx context.Context, customerID, messageID int64) (bool, error) {
	gMessageID, err := s.editableMessage(ctx, customerID, messageID)
	if err != nil || gMessageID == 0 {
		return false, err
	}
	n, err := store.Affected(ctx, s.db,
		"UPDATE `g_chat_message` SET `deleted_at` = NOW() WHERE `id` = ?", gMessageID)
	if err != nil {
		return false, fmt.Errorf("delete chat message: %w", err)
	}
	return n > 0, nil
}

// editableMessage returns the canonical id of a message the caller may still
// change, or 0. It checks both authorship and the edit window.
func (s *Service) editableMessage(ctx context.Context, customerID, messageID int64) (int64, error) {
	var gMessageID int64
	var senderEmail string
	var createdAt time.Time
	err := s.db.QueryRowContext(ctx,
		"SELECT `g_chat_message`.`id`, `g_chat_message`.`sender_email`, `g_chat_message`.`created_at` "+
			"FROM `chat_message` "+
			"JOIN `g_chat_message` ON `chat_message`.`g_chat_message_id` = `g_chat_message`.`id` "+
			"WHERE `chat_message`.`customer_id` = ? AND `chat_message`.`id` = ? LIMIT 1",
		customerID, messageID).Scan(&gMessageID, &senderEmail, &createdAt)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("look up chat message: %w", err)
	}
	if time.Since(createdAt.UTC()) > EditWindow {
		return 0, nil
	}
	// Only the author may edit, which the PHP does not check.
	owns, err := store.SelectInt(ctx, s.db,
		"SELECT `id` FROM `alias` WHERE `customer_id` = ? AND `email` = ? LIMIT 1", customerID, senderEmail)
	if err != nil {
		return 0, fmt.Errorf("verify message author: %w", err)
	}
	if owns == 0 {
		return 0, nil
	}
	return gMessageID, nil
}

// GetAttachment returns a chat attachment the caller can see.
func (s *Service) GetAttachment(ctx context.Context, customerID, attachmentID int64) (*AttachmentData, error) {
	var a AttachmentData
	var hash string
	err := s.db.QueryRowContext(ctx,
		"SELECT `g_chat_attachment`.`file_name`, `g_chat_attachment`.`mime_type`, "+
			"`g_chat_attachment`.`file_size`, `g_chat_attachment`.`hash_md5` FROM `g_chat_attachment` "+
			"JOIN `chat_message` ON `chat_message`.`g_chat_message_id` = `g_chat_attachment`.`g_chat_message_id` "+
			"WHERE `g_chat_attachment`.`id` = ? AND `chat_message`.`customer_id` = ? LIMIT 1",
		attachmentID, customerID).Scan(&a.FileName, &a.MimeType, &a.FileSize, &hash)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look up chat attachment: %w", err)
	}
	data, err := s.blob.Get(hash)
	if err != nil {
		return nil, err
	}
	a.FileData = data
	return &a, nil
}

// GetPresence returns the presence of the given users within the tenant.
func (s *Service) GetPresence(ctx context.Context, customerID int64, userIDs []int64) ([]Presence, error) {
	if len(userIDs) == 0 {
		return []Presence{}, nil
	}
	in := store.Placeholders(len(userIDs))
	rows, err := s.db.QueryContext(ctx,
		"SELECT `user_id`, `status`, `last_activity_at` FROM `presence_status` "+
			"WHERE `customer_id` = ? AND `user_id` IN ("+in+")",
		append([]any{customerID}, store.Args(userIDs)...)...)
	if err != nil {
		return nil, fmt.Errorf("get presence: %w", err)
	}
	defer rows.Close()

	known := map[int64]Presence{}
	for rows.Next() {
		var p Presence
		var lastActivity sql.NullTime
		if err := rows.Scan(&p.UserID, &p.Status, &lastActivity); err != nil {
			return nil, fmt.Errorf("scan presence: %w", err)
		}
		if lastActivity.Valid {
			p.LastActivityAt = rfc3339(lastActivity.Time)
			// A user who stopped reporting is stale, so derive the current
			// status rather than trusting a value nobody refreshed.
			p.Status = derive(p.Status, lastActivity.Time)
		}
		known[p.UserID] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get presence: %w", err)
	}

	out := make([]Presence, 0, len(userIDs))
	for _, id := range userIDs {
		if p, ok := known[id]; ok {
			out = append(out, p)
			continue
		}
		out = append(out, Presence{UserID: id, Status: "offline"})
	}
	return out, nil
}

// derive ages a stored status: away after 10 idle minutes, offline after 15.
func derive(status string, lastActivity time.Time) string {
	if status == "offline" {
		return status
	}
	idle := time.Since(lastActivity.UTC())
	switch {
	case idle > 15*time.Minute:
		return "offline"
	case idle > 10*time.Minute:
		return "away"
	}
	return status
}

// SetPresence records the caller's availability.
func (s *Service) SetPresence(ctx context.Context, customerID, userID int64, status string) (bool, error) {
	switch status {
	case "online", "offline", "away":
	default:
		return false, nil
	}
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO `presence_status` (`customer_id`, `user_id`, `status`, `last_activity_at`) "+
			"VALUES (?,?,?,NOW()) ON DUPLICATE KEY UPDATE "+
			"`status` = VALUES(`status`), `last_activity_at` = VALUES(`last_activity_at`)",
		customerID, userID, status)
	if err != nil {
		return false, fmt.Errorf("set presence: %w", err)
	}
	return true, nil
}

// GetTyping returns the unexpired typing indicators for a chat, excluding the
// caller's own.
func (s *Service) GetTyping(ctx context.Context, customerID, userID, chatID int64) ([]TypingIndicator, error) {
	gChatID, err := store.SelectInt(ctx, s.db,
		"SELECT `g_chat_id` FROM `chat` WHERE `customer_id` = ? AND `id` = ? LIMIT 1", customerID, chatID)
	if err != nil {
		return nil, fmt.Errorf("look up chat: %w", err)
	}
	if gChatID == 0 {
		return []TypingIndicator{}, nil
	}
	// Expired rows are removed on read, which keeps the table from growing
	// without a separate sweeper.
	if _, err := s.db.ExecContext(ctx,
		"DELETE FROM `typing_indicator` WHERE `expires_at` <= NOW()"); err != nil {
		return nil, fmt.Errorf("expire typing indicators: %w", err)
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT `typing_indicator`.`user_id`, `user`.`name`, `typing_indicator`.`started_at`, "+
			"`typing_indicator`.`expires_at` FROM `typing_indicator` "+
			"LEFT JOIN `user` ON `user`.`id` = `typing_indicator`.`user_id` "+
			"WHERE `typing_indicator`.`g_chat_id` = ? AND `typing_indicator`.`user_id` <> ? "+
			"AND `typing_indicator`.`expires_at` > NOW()",
		gChatID, userID)
	if err != nil {
		return nil, fmt.Errorf("get typing indicators: %w", err)
	}
	defer rows.Close()

	out := []TypingIndicator{}
	for rows.Next() {
		var t TypingIndicator
		var name sql.NullString
		var started, expires sql.NullTime
		if err := rows.Scan(&t.UserID, &name, &started, &expires); err != nil {
			return nil, fmt.Errorf("scan typing indicator: %w", err)
		}
		t.UserName = name.String
		if started.Valid {
			t.StartedAt = rfc3339(started.Time)
		}
		if expires.Valid {
			t.ExpiresAt = rfc3339(expires.Time)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// StartTyping starts or extends the caller's typing indicator.
func (s *Service) StartTyping(ctx context.Context, customerID, userID, chatID int64) (bool, error) {
	gChatID, err := store.SelectInt(ctx, s.db,
		"SELECT `g_chat_id` FROM `chat` WHERE `customer_id` = ? AND `id` = ? LIMIT 1", customerID, chatID)
	if err != nil {
		return false, fmt.Errorf("look up chat: %w", err)
	}
	if gChatID == 0 {
		return false, nil
	}
	seconds := int(TypingTTL / time.Second)
	n, err := store.Affected(ctx, s.db,
		"UPDATE `typing_indicator` SET `expires_at` = DATE_ADD(NOW(), INTERVAL ? SECOND) "+
			"WHERE `customer_id` = ? AND `g_chat_id` = ? AND `user_id` = ?",
		seconds, customerID, gChatID, userID)
	if err != nil {
		return false, fmt.Errorf("extend typing indicator: %w", err)
	}
	if n > 0 {
		return true, nil
	}
	if _, err := s.db.ExecContext(ctx,
		"INSERT INTO `typing_indicator` (`customer_id`, `g_chat_id`, `user_id`, `expires_at`) "+
			"VALUES (?,?,?, DATE_ADD(NOW(), INTERVAL ? SECOND))",
		customerID, gChatID, userID, seconds); err != nil {
		return false, fmt.Errorf("start typing indicator: %w", err)
	}
	return true, nil
}

// StopTyping clears the caller's typing indicator.
func (s *Service) StopTyping(ctx context.Context, customerID, userID, chatID int64) (bool, error) {
	gChatID, err := store.SelectInt(ctx, s.db,
		"SELECT `g_chat_id` FROM `chat` WHERE `customer_id` = ? AND `id` = ? LIMIT 1", customerID, chatID)
	if err != nil {
		return false, fmt.Errorf("look up chat: %w", err)
	}
	if gChatID == 0 {
		return false, nil
	}
	if _, err := s.db.ExecContext(ctx,
		"DELETE FROM `typing_indicator` WHERE `customer_id` = ? AND `g_chat_id` = ? AND `user_id` = ?",
		customerID, gChatID, userID); err != nil {
		return false, fmt.Errorf("stop typing indicator: %w", err)
	}
	return true, nil
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
