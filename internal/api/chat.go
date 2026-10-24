package api

import (
	"net/http"

	"github.com/103mail/webmail-server/internal/chat"
)

// handleChat implements /api/v1/chat/{entity}/{id}.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request, req *request) (any, int) {
	if !req.Identity.Authenticated() {
		return nil, http.StatusUnauthorized
	}
	customerID := req.Identity.CustomerID
	userID := req.Identity.UserID
	ctx := r.Context()
	search := req.str("search")

	switch req.Entity {
	case "session":
		// Minting a ClientId is the one authenticated step of opening a socket.
		// Everything after it is addressed by that opaque id, which is why it
		// must never be a user id or a token (ADR 13, ADR 18).
		if req.Method != http.MethodPost {
			return nil, http.StatusMethodNotAllowed
		}
		out, err := s.chat.NewSession(ctx, customerID, userID)
		return s.result(w, r, out, err)

	case "chat":
		id := parseID(req.ID)
		if id == 0 {
			return nil, http.StatusBadRequest
		}
		switch req.Method {
		case http.MethodGet:
			out, err := s.chat.ListChats(ctx, customerID, id, search)
			return s.result(w, r, out, err)
		case http.MethodPost:
			chatType := req.str("type")
			if chatType != "direct" && chatType != "group" {
				return nil, http.StatusBadRequest
			}
			participants, ok := req.strings("participants")
			if !ok {
				return nil, http.StatusBadRequest
			}
			// A direct chat is exactly two people; a group needs a name.
			name := req.strPtr("name")
			if chatType == "group" && (name == nil || *name == "") {
				return nil, http.StatusBadRequest
			}
			if chatType == "direct" {
				if len(participants) != 1 {
					return nil, http.StatusBadRequest
				}
				name = nil
			}
			out, err := s.chat.AddChat(ctx, customerID, id, chatType, name, participants)
			return s.result(w, r, out, err)
		case http.MethodPut:
			preference := req.strPtr("notificationPreference")
			if preference != nil {
				switch *preference {
				case "all", "mentions_only", "none":
				default:
					return nil, http.StatusBadRequest
				}
			}
			out, err := s.chat.EditChat(ctx, customerID, id, req.strPtr("name"),
				req.boolPtr("muted"), preference)
			return s.result(w, r, out, err)
		case http.MethodDelete:
			out, err := s.chat.DeleteChat(ctx, customerID, id)
			return s.result(w, r, out, err)
		}
		return nil, http.StatusMethodNotAllowed

	case "participant":
		id := parseID(req.ID)
		if id == 0 {
			return nil, http.StatusBadRequest
		}
		switch req.Method {
		case http.MethodGet:
			out, err := s.chat.ListParticipants(ctx, customerID, id)
			return s.result(w, r, out, err)
		case http.MethodPost:
			address := req.str("email")
			if address == "" {
				return nil, http.StatusBadRequest
			}
			role := req.str("role")
			if role == "" {
				role = "member"
			}
			switch role {
			case "creator", "admin", "member":
			default:
				return nil, http.StatusBadRequest
			}
			out, err := s.chat.AddParticipant(ctx, customerID, id, address, role)
			return s.result(w, r, out, err)
		case http.MethodDelete:
			out, err := s.chat.RemoveParticipant(ctx, customerID, id)
			return s.result(w, r, out, err)
		}
		return nil, http.StatusMethodNotAllowed

	case "read":
		// What the socket's read frame does, for a client that has no socket.
		id := parseID(req.ID)
		if id == 0 {
			return nil, http.StatusBadRequest
		}
		if req.Method != http.MethodPut {
			return nil, http.StatusMethodNotAllowed
		}
		if err := s.chat.MarkChatRead(ctx, customerID, id); err != nil {
			s.fail(w, r, err)
			return nil, 0
		}
		return true, http.StatusOK

	case "message":
		id := parseID(req.ID)
		if id == 0 {
			return nil, http.StatusBadRequest
		}
		switch req.Method {
		case http.MethodGet:
			out, err := s.chat.ListMessages(ctx, customerID, id, search)
			return s.result(w, r, out, err)
		case http.MethodPost:
			body := req.str("body")
			if body == "" {
				return nil, http.StatusBadRequest
			}
			files, ok := req.attachments()
			if !ok {
				return nil, http.StatusBadRequest
			}
			out, err := s.chat.AddMessage(ctx, customerID, id, body, req.str("bodyHtml"),
				toChatAttachments(files))
			return s.result(w, r, out, err)
		case http.MethodPut:
			body := req.str("body")
			if body == "" {
				return nil, http.StatusBadRequest
			}
			out, err := s.chat.EditMessage(ctx, customerID, id, body, req.str("bodyHtml"))
			return s.result(w, r, out, err)
		case http.MethodDelete:
			out, err := s.chat.DeleteMessage(ctx, customerID, id)
			return s.result(w, r, out, err)
		}
		return nil, http.StatusMethodNotAllowed

	case "attachment":
		id := parseID(req.ID)
		if id == 0 {
			return nil, http.StatusBadRequest
		}
		if req.Method != http.MethodGet {
			return nil, http.StatusMethodNotAllowed
		}
		out, err := s.chat.GetAttachment(ctx, customerID, id)
		if err != nil {
			s.fail(w, r, err)
			return nil, 0
		}
		if out == nil {
			return nil, http.StatusNotFound
		}
		return out, http.StatusOK

	case "presence":
		switch req.Method {
		case http.MethodGet:
			userIDs, ok := parseIDs(req.ID)
			if !ok {
				return nil, http.StatusBadRequest
			}
			out, err := s.chat.GetPresence(ctx, customerID, userIDs)
			return s.result(w, r, out, err)
		case http.MethodPut:
			target := parseID(req.ID)
			if target == 0 {
				return nil, http.StatusBadRequest
			}
			if target != userID {
				return nil, http.StatusForbidden
			}
			status := req.str("status")
			switch status {
			case "online", "offline", "away":
			default:
				return nil, http.StatusBadRequest
			}
			out, err := s.chat.SetPresence(ctx, customerID, userID, status)
			return s.result(w, r, out, err)
		}
		return nil, http.StatusMethodNotAllowed

	case "typing":
		chatID := parseID(req.ID)
		if chatID == 0 {
			return nil, http.StatusBadRequest
		}
		switch req.Method {
		case http.MethodGet:
			out, err := s.chat.GetTyping(ctx, customerID, userID, chatID)
			return s.result(w, r, out, err)
		case http.MethodPost:
			out, err := s.chat.StartTyping(ctx, customerID, userID, chatID)
			return s.result(w, r, out, err)
		case http.MethodDelete:
			out, err := s.chat.StopTyping(ctx, customerID, userID, chatID)
			return s.result(w, r, out, err)
		}
		return nil, http.StatusMethodNotAllowed
	}
	return nil, http.StatusNotFound
}

func toChatAttachments(files []attachment) []chat.NewAttachment {
	out := make([]chat.NewAttachment, len(files))
	for i, f := range files {
		out[i] = chat.NewAttachment{FileName: f.FileName, MimeType: f.MimeType, FileData: f.FileData}
	}
	return out
}
