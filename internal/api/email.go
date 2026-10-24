package api

import (
	"errors"
	"net/http"

	"github.com/103mail/webmail-server/internal/chat"
	"github.com/103mail/webmail-server/internal/email"
)

// handleEmail implements /api/v1/email/{entity}/{id}.
func (s *Server) handleEmail(w http.ResponseWriter, r *http.Request, req *request) (any, int) {
	if !req.Identity.Authenticated() {
		return nil, http.StatusUnauthorized
	}
	customerID := req.Identity.CustomerID
	ctx := r.Context()
	search := req.str("search")

	switch req.Entity {
	case "mailbox":
		// GET and POST address the customer; PUT and DELETE address a mailbox.
		if req.Method == http.MethodGet || req.Method == http.MethodPost {
			id := parseID(req.ID)
			if id == 0 {
				return nil, http.StatusBadRequest
			}
			if id != customerID {
				return nil, http.StatusNotFound
			}
		} else {
			mailboxID := parseID(req.ID)
			if mailboxID == 0 {
				return nil, http.StatusBadRequest
			}
			exists, err := s.email.MailboxExists(ctx, customerID, mailboxID)
			if err != nil {
				s.fail(w, r, err)
				return nil, 0
			}
			if !exists {
				return nil, http.StatusNotFound
			}
		}
		switch req.Method {
		case http.MethodGet:
			out, err := s.email.ListMailboxes(ctx, customerID, search)
			return s.result(w, r, out, err)
		case http.MethodPost:
			senderEmail, senderName := req.str("senderEmail"), req.str("senderName")
			if senderEmail == "" || senderName == "" {
				// Fall back to the default mailbox's identity.
				mailboxes, err := s.email.ListMailboxes(ctx, customerID, "")
				if err != nil {
					s.fail(w, r, err)
					return nil, 0
				}
				for _, m := range mailboxes {
					if m.IsDefault {
						if senderEmail == "" {
							senderEmail = m.SenderEmail
						}
						if senderName == "" {
							senderName = m.SenderName
						}
						break
					}
				}
			}
			color := req.str("color")
			if color == "" {
				color = "red"
			}
			out, err := s.email.AddMailbox(ctx, customerID, req.str("name"), color, senderEmail, senderName)
			return s.result(w, r, out, err)
		case http.MethodPut:
			out, err := s.email.EditMailbox(ctx, customerID, req.Identity.UserID, parseID(req.ID),
				req.str("name"), req.str("color"), req.boolPtr("isDefault"), req.boolPtr("isShared"),
				req.str("senderEmail"), req.str("senderName"))
			return s.result(w, r, out, err)
		case http.MethodDelete:
			out, err := s.email.DeleteMailbox(ctx, customerID, parseID(req.ID))
			return s.result(w, r, out, err)
		}
		return nil, http.StatusMethodNotAllowed

	case "folder":
		var mailboxID int64
		if req.Method == http.MethodGet || req.Method == http.MethodPost {
			mailboxID = parseID(req.ID)
			if mailboxID == 0 {
				return nil, http.StatusBadRequest
			}
			exists, err := s.email.MailboxExists(ctx, customerID, mailboxID)
			if err != nil {
				s.fail(w, r, err)
				return nil, 0
			}
			if !exists {
				return nil, http.StatusNotFound
			}
		} else {
			folderID := parseID(req.ID)
			if folderID == 0 {
				return nil, http.StatusBadRequest
			}
			var err error
			mailboxID, err = s.email.MailboxIDForFolder(ctx, customerID, folderID)
			if err != nil {
				s.fail(w, r, err)
				return nil, 0
			}
			if mailboxID == 0 {
				return nil, http.StatusNotFound
			}
		}
		switch req.Method {
		case http.MethodGet:
			out, err := s.email.ListFolders(ctx, customerID, mailboxID, search)
			return s.result(w, r, out, err)
		case http.MethodPost:
			name := req.str("name")
			if name == "" || len(name) > 255 {
				return nil, http.StatusBadRequest
			}
			out, err := s.email.AddFolder(ctx, customerID, mailboxID, name)
			return s.result(w, r, out, err)
		case http.MethodPut:
			out, err := s.email.EditFolder(ctx, customerID, parseID(req.ID),
				req.str("name"), req.boolean("isDefault"))
			return s.result(w, r, out, err)
		case http.MethodDelete:
			out, err := s.email.DeleteFolder(ctx, customerID, parseID(req.ID))
			return s.result(w, r, out, err)
		}
		return nil, http.StatusMethodNotAllowed

	case "thread":
		id := parseID(req.ID)
		if id == 0 {
			return nil, http.StatusBadRequest
		}
		switch req.Method {
		case http.MethodGet:
			out, err := s.email.ListThreads(ctx, customerID, id, search)
			return s.result(w, r, out, err)
		case http.MethodPost:
			subject := req.str("subject")
			if subject == "" {
				return nil, http.StatusBadRequest
			}
			subscribers, ok := req.strings("subscribers")
			if !ok {
				return nil, http.StatusBadRequest
			}
			files, ok := req.attachments()
			if !ok {
				return nil, http.StatusBadRequest
			}
			out, err := s.email.AddThread(ctx, customerID, id, subscribers, subject,
				req.str("body"), toEmailAttachments(files))
			return s.result(w, r, out, err)
		case http.MethodPut:
			out, err := s.email.EditThread(ctx, customerID, id, req.int64("folderId"))
			return s.result(w, r, out, err)
		case http.MethodDelete:
			out, err := s.email.DeleteThread(ctx, customerID, id)
			return s.result(w, r, out, err)
		}
		return nil, http.StatusMethodNotAllowed

	case "subscriber":
		threadID := parseID(req.ID)
		if threadID == 0 {
			return nil, http.StatusBadRequest
		}
		address := req.str("email")
		if address == "" {
			return nil, http.StatusBadRequest
		}
		switch req.Method {
		case http.MethodPost:
			out, err := s.email.AddSubscriber(ctx, customerID, threadID, address)
			return s.result(w, r, out, err)
		case http.MethodDelete:
			// Leaving: only one's own address, nobody can be removed (RFC 5.6).
			out, err := s.email.Leave(ctx, customerID, threadID, address)
			return s.result(w, r, out, err)
		}
		return nil, http.StatusMethodNotAllowed

	case "message":
		id := parseID(req.ID)
		if id == 0 {
			return nil, http.StatusBadRequest
		}
		switch req.Method {
		case http.MethodGet:
			out, err := s.email.ListMessages(ctx, customerID, id, search)
			return s.result(w, r, out, err)
		case http.MethodPost:
			files, ok := req.attachments()
			if !ok {
				return nil, http.StatusBadRequest
			}
			out, err := s.email.AddMessage(ctx, customerID, id, req.str("body"), toEmailAttachments(files))
			return s.result(w, r, out, err)
		case http.MethodPut:
			out, err := s.email.MarkMessageRead(ctx, customerID, id)
			return s.result(w, r, out, err)
		}
		return nil, http.StatusMethodNotAllowed

	case "attachment":
		attachmentID := parseID(req.ID)
		if attachmentID == 0 {
			return nil, http.StatusBadRequest
		}
		switch req.Method {
		case http.MethodGet:
			out, err := s.email.GetAttachment(ctx, customerID, attachmentID)
			if err != nil {
				s.fail(w, r, err)
				return nil, 0
			}
			if out == nil {
				return nil, http.StatusNotFound
			}
			return out, http.StatusOK
		case http.MethodDelete:
			// Unlike the other deletes, which report a miss as false, an
			// attachment that is gone is a 404. The PHP looks the row up
			// before it acts on it and answers the same way.
			out, err := s.email.DeleteAttachment(ctx, customerID, attachmentID)
			if err != nil {
				s.fail(w, r, err)
				return nil, 0
			}
			if !out {
				return nil, http.StatusNotFound
			}
			return out, http.StatusOK
		}
		return nil, http.StatusMethodNotAllowed

	case "contact":
		if req.Method == http.MethodGet || req.Method == http.MethodPost {
			id := parseID(req.ID)
			if id == 0 {
				return nil, http.StatusBadRequest
			}
			if id != customerID {
				return nil, http.StatusNotFound
			}
		} else if parseID(req.ID) == 0 {
			return nil, http.StatusBadRequest
		}
		switch req.Method {
		case http.MethodGet:
			out, err := s.email.ListContacts(ctx, customerID, search)
			return s.result(w, r, out, err)
		case http.MethodPost:
			address := req.str("email")
			if address == "" {
				return nil, http.StatusBadRequest
			}
			out, err := s.email.AddContact(ctx, customerID, address, req.str("name"))
			return s.result(w, r, out, err)
		case http.MethodPut:
			out, err := s.email.EditContact(ctx, customerID, parseID(req.ID),
				req.str("email"), req.str("name"))
			return s.result(w, r, out, err)
		case http.MethodDelete:
			out, err := s.email.DeleteContact(ctx, customerID, parseID(req.ID))
			return s.result(w, r, out, err)
		}
		return nil, http.StatusMethodNotAllowed
	}
	return nil, http.StatusNotFound
}

