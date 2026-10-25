package federation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// ChatVersion is the protocol version that carries the chat extension. A
// Domain lists it only when it implements all of RFC 14 (RFC 14.1).
const ChatVersion = "0.3"

// The kinds of envelope version 0.3 adds. A mail envelope has no kind, or
// "mail" (RFC 14.2).
const (
	KindMail     = "mail"
	KindChat     = "chat"
	KindChatRead = "chat_read"
)

// Outbox kinds: mail rows are "envelope", which they were before chat existed.
// The chat loop drains the other two (RFC 14.4).
const (
	outboxMail     = "envelope"
	outboxChat     = "chat"
	outboxChatRead = "chat_read"
)

// Retry schedule for chat envelopes, shorter than mail's because a chat
// message is worth the most while the conversation is going on (RFC 14.4).
const (
	ChatRetryBase   = time.Second
	ChatRetryCap    = time.Minute
	ChatRetryBudget = time.Hour
)

// ChatEnvelope is a chat message, or a read receipt, between Domains (RFC
// 14.2, 14.7). It shares its outer fields with Envelope and carries a chat
// object where a mail envelope carries a thread.
type ChatEnvelope struct {
	Version      string `json:"version"`
	Kind         string `json:"kind"`
	EnvelopeID   string `json:"envelope_id"`
	OriginDomain string `json:"origin_domain"`
	OriginSentAt string `json:"origin_sent_at"`
	// Thread is read only to refuse an envelope that carries one.
	Thread      json.RawMessage `json:"thread,omitempty"`
	Chat        Chat            `json:"chat"`
	Message     *ChatMessage    `json:"message,omitempty"`
	Attachments *[]Attachment   `json:"attachments,omitempty"`
	Read        *ChatRead       `json:"read,omitempty"`
	Recipients  []string        `json:"recipients"`
}

// Chat identifies the conversation. A group is known by its federation id; a
// direct chat by its two members, whatever id either side gave it (RFC 14.3).
type Chat struct {
	FederationID string   `json:"federation_id"`
	Type         string   `json:"type"`
	Name         *string  `json:"name,omitempty"`
	Members      []string `json:"members"`
	Left         []string `json:"left,omitempty"`
	Refused      []string `json:"refused,omitempty"`
}

// ChatMessage is Message with the one field chat adds: history marks an
// earlier message sent to someone who just joined (RFC 14.5).
type ChatMessage struct {
	Message
	History bool `json:"history,omitempty"`
}

// ChatRead says how far one member has read a chat (RFC 14.7).
type ChatRead struct {
	ReaderEmail string `json:"reader_email"`
	UpTo        string `json:"up_to"`
	ReadAt      string `json:"read_at"`
}

// Validate checks the field rules of RFC 14.2 and 14.7, and those of RFC 5.3
// for the fields both kinds share.
func (e *ChatEnvelope) Validate(maxAttachmentBytes int64) *Rejection {
	if e.Version != ChatVersion {
		return reject(400, ErrUnsupportedVersion, "a chat envelope is version "+ChatVersion)
	}
	if !validUUID(e.EnvelopeID) {
		return reject(400, ErrSchemaViolation, "envelope_id is not a UUID")
	}
	if e.OriginDomain == "" || e.OriginDomain != strings.ToLower(e.OriginDomain) {
		return reject(400, ErrSchemaViolation, "origin_domain must be a lowercased DNS name")
	}
	if _, err := ParseTimestamp(e.OriginSentAt); err != nil {
		return reject(400, ErrSchemaViolation, "origin_sent_at is not an RFC 3339 UTC timestamp")
	}
	if len(e.Thread) > 0 && string(e.Thread) != "null" {
		return reject(400, ErrSchemaViolation, "a chat envelope carries no thread")
	}
	if r := e.Chat.validate(); r != nil {
		return r
	}

	if len(e.Recipients) == 0 || len(e.Recipients) > maxRecipients {
		return reject(400, ErrSchemaViolation, "recipients must hold between 1 and 100 addresses")
	}
	for _, address := range e.Recipients {
		if !validAddress(address) {
			return reject(400, ErrSchemaViolation, "recipient is not an address: "+address)
		}
	}

	switch e.Kind {
	case KindChat:
		if e.Read != nil {
			return reject(400, ErrSchemaViolation, "a chat message carries no read")
		}
		return e.validateMessage(maxAttachmentBytes)
	case KindChatRead:
		if e.Message != nil || e.Attachments != nil {
			return reject(400, ErrSchemaViolation, "a read receipt carries no message or attachments")
		}
		return e.validateRead()
	}
	return reject(400, ErrSchemaViolation, "kind must be chat or chat_read")
}

