// Package notify sends the outbound email this system produces.
//
// Everything it sends is a notification built from an email_template row. There
// is deliberately no way to pass a body: per ADR 4 message content never leaves
// by email, and constraining the only outbound path to templates is what makes
// that structural rather than a rule callers have to remember.
package notify

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/yuin/goldmark"

	"github.com/103mail/webmail-server/internal/mailer"
	"github.com/103mail/webmail-server/internal/store"
)

// ErrNoTemplate reports that no email_template row matches the requested name.
var ErrNoTemplate = errors.New("notify: no such email template")

// Service renders and sends notification email.
type Service struct {
	db     *store.DB
	sender mailer.Sender
	md     goldmark.Markdown
}

// New returns a Service that sends through sender.
func New(db *store.DB, sender mailer.Sender) *Service {
	return &Service{db: db, sender: sender, md: goldmark.New()}
}

// css is prepended to every rendered body, matching the PHP so that templates
// written for one backend look the same through the other.
const css = `<style>#emailtemplate a { color: #009ac7; }</style>`

// template is one email_template row.
type template struct {
	Subject     string
	Body        string
	Language    string
	SenderName  string
	SenderEmail string
}

// Send renders a template and mails it, recording the attempt in sent_email.
//
// customerID may be nil for a message that belongs to no tenant. A failure to
// send is recorded and returned; the sent_email row is written either way, so
// the table is the record of what was attempted, not only of what succeeded.
func (s *Service) Send(ctx context.Context, customerID *int64, toName, toEmail,
	templateName, preferredLanguage string, params map[string]string, bccEmail string) error {
	if toEmail == "" {
		return errors.New("notify: no recipient")
	}
	tmpl, err := s.lookup(ctx, templateName, preferredLanguage)
	if err != nil {
		return err
	}

	subject := render(tmpl.Subject, params, true)
	markdown := render(tmpl.Body, params, true)
	var rendered bytes.Buffer
	if err := s.md.Convert([]byte(markdown), &rendered); err != nil {
		return fmt.Errorf("render template body: %w", err)
	}
	body := css + `<div id="emailtemplate">` + rendered.String() + `</div>`

	msg := mailer.Outbound{
		From:    mailer.Address{Name: tmpl.SenderName, Email: tmpl.SenderEmail},
		To:      []mailer.Address{{Name: toName, Email: toEmail}},
		Subject: subject,
		// The markdown source doubles as the plain-text alternative, so a
		// client that will not render HTML still gets a readable message.
		Text: markdown,
		HTML: body,
	}
	if bccEmail != "" {
		msg.Bcc = []mailer.Address{{Email: bccEmail}}
	}

	status, statusMessage := "sent", ""
	id, sendErr := s.sender.Send(ctx, msg)
	if sendErr != nil {
		status, statusMessage = "error", sendErr.Error()
	} else {
		statusMessage = id
	}

	if err := s.record(ctx, customerID, tmpl, toName, toEmail, templateName,
		preferredLanguage, params, bccEmail, subject, body, status, statusMessage); err != nil {
		// A send that happened must not be reported as a failure because the
		// bookkeeping failed, but the discrepancy has to surface.
		if sendErr == nil {
			return fmt.Errorf("notify: message sent but not recorded: %w", err)
		}
		return fmt.Errorf("notify: send failed (%v) and was not recorded: %w", sendErr, err)
	}
	if sendErr != nil {
		return fmt.Errorf("notify: send %q to %s: %w", templateName, toEmail, sendErr)
	}
	return nil
}

// lookup finds the best template for a language: the requested one, then
// English, then any. This is the fallback chain the PHP uses.
func (s *Service) lookup(ctx context.Context, name, language string) (*template, error) {
	queries := []struct {
		sql  string
		args []any
	}{
		{`SELECT "subject", "body", "language", "sender_name", "sender_email" FROM "email_template" ` +
			`WHERE "name" = ? AND "language" = ? LIMIT 1`, []any{name, language}},
		{`SELECT "subject", "body", "language", "sender_name", "sender_email" FROM "email_template" ` +
			`WHERE "name" = ? AND "language" = 'en' LIMIT 1`, []any{name}},
		{`SELECT "subject", "body", "language", "sender_name", "sender_email" FROM "email_template" ` +
			`WHERE "name" = ? LIMIT 1`, []any{name}},
	}
	for _, q := range queries {
		var t template
		err := s.db.QueryRowContext(ctx, q.sql, q.args...).
			Scan(&t.Subject, &t.Body, &t.Language, &t.SenderName, &t.SenderEmail)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("look up email template %q: %w", name, err)
		}
		return &t, nil
	}
	return nil, fmt.Errorf("%w: %q", ErrNoTemplate, name)
}

func (s *Service) record(ctx context.Context, customerID *int64, tmpl *template,
	toName, toEmail, templateName, preferredLanguage string, params map[string]string,
	bccEmail, subject, body, status, statusMessage string) error {
	encoded, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("encode template parameters: %w", err)
	}
	var customer any
	if customerID != nil {
		customer = *customerID
	}
	// status_message is a varchar(255); a provider error can be longer. Cut
	// on a rune boundary, since PostgreSQL refuses half a UTF-8 sequence.
	if r := []rune(statusMessage); len(r) > 255 {
		statusMessage = string(r[:255])
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO "sent_email" ("customer_id","from_name","from_email","to_name","to_email",`+
			`"preferred_language","template_name","language","parameters","bcc_email","subject",`+
			`"body","status","status_message","created_at") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		customer, tmpl.SenderName, tmpl.SenderEmail, toName, toEmail, preferredLanguage,
		templateName, tmpl.Language, string(encoded), bccEmail, subject, body, status, statusMessage,
		s.db.Now())
	if err != nil {
		return fmt.Errorf("record sent email: %w", err)
	}
	return nil
}