func toEmailAttachments(files []attachment) []email.NewAttachment {
	out := make([]email.NewAttachment, len(files))
	for i, f := range files {
		out[i] = email.NewAttachment{FileName: f.FileName, MimeType: f.MimeType, FileData: f.FileData}
	}
	return out
}

// result answers 200 with the value, 400 naming the addresses when a message
// was refused for recipients with no account, 403 when the caller is not a
// member or tried to remove someone else, or 500 for any other error.
func (s *Server) result(w http.ResponseWriter, r *http.Request, value any, err error) (any, int) {
	var unknown *email.UnknownRecipientsError
	if errors.As(err, &unknown) {
		return map[string]any{"error": "unknown_recipients", "recipients": unknown.Addresses},
			http.StatusBadRequest
	}
	var notMember *email.NotMemberError
	if errors.As(err, &notMember) {
		return map[string]any{"error": "not_a_member", "detail": err.Error()}, http.StatusForbidden
	}
	var notInChat *chat.NotMemberError
	if errors.As(err, &notInChat) {
		return map[string]any{"error": "not_a_member", "detail": err.Error()}, http.StatusForbidden
	}
	var tooLarge *email.TooLargeError
	if errors.As(err, &tooLarge) {
		return map[string]any{"error": "too_large", "detail": err.Error()}, http.StatusRequestEntityTooLarge
	}
	var notYours *email.OnlyYourselfError
	if errors.As(err, &notYours) {
		return map[string]any{"error": "only_yourself", "detail": err.Error()}, http.StatusForbidden
	}
	if err != nil {
		s.fail(w, r, err)
		return nil, 0
	}
	return value, http.StatusOK
}
