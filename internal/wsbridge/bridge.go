package wsbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mevdschee/webmail-server/internal/chat"
)

// Service is the bridge between ws2api and the chat domain.
type Service struct {
	chat *chat.Service
	log  *slog.Logger

	// pushURL is where the proxy tier listens. Empty disables fan-out: a
	// message still reaches the database and its sender, but the other
	// participants do not see it until they reload. A deployment without chat
	// is legitimate, so this warns rather than failing.
	pushURL string
	client  *http.Client
}

// PushTimeout bounds one fan-out post. It is short because the proxy is on the
// same network and either has the connection or does not.
const PushTimeout = 5 * time.Second

// New returns a Service. It reads WEBMAIL_WS_PUSH_URL for the proxy address.
func New(c *chat.Service, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	pushURL := strings.TrimSuffix(os.Getenv("WEBMAIL_WS_PUSH_URL"), "/")
	if pushURL == "" {
		log.Warn("no WEBMAIL_WS_PUSH_URL; chat messages will not be pushed to other participants")
	}
	return &Service{
		chat:    c,
		log:     log,
		pushURL: pushURL,
		client: &http.Client{
			Timeout: PushTimeout,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 32,
			},
		},
	}
}

// Connect answers the proxy's upgrade check. It must return exactly "ok" for
// the connection to be accepted, and anything else refuses it with 403.
//
// This is where the socket is authenticated: the ClientId is resolved to the
// session that minted it, and claimed so it cannot open a second connection.
func (s *Service) Connect(ctx context.Context, clientID string) bool {
	session, err := s.chat.ClaimSession(ctx, clientID)
	if err != nil {
		if !chat.IsNoSession(err) {
			s.log.Error("could not claim a chat session", "error", err)
		}
		return false
	}
	if _, err := s.chat.SetPresence(ctx, session.CustomerID, session.UserID, "online"); err != nil {
		// Presence is not worth refusing a connection over.
		s.log.Warn("could not set presence online", "user", session.UserID, "error", err)
	}
	s.broadcastPresence(ctx, session, "online")
	s.log.Info("chat connected", "user", session.UserID)
	return true
}

// Disconnect handles the proxy's close notification. Presence goes offline
// here, which is what makes a clean disconnect immediate rather than waiting for
// the idle sweep to notice.
func (s *Service) Disconnect(ctx context.Context, clientID, reason string) {
	session, err := s.chat.LiveSession(ctx, clientID)
	if err == nil {
		if _, err := s.chat.SetPresence(ctx, session.CustomerID, session.UserID, "offline"); err != nil {
			s.log.Warn("could not set presence offline", "user", session.UserID, "error", err)
		}
		s.broadcastPresence(ctx, session, "offline")
		s.log.Info("chat disconnected", "user", session.UserID, "reason", reason)
	}
	if err := s.chat.CloseSession(ctx, clientID); err != nil {
		s.log.Error("could not close a chat session", "error", err)
	}
}

// Handle processes one client frame and returns what to write back on that same
// socket. An empty result means there is nothing to say, and the client ignores
// the empty frame the proxy writes.
func (s *Service) Handle(ctx context.Context, clientID string, body []byte) []byte {
	session, err := s.chat.LiveSession(ctx, clientID)
	if err != nil {
		// The socket is open but the session is not; the client should
		// reconnect rather than keep sending.
		return errorFrame("", "this session is no longer valid")
	}

	frame, err := Decode(body)
	if err != nil {
		return errorFrame("", err.Error())
	}

	switch frame.Type {
	case TypePing:
		return Encode(&Frame{Type: TypePong})
	case TypeHello:
		return s.hello(ctx, session)
	case TypeMessage:
		return s.message(ctx, session, frame)
	case TypeTyping:
		return s.typing(ctx, session, frame)
	case TypeRead:
		return s.read(ctx, session, frame)
	case TypePresence:
		return s.presence(ctx, session, frame)
	}
	return errorFrame(frame.ID, "unknown frame type")
}

func (s *Service) hello(ctx context.Context, session *chat.Session) []byte {
	return Encode(&Frame{Type: TypeReady, UserID: session.UserID})
}

