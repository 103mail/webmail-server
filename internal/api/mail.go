package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/103mail/webmail-server/internal/email"
	"github.com/103mail/webmail-server/internal/mailer"
)

// handleInboundMail accepts one received message from Amazon SES.
//
// SES receipt rules write the raw message to S3 and publish a notification to
// an SNS topic; SNS posts that notification here (ADR 15). The endpoint carries
// no bearer token because the caller is Amazon: the SNS signature is the
// authentication, and nothing is read out of the body until it verifies.
//
// A message that is dropped or ignored is still answered with 200, because a
// 5xx is what makes SNS retry and there is nothing to retry. A post we do not
// believe gets 403, so a forged one is not retried either.
func (s *Server) handleInboundMail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.respond(w, http.StatusMethodNotAllowed, nil)
		return
	}
	if s.receiver == nil || s.verifier == nil {
		s.log.Warn("inbound mail posted but SES is not configured")
		s.respond(w, http.StatusServiceUnavailable, nil)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes))
	if err != nil {
		s.respond(w, http.StatusRequestEntityTooLarge, nil)
		return
	}
	notification, err := mailer.ParseNotification(body)
	if err != nil {
		// Not an SNS envelope at all. Retrying will not make it one.
		s.log.Warn("inbound mail: unparseable post", "error", err)
		s.respond(w, http.StatusBadRequest, nil)
		return
	}

	inbound, err := s.receiver.Receive(r.Context(), body)
	switch {
	case errors.Is(err, mailer.ErrNotVerified):
		s.log.Warn("inbound mail rejected: unverified post", "remote", r.RemoteAddr, "error", err)
		s.respond(w, http.StatusForbidden, nil)
		return
	case errors.Is(err, mailer.ErrNotMail):
		// Verified, but not a received message: a subscription confirmation,
		// or a notification type we do not act on.
		s.handleNonMail(w, r, notification)
		return
	case err != nil:
		// Ours to fix, so 5xx and let SNS deliver it again.
		s.fail(w, r, err)
		return
	}

	status, err := s.acceptInbound(r.Context(), inbound)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("inbound mail", "providerId", inbound.ProviderID, "from", inbound.From.Email,
		"recipients", len(inbound.Recipients), "outcome", status)
	s.respond(w, http.StatusOK, map[string]string{"status": status})
}

// handleNonMail deals with the SNS messages that are not a received email.
// Subscription confirmations are completed here, because that is how the topic
// gets wired up to this endpoint in the first place.
func (s *Server) handleNonMail(w http.ResponseWriter, r *http.Request, n *mailer.Notification) {
	switch n.Type {
	case mailer.TypeSubscriptionConfirmation, mailer.TypeUnsubscribeConfirmation:
		if err := s.verifier.Confirm(r.Context(), n); err != nil {
			// Fetching the subscribe URL failed, which is worth another try.
			s.fail(w, r, err)
			return
		}
		s.log.Info("sns subscription confirmed", "topic", n.TopicArn, "type", n.Type)
		s.respond(w, http.StatusOK, map[string]string{"status": "confirmed"})
	default:
		s.log.Info("inbound mail: ignoring notification", "topic", n.TopicArn, "type", n.Type)
		s.respond(w, http.StatusOK, map[string]string{"status": "ignored: not mail"})
	}
}

// acceptInbound applies the delivery policy and stores the message. The
// returned string says what happened, for the log and the response.
func (s *Server) acceptInbound(ctx context.Context, in *mailer.Inbound) (string, error) {
	// SES has already run the checks, so a message it judged unsafe is never
	// admitted. A false positive is lost silently apart from this log line.
	for _, failed := range in.Verdicts.Failed() {
		if failed == "virus" || failed == "spam" {
			return "dropped: " + failed, nil
		}
	}

	senderEmail := email.DecodeNumericEntities(in.From.Email)
	if senderEmail == "" {
		return "dropped: no sender", nil
	}

	// Our own outbound coming back would loop, so it is never ingested. This
	// is the check web/mail.php makes.
	local, err := s.email.IsLocalDomain(ctx, senderEmail)
	if err != nil {
		return "", err
	}
	if local {
		return "ignored: local sender", nil
	}

	// Claim the message before doing any work, so a redelivery cannot produce
	// a second thread.
	seen, err := s.email.SeenInbound(ctx, in.ProviderID)
	if err != nil {
		return "", err
	}
	if seen {
		return "ignored: duplicate", nil
	}

	subscribers := recipientsOf(in)
	if len(subscribers) == 0 {
		return "dropped: no recipients", nil
	}

	body := in.HTML
	if body == "" && in.Text != "" {
		// A plain-text message still has to become HTML, because that is what
		// the thread body is rendered from.
		body = "<html><body><pre>" + htmlEscape(in.Text) + "</pre></body></html>"
	}

	attachments := make([]email.NewAttachment, 0, len(in.Attachments))
	for _, a := range in.Attachments {
		attachments = append(attachments, email.NewAttachment{
			FileName: a.FileName,
			MimeType: a.MimeType,
			FileData: a.Content,
		})
	}

	threadID, err := s.email.AddExternalThread(ctx, senderEmail, in.From.Name,
		subscribers, in.Subject, body, attachments)
	if err != nil {
		return "", err
	}
	if threadID == 0 {
		return "dropped: no local mailbox", nil
	}
	return "accepted", nil
}

// recipientsOf collects the addresses a message should be delivered to. The
// envelope recipients come first, since they are what SES accepted the message
// for; the To and Cc headers fill in the rest.
func recipientsOf(in *mailer.Inbound) []string {
	seen := map[string]bool{}
	var out []string
	add := func(address string) {
		address = email.DecodeNumericEntities(strings.TrimSpace(address))
		if address == "" || seen[strings.ToLower(address)] {
			return
		}
		seen[strings.ToLower(address)] = true
		out = append(out, address)
	}
	for _, a := range in.Recipients {
		add(a)
	}
	for _, a := range in.To {
		add(a.Email)
	}
	for _, a := range in.Cc {
		add(a.Email)
	}
	return out
}

var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func htmlEscape(s string) string { return htmlEscaper.Replace(s) }