func (c *Chat) validate() *Rejection {
	if !validUUID(c.FederationID) {
		return reject(400, ErrSchemaViolation, "chat.federation_id is not a UUID")
	}
	switch c.Type {
	case "direct":
		if c.Name != nil {
			return reject(400, ErrSchemaViolation, "a direct chat has no name")
		}
		if len(c.Members) != 2 {
			return reject(400, ErrSchemaViolation, "a direct chat has exactly two members")
		}
	case "group":
		if c.Name == nil || *c.Name == "" || len(*c.Name) > maxSenderNameBytes {
			return reject(400, ErrSchemaViolation, "a group chat has a name of at most 255 bytes")
		}
		if len(c.Members) == 0 || len(c.Members) > maxMembers {
			return reject(400, ErrSchemaViolation, "chat.members must hold between 1 and 1000 addresses")
		}
	default:
		return reject(400, ErrSchemaViolation, "chat.type must be direct or group")
	}
	for _, address := range c.Members {
		if !validAddress(address) {
			return reject(400, ErrSchemaViolation, "member is not an address: "+address)
		}
	}
	for field, list := range map[string][]string{"chat.refused": c.Refused, "chat.left": c.Left} {
		if len(list) > maxRecipients {
			return reject(400, ErrSchemaViolation, field+" holds more than 100 addresses")
		}
		for _, address := range list {
			if !validAddress(address) {
				return reject(400, ErrSchemaViolation, field+" entry is not an address: "+address)
			}
			if containsFold(c.Members, address) {
				return reject(400, ErrSchemaViolation, address+" is in "+field+" and a member")
			}
		}
	}
	return nil
}

func (e *ChatEnvelope) validateMessage(maxAttachmentBytes int64) *Rejection {
	m := e.Message
	if m == nil {
		return reject(400, ErrSchemaViolation, "a chat envelope carries a message")
	}
	if !validUUID(m.FederationID) {
		return reject(400, ErrSchemaViolation, "message.federation_id is not a UUID")
	}
	if len(m.SenderEmail) > maxAddressBytes || !validAddress(m.SenderEmail) {
		return reject(400, ErrSchemaViolation, "message.sender_email is not an address")
	}
	if len(m.SenderName) > maxSenderNameBytes {
		return reject(400, ErrSchemaViolation, "message.sender_name exceeds 255 bytes")
	}
	if len(m.Body) > maxBodyBytes {
		return reject(400, ErrSchemaViolation, "message.body exceeds 1 MiB")
	}
	if len(m.BodyHTML) > maxBodyHTMLBytes {
		return reject(400, ErrSchemaViolation, "message.body_html exceeds 4 MiB")
	}
	if _, err := ParseTimestamp(m.SentAt); err != nil {
		return reject(400, ErrSchemaViolation, "message.sent_at is not an RFC 3339 UTC timestamp")
	}
	if e.Attachments != nil {
		if len(*e.Attachments) > maxAttachments {
			return reject(400, ErrSchemaViolation, "more than 64 attachments")
		}
		for i := range *e.Attachments {
			if r := (*e.Attachments)[i].validate(maxAttachmentBytes); r != nil {
				return r
			}
		}
	}
	// The sender is a member, unless they are leaving with this message or it
	// is earlier history from someone who has since left (RFC 14.5). Every
	// recipient is a member, as for a thread (RFC 5.3).
	required := e.Recipients
	leaving := containsFold(e.Chat.Left, m.SenderEmail)
	if !IsMailerDaemon(m.SenderEmail) && !leaving && !m.History {
		required = append([]string{m.SenderEmail}, required...)
	}
	for _, address := range required {
		if !containsFold(e.Chat.Members, address) {
			return reject(400, ErrSchemaViolation, "chat.members leaves out "+address)
		}
	}
	return nil
}