// message posts to a chat and fans the result out to the other participants.
//
// The sender's own socket gets the acknowledgement as the response to this
// frame, so it is deliberately not in the fan-out; pushing to it as well would
// deliver the message twice.
func (s *Service) message(ctx context.Context, session *chat.Session, frame *Frame) []byte {
	chatID, err := strconv.ParseInt(frame.ChatID, 10, 64)
	if err != nil || chatID == 0 {
		return errorFrame(frame.ID, "chatId is required")
	}
	if strings.TrimSpace(frame.Body) == "" {
		return errorFrame(frame.ID, "body is required")
	}

	messageID, err := s.chat.AddMessage(ctx, session.CustomerID, chatID, frame.Body, "", nil)
	if err != nil {
		s.log.Error("could not add a chat message", "error", err)
		return errorFrame(frame.ID, "could not send the message")
	}
	if messageID == 0 {
		return errorFrame(frame.ID, "no such chat")
	}

	// The message as the REST API would return it, so the client has one
	// representation of a chat message rather than two.
	payload := s.messagePayload(ctx, session, chatID, messageID)
	s.fanOut(ctx, session, chatID, func(target chat.Target) *Frame {
		return &Frame{
			Type:    TypeMessage,
			ChatID:  strconv.FormatInt(target.ChatID, 10),
			Message: payload,
		}
	})
	return Encode(&Frame{
		Type:      TypeAck,
		ID:        frame.ID,
		ChatID:    frame.ChatID,
		MessageID: strconv.FormatInt(messageID, 10),
		Message:   payload,
	})
}

// messagePayload re-reads the message that was just written so the wire shape
// matches GET /api/v1/chat/message/{chatId} exactly. One extra query buys one
// representation instead of two that can drift.
func (s *Service) messagePayload(ctx context.Context, session *chat.Session,
	chatID, messageID int64) json.RawMessage {
	messages, err := s.chat.ListMessages(ctx, session.CustomerID, chatID, "")
	if err != nil {
		s.log.Error("could not read back a chat message", "error", err)
		return nil
	}
	for i := range messages {
		if messages[i].MessageID == messageID {
			raw, err := json.Marshal(messages[i])
			if err != nil {
				return nil
			}
			return raw
		}
	}
	return nil
}

func (s *Service) typing(ctx context.Context, session *chat.Session, frame *Frame) []byte {
	chatID, err := strconv.ParseInt(frame.ChatID, 10, 64)
	if err != nil || chatID == 0 {
		return errorFrame(frame.ID, "chatId is required")
	}
	active := on(frame.On)
	if active {
		_, err = s.chat.StartTyping(ctx, session.CustomerID, session.UserID, chatID)
	} else {
		_, err = s.chat.StopTyping(ctx, session.CustomerID, session.UserID, chatID)
	}
	if err != nil {
		s.log.Error("could not record typing", "error", err)
		return nil
	}
	s.fanOut(ctx, session, chatID, func(target chat.Target) *Frame {
		return &Frame{
			Type:   TypeTyping,
			ChatID: strconv.FormatInt(target.ChatID, 10),
			UserID: session.UserID,
			On:     &active,
		}
	})
	// Nothing to tell the sender: they know they are typing.
	return nil
}

func (s *Service) read(ctx context.Context, session *chat.Session, frame *Frame) []byte {
	chatID, err := strconv.ParseInt(frame.ChatID, 10, 64)
	if err != nil || chatID == 0 {
		return errorFrame(frame.ID, "chatId is required")
	}
	if err := s.chat.MarkChatRead(ctx, session.CustomerID, chatID); err != nil {
		s.log.Error("could not mark a chat read", "error", err)
		return errorFrame(frame.ID, "could not mark the chat read")
	}
	return Encode(&Frame{Type: TypeAck, ID: frame.ID, ChatID: frame.ChatID})
}

func (s *Service) presence(ctx context.Context, session *chat.Session, frame *Frame) []byte {
	switch frame.Status {
	case "online", "away", "offline":
	default:
		return errorFrame(frame.ID, "status must be online, away or offline")
	}
	if _, err := s.chat.SetPresence(ctx, session.CustomerID, session.UserID, frame.Status); err != nil {
		s.log.Error("could not set presence", "error", err)
		return errorFrame(frame.ID, "could not set presence")
	}
	s.broadcastPresence(ctx, session, frame.Status)
	return nil
}

