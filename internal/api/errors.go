package api

import (
	"errors"
	"strings"

	"github.com/103mail/webmail-server/internal/calendar"
	"github.com/103mail/webmail-server/internal/rrule"
)

// isBadInput reports whether an error is the caller's fault rather than the
// server's, so the handler can answer 400 instead of 500.
func isBadInput(err error) bool {
	var unsupported *rrule.ErrUnsupported
	if errors.As(err, &unsupported) || errors.Is(err, calendar.ErrBadInput) {
		return true
	}
	// rrule.Parse reports malformed values as plain errors.
	return strings.Contains(err.Error(), "parse repeat rule")
}