func (e *ChatEnvelope) validateRead() *Rejection {
	r := e.Read
	if r == nil {
		return reject(400, ErrSchemaViolation, "a read receipt carries read")
	}
	if !validAddress(r.ReaderEmail) || !strings.EqualFold(DomainOf(r.ReaderEmail), e.OriginDomain) {
		return reject(400, ErrSchemaViolation, "read.reader_email is not an address on "+e.OriginDomain)
	}
	if !validUUID(r.UpTo) {
		return reject(400, ErrSchemaViolation, "read.up_to is not a UUID")
	}
	if _, err := ParseTimestamp(r.ReadAt); err != nil {
		return reject(400, ErrSchemaViolation, "read.read_at is not an RFC 3339 UTC timestamp")
	}
	return nil
}

// Author is who speaks in the envelope: the writer of a message, or the
// reader of a receipt. Its domain must be the sending Domain (RFC 9.6).
func (e *ChatEnvelope) Author() string {
	if e.Read != nil {
		return e.Read.ReaderEmail
	}
	if e.Message != nil {
		return e.Message.SenderEmail
	}
	return ""
}

// RecipientsOn checks that every recipient is on the Destination's own domain,
// as for a mail envelope (RFC 5.3).
func (e *ChatEnvelope) RecipientsOn(domain string) *Rejection {
	for _, address := range e.Recipients {
		if !strings.EqualFold(DomainOf(address), domain) {
			return reject(400, ErrNotRegistrar, "recipient "+address+" is not on "+domain)
		}
	}
	return nil
}

func containsFold(list []string, address string) bool {
	return slices.ContainsFunc(list, func(a string) bool { return strings.EqualFold(a, address) })
}

func lowercase(list []string) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}

// ChatDeliverer writes accepted chat envelopes into the chat domain, and
// tells it how its own envelopes fared. Declared here and satisfied by
// *chat.Service, so this package never imports that one.
type ChatDeliverer interface {
	// DeliverChat writes a message for its local recipients. It returns how
	// many it delivered to and the ones that have no account here (RFC 6.3).
	DeliverChat(ctx context.Context, in ChatInbound) (int, []string, error)
	// UndeliveredChat lists the local recipients of an accepted envelope that
	// have no copy of its message, to answer a replay the same way (RFC 5.4).
	UndeliveredChat(ctx context.Context, in ChatInbound) ([]string, error)
	// ReceiveRead records how far a member on another Domain has read.
	ReceiveRead(ctx context.Context, in ChatReadInbound) error
	// ChatDelivered records that a peer accepted a message for recipients
	// (RFC 14.7). gMessageID is the local message the envelope carried.
	ChatDelivered(ctx context.Context, gMessageID int64, recipients []string) error
	// BounceChat tells the sender, in the chat, that a message did not reach
	// some members. It is local and never leaves this Server (RFC 14.1, 6.5).
	BounceChat(ctx context.Context, b ChatBounce) error
}

// ChatInbound is an accepted chat message, decoded and checked.
type ChatInbound struct {
	OriginDomain string
	ChatFedID    string
	Type         string
	Name         *string
	Members      []string
	Left         []string
	Refused      []string
	MessageFedID string
	SenderEmail  string
	SenderName   string
	Body         string
	BodyHTML     string
	SentAt       time.Time
	History      bool
	Recipients   []string
	Attachments  []InboundAttachment
}

// ChatReadInbound is an accepted read receipt.
type ChatReadInbound struct {
	OriginDomain string
	ChatFedID    string
	Type         string
	Members      []string
	ReaderEmail  string
	UpTo         string
	ReadAt       time.Time
	Recipients   []string
}

// ChatBounce is a chat message that did not reach some of its recipients.
type ChatBounce struct {
	GMessageID int64
	ChatFedID  string
	Recipients []string
	PeerDomain string
	HTTPStatus int
	ErrorCode  string
	// Refused means the recipients do not exist at the peer, or it does not
	// do chat, so they are not members. Backfill means the message timed out
	// and is sent again when the peer answers.
	Refused  bool
	Backfill bool
}