// fanOut sends a frame to every open session of a chat's participants except
// the one that caused it.
//
// The frame is built per recipient, because a chat id is per-customer: the same
// conversation is a different id to each participant, and a frame carrying the
// sender's would be about a chat the recipient does not have.
//
// N posts, one per session, because a stateless API tier has no shared
// connection state to broadcast over. That is the cost ADR 13 accepted, and it
// buys fan-out that is naturally parallel with no participant able to block
// another.
func (s *Service) fanOut(ctx context.Context, session *chat.Session, chatID int64,
	build func(chat.Target) *Frame) {
	if s.pushURL == "" {
		return
	}
	gChatID, err := s.chat.GChatID(ctx, session.CustomerID, chatID)
	if err != nil || gChatID == 0 {
		return
	}
	targets, err := s.chat.SessionsForChat(ctx, gChatID)
	if err != nil {
		s.log.Error("could not list sessions for a chat", "error", err)
		return
	}

	var wg sync.WaitGroup
	for _, target := range targets {
		if target.ClientID == session.ClientID {
			// The sender already has this as the response to their own frame.
			continue
		}
		wg.Add(1)
		go func(target chat.Target) {
			defer wg.Done()
			s.push(ctx, target.ClientID, Encode(build(target)))
		}(target)
	}
	wg.Wait()
}

func (s *Service) broadcastPresence(ctx context.Context, session *chat.Session, status string) {
	if s.pushURL == "" {
		return
	}
	clientIDs, err := s.chat.SessionsForPeers(ctx, session.CustomerID)
	if err != nil {
		s.log.Error("could not list peer sessions", "error", err)
		return
	}
	// The address as well as the user id: a chat lists its participants by
	// address, so an id alone is something the client cannot join to anything.
	email, err := s.chat.PrimaryAlias(ctx, session.CustomerID)
	if err != nil {
		s.log.Warn("could not resolve a presence address", "error", err)
	}
	s.pushAll(ctx, clientIDs, session.ClientID, &Frame{
		Type:   TypePresence,
		UserID: session.UserID,
		Email:  email,
		Status: status,
	})
}

// pushAll posts a frame to each session in parallel. One slow proxy node must
// not hold up the others, which is the same rule the federation outbox follows.
func (s *Service) pushAll(ctx context.Context, clientIDs []string, except string, frame *Frame) {
	body := Encode(frame)
	var wg sync.WaitGroup
	for _, clientID := range clientIDs {
		if clientID == except {
			continue
		}
		wg.Add(1)
		go func(clientID string) {
			defer wg.Done()
			s.push(ctx, clientID, body)
		}(clientID)
	}
	wg.Wait()
}

// push writes one frame to one connection through the proxy.
//
// A 404 means the proxy no longer holds that connection, which is how this
// server learns a session is gone without anyone having to tell it. The row is
// closed there and then so the next fan-out does not try it again.
func (s *Service) push(ctx context.Context, clientID string, body []byte) {
	url := s.pushURL + "/" + clientID
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		s.log.Error("could not build a push request", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		s.log.Warn("could not push to a chat client", "error", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))

	if resp.StatusCode == http.StatusNotFound {
		if err := s.chat.CloseSession(ctx, clientID); err != nil {
			s.log.Error("could not close a vanished session", "error", err)
		}
		return
	}
	if resp.StatusCode != http.StatusOK {
		s.log.Warn("a push was refused", "status", resp.StatusCode)
	}
}

// SweepSessions closes sessions that went quiet and deletes old closed rows.
// Exported for the worker; the idle window has to be longer than the client's
// ping interval or a healthy connection would be swept.
func (s *Service) SweepSessions(ctx context.Context) error {
	if err := s.chat.SweepSessions(ctx, 5*time.Minute, time.Hour); err != nil {
		return fmt.Errorf("sweep chat sessions: %w", err)
	}
	return nil
}