// ChatOutbound is one chat envelope, or one receipt, for one peer.
type ChatOutbound struct {
	PeerDomain   string
	OriginDomain string
	Kind         string
	ChatFedID    string
	Type         string
	Name         *string
	Members      []string
	Left         []string
	Refused      []string
	Recipients   []string

	MessageFedID string
	SenderEmail  string
	SenderName   string
	Body         string
	BodyHTML     string
	SentAt       time.Time
	History      bool
	Attachments  []OutboundAttachment

	ReaderEmail string
	UpTo        string
	ReadAt      time.Time

	// RelatedID is the local g_chat_message the envelope carries.
	RelatedID int64
}

// EnqueueChat writes a chat envelope to the outbox inside the caller's
// transaction, the same write-before-send rule as Enqueue. The chat loop
// sends it within a second (RFC 14.4).
func (s *Service) EnqueueChat(ctx context.Context, tx *sql.Tx, out ChatOutbound) error {
	if !s.Enabled() {
		return fmt.Errorf("federation is not configured")
	}
	envelopeID, err := NewUUID()
	if err != nil {
		return err
	}
	kind := out.Kind
	if kind == "" {
		kind = KindChat
	}
	envelope := ChatEnvelope{
		Version:      ChatVersion,
		Kind:         kind,
		EnvelopeID:   envelopeID,
		OriginDomain: out.OriginDomain,
		OriginSentAt: FormatTimestamp(s.db.Now()),
		Chat: Chat{
			FederationID: out.ChatFedID,
			Type:         out.Type,
			Name:         out.Name,
			Members:      out.Members,
			Left:         out.Left,
			Refused:      out.Refused,
		},
		Recipients: out.Recipients,
	}
	if out.Type == "direct" {
		envelope.Chat.Name = nil
	}
	outboxKind := outboxChat
	switch kind {
	case KindChat:
		envelope.Message = &ChatMessage{
			Message: Message{
				FederationID: out.MessageFedID,
				SenderEmail:  out.SenderEmail,
				SenderName:   out.SenderName,
				Body:         out.Body,
				BodyHTML:     out.BodyHTML,
				SentAt:       FormatTimestamp(out.SentAt),
			},
			History: out.History,
		}
		attachments := make([]Attachment, 0, len(out.Attachments))
		for _, a := range out.Attachments {
			attachments = append(attachments, encodeAttachment(a))
		}
		envelope.Attachments = &attachments
	case KindChatRead:
		outboxKind = outboxChatRead
		envelope.Read = &ChatRead{
			ReaderEmail: out.ReaderEmail,
			UpTo:        out.UpTo,
			ReadAt:      FormatTimestamp(out.ReadAt),
		}
	default:
		return fmt.Errorf("unknown chat envelope kind %q", kind)
	}

	body, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("serialize chat envelope: %w", err)
	}
	var relatedID any
	if out.RelatedID != 0 {
		relatedID = out.RelatedID
	}
	now := s.db.Now()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO "federation_outbox" ("kind", "peer_domain", "origin_domain", "body", `+
			`"idempotency_key", "related_id", "status", "next_retry_at", "created_at") `+
			"VALUES (?,?,?,?,?,?, 'pending', ?, ?)",
		outboxKind, out.PeerDomain, out.OriginDomain, string(body), envelopeID, relatedID, now, now); err != nil {
		return fmt.Errorf("enqueue chat envelope for %s: %w", out.PeerDomain, err)
	}
	return nil
}

// receiveChat is Receive for an envelope of kind chat or chat_read, from the
// point where the request is authenticated and its nonce claimed.
func (s *Service) receiveChat(ctx context.Context, req *Request) (*Accepted, *Rejection) {
	if s.chats == nil {
		return nil, reject(400, ErrUnsupportedKind, "this Domain does not do chat")
	}
	var envelope ChatEnvelope
	if err := json.Unmarshal(req.Body, &envelope); err != nil {
		return nil, reject(400, ErrMalformedEnvelope, "envelope is not JSON")
	}
	if r := envelope.Validate(s.cfg.MaxAttachmentBytes); r != nil {
		return nil, r
	}
	// The triple check of RFC 9.6, with the reader of a receipt in the role
	// of the sender: a Domain speaks only for its own users.
	if !strings.EqualFold(envelope.OriginDomain, req.Sender) {
		return nil, reject(401, ErrSenderMismatch, "origin_domain does not match X-Federation-Sender")
	}
	if !strings.EqualFold(DomainOf(envelope.Author()), req.Sender) {
		return nil, reject(401, ErrSenderMismatch, "the author is not on the sending Domain")
	}
	if r := envelope.RecipientsOn(req.Destination); r != nil {
		return nil, r
	}

	seen, err := s.alreadyAccepted(ctx, req.Sender, envelope.EnvelopeID)
	if err != nil {
		return nil, reject(500, ErrServerError, err.Error())
	}

	if envelope.Kind == KindChatRead {
		if seen {
			return accepted(envelope.EnvelopeID, nil), nil
		}
		if r := s.rateLimited(ctx, req.Sender); r != nil {
			return nil, r
		}
		in, r := toReadInbound(&envelope)
		if r != nil {
			return nil, r
		}
		err := s.chats.ReceiveRead(ctx, *in)
		if errors.Is(err, ErrNotMember) {
			return nil, reject(http.StatusTooEarly, ErrNotAMember, err.Error())
		}
		if err != nil {
			s.log.Error("inbound read receipt failed", "sender", req.Sender,
				"envelope", envelope.EnvelopeID, "error", err)
			return nil, reject(500, ErrServerError, "could not record the receipt")
		}
		s.recordChatAccepted(ctx, req.Sender, envelope.EnvelopeID)
		return accepted(envelope.EnvelopeID, nil), nil
	}

	in, r := toChatInbound(&envelope)
	if r != nil {
		return nil, r
	}
	if seen {
		refused, err := s.chats.UndeliveredChat(ctx, *in)
		if err != nil {
			return nil, reject(500, ErrServerError, err.Error())
		}
		return accepted(envelope.EnvelopeID, refused), nil
	}
	if r := s.rateLimited(ctx, req.Sender); r != nil {
		return nil, r
	}
	delivered, refused, err := s.chats.DeliverChat(ctx, *in)
	if errors.Is(err, ErrNotMember) {
		return nil, reject(http.StatusTooEarly, ErrNotAMember, err.Error())
	}
	if err != nil {
		s.log.Error("inbound chat delivery failed", "sender", req.Sender,
			"envelope", envelope.EnvelopeID, "error", err)
		return nil, reject(500, ErrServerError, "could not store the message")
	}
	if delivered == 0 && len(refused) > 0 {
		return nil, reject(404, ErrNoRecipients, "no recipient is known here")
	}
	s.recordChatAccepted(ctx, req.Sender, envelope.EnvelopeID)
	s.log.Info("chat envelope accepted", "sender", req.Sender,
		"envelope", envelope.EnvelopeID, "recipients", delivered, "refused", len(refused))
	return accepted(envelope.EnvelopeID, refused), nil
}

func (s *Service) recordChatAccepted(ctx context.Context, sender, envelopeID string) {
	if err := s.recordAccepted(ctx, sender, envelopeID); err != nil {
		s.log.Error("could not record an accepted envelope",
			"sender", sender, "envelope", envelopeID, "error", err)
	}
}

func toChatInbound(e *ChatEnvelope) (*ChatInbound, *Rejection) {
	sentAt, err := ParseTimestamp(e.Message.SentAt)
	if err != nil {
		return nil, reject(400, ErrSchemaViolation, "message.sent_at is not a timestamp")
	}
	in := &ChatInbound{
		OriginDomain: e.OriginDomain,
		ChatFedID:    e.Chat.FederationID,
		Type:         e.Chat.Type,
		Name:         e.Chat.Name,
		Members:      withoutMailerDaemon(lowercase(e.Chat.Members)),
		Left:         lowercase(e.Chat.Left),
		Refused:      lowercase(e.Chat.Refused),
		MessageFedID: e.Message.FederationID,
		SenderEmail:  strings.ToLower(e.Message.SenderEmail),
		SenderName:   e.Message.SenderName,
		Body:         e.Message.Body,
		BodyHTML:     e.Message.BodyHTML,
		SentAt:       sentAt,
		History:      e.Message.History,
		Recipients:   lowercase(e.Recipients),
	}
	if e.Attachments != nil {
		for i := range *e.Attachments {
			a := &(*e.Attachments)[i]
			data, r := a.Bytes()
			if r != nil {
				return nil, r
			}
			in.Attachments = append(in.Attachments, InboundAttachment{
				FederationID: a.FederationID, FileName: a.FileName, MimeType: a.MimeType, Data: data,
			})
		}
	}
	return in, nil
}

func toReadInbound(e *ChatEnvelope) (*ChatReadInbound, *Rejection) {
	readAt, err := ParseTimestamp(e.Read.ReadAt)
	if err != nil {
		return nil, reject(400, ErrSchemaViolation, "read.read_at is not a timestamp")
	}
	return &ChatReadInbound{
		OriginDomain: e.OriginDomain,
		ChatFedID:    e.Chat.FederationID,
		Type:         e.Chat.Type,
		Members:      withoutMailerDaemon(lowercase(e.Chat.Members)),
		ReaderEmail:  strings.ToLower(e.Read.ReaderEmail),
		UpTo:         e.Read.UpTo,
		ReadAt:       readAt,
		Recipients:   lowercase(e.Recipients),
	}, nil
}

func withoutMailerDaemon(list []string) []string {
	return slices.DeleteFunc(list, IsMailerDaemon)
}

// afterChatAttempt tells the chat domain what became of a chat envelope: who
// has the message, and who it did not reach. A receipt that fails is only
// logged; the next one says everything it did (RFC 14.7).
func (s *Service) afterChatAttempt(ctx context.Context, row outboxRow, httpStatus int, code string,
	refused []Refusal, delivered, failed, backfill bool) {
	if s.chats == nil || row.Kind != outboxChat {
		return
	}
	var envelope ChatEnvelope
	if err := json.Unmarshal(row.Body, &envelope); err != nil {
		s.log.Error("could not read a chat envelope after sending it", "id", row.ID, "error", err)
		return
	}
	// History is not tracked for receipts (RFC 14.7), and its sender is not
	// waiting on it.
	if envelope.Message != nil && envelope.Message.History {
		return
	}
	if delivered {
		var refusedAddresses []string
		for _, r := range refused {
			if containsFold(envelope.Recipients, r.Address) {
				refusedAddresses = append(refusedAddresses, r.Address)
			}
		}
		got := slices.DeleteFunc(slices.Clone(envelope.Recipients), func(a string) bool {
			return containsFold(refusedAddresses, a)
		})
		if row.RelatedID != 0 && len(got) > 0 {
			if err := s.chats.ChatDelivered(ctx, row.RelatedID, got); err != nil {
				s.log.Error("could not record chat delivery", "envelope", row.EnvelopeID, "error", err)
			}
		}
		if len(refusedAddresses) == 0 {
			return
		}
		s.bounceChat(ctx, row, envelope, ChatBounce{Recipients: refusedAddresses, HTTPStatus: httpStatus,
			ErrorCode: ErrUnknownRecipient, Refused: true})
		return
	}
	if failed || backfill {
		s.bounceChat(ctx, row, envelope, ChatBounce{Recipients: envelope.Recipients, HTTPStatus: httpStatus,
			ErrorCode: code, Refused: failed && refusesMembers(httpStatus), Backfill: backfill})
	}
}

// refusesMembers reports whether a permanent failure means the recipients
// are not members: they do not exist there, or their Domain does not do chat
// or does not take envelopes from us (RFC 14.1, 6.5).
func refusesMembers(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return false
}

func (s *Service) bounceChat(ctx context.Context, row outboxRow, envelope ChatEnvelope, b ChatBounce) {
	b.GMessageID = row.RelatedID
	b.ChatFedID = envelope.Chat.FederationID
	b.PeerDomain = row.PeerDomain
	if err := s.chats.BounceChat(ctx, b); err != nil {
		s.log.Error("could not write a chat bounce", "envelope", row.EnvelopeID, "error", err)
	}
}

// envelopeVersion reads the version a stored envelope was written in, which
// is the version it is sent and signed as.
func envelopeVersion(body []byte) string {
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &v); err != nil || v.Version == "" {
		return Version
	}
	return v.Version
}

// isChatKind reports whether an outbox kind is drained by the chat loop.
func isChatKind(kind string) bool { return kind == outboxChat || kind == outboxChatRead }
