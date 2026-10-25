// Package rrule expands the recurrence of a meeting: the JSON of RFC 15.6
// that a meeting stores and sends between Domains (repeat.go, ADR 22).
//
// It also reads the RRULE the calendar API takes, the subset of RFC 5545 that
// the calendar UI produces: FREQ of DAILY, WEEKLY, MONTHLY or YEARLY, with
// INTERVAL, COUNT, UNTIL, BYDAY and BYMONTHDAY. Parts outside that subset are
// reported by Parse rather than silently ignored, and FromRule turns the rest
// into the JSON form, so a rule is never expanded into the wrong dates.
package rrule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Frequency is the FREQ part of a rule.
type Frequency int

// The frequencies this package supports.
const (
	Daily Frequency = iota
	Weekly
	Monthly
	Yearly
)

func (f Frequency) String() string {
	switch f {
	case Daily:
		return "daily"
	case Weekly:
		return "weekly"
	case Monthly:
		return "monthly"
	case Yearly:
		return "yearly"
	}
	return "unknown"
}

// Rule is a parsed recurrence rule.
type Rule struct {
	Freq       Frequency
	Interval   int
	Count      int       // 0 means unbounded
	Until      time.Time // zero means unbounded
	ByDay      []time.Weekday
	ByMonthDay []int
	text       string
}

// ErrUnsupported reports a rule part this package does not implement.
type ErrUnsupported struct{ Part string }

func (e *ErrUnsupported) Error() string {
	return fmt.Sprintf("unsupported recurrence rule part %q", e.Part)
}

var weekdays = map[string]time.Weekday{
	"SU": time.Sunday, "MO": time.Monday, "TU": time.Tuesday, "WE": time.Wednesday,
	"TH": time.Thursday, "FR": time.Friday, "SA": time.Saturday,
}

// Parse reads an RFC 5545 RRULE string, with or without the "RRULE:" prefix.
func Parse(s string) (*Rule, error) {
	text := strings.TrimSpace(s)
	if text == "" {
		return nil, nil
	}
	body := text
	if i := strings.Index(strings.ToUpper(body), "RRULE:"); i == 0 {
		body = body[len("RRULE:"):]
	}

	r := &Rule{Interval: 1, text: text}
	seenFreq := false
	for _, part := range strings.Split(body, ";") {
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("malformed recurrence rule part %q", part)
		}
		key = strings.ToUpper(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		switch key {
		case "FREQ":
			seenFreq = true
			switch strings.ToUpper(value) {
			case "DAILY":
				r.Freq = Daily
			case "WEEKLY":
				r.Freq = Weekly
			case "MONTHLY":
				r.Freq = Monthly
			case "YEARLY":
				r.Freq = Yearly
			default:
				return nil, &ErrUnsupported{Part: "FREQ=" + value}
			}
		case "INTERVAL":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("invalid INTERVAL %q", value)
			}
			r.Interval = n
		case "COUNT":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("invalid COUNT %q", value)
			}
			r.Count = n
		case "UNTIL":
			t, err := parseUntil(value)
			if err != nil {
				return nil, err
			}
			r.Until = t
		case "BYDAY":
			for _, d := range strings.Split(value, ",") {
				d = strings.ToUpper(strings.TrimSpace(d))
				// An ordinal prefix such as "2MO" selects the second Monday of
				// the period, which this subset does not implement.
				if len(d) != 2 {
					return nil, &ErrUnsupported{Part: "BYDAY=" + d}
				}
				wd, ok := weekdays[d]
				if !ok {
					return nil, fmt.Errorf("invalid BYDAY %q", d)
				}
				r.ByDay = append(r.ByDay, wd)
			}
		case "BYMONTHDAY":
			for _, d := range strings.Split(value, ",") {
				n, err := strconv.Atoi(strings.TrimSpace(d))
				if err != nil || n == 0 || n < -31 || n > 31 {
					return nil, fmt.Errorf("invalid BYMONTHDAY %q", d)
				}
				r.ByMonthDay = append(r.ByMonthDay, n)
			}
		case "WKST":
			// Only affects rules this subset does not support.
		default:
			return nil, &ErrUnsupported{Part: key}
		}
	}
	if !seenFreq {
		return nil, fmt.Errorf("recurrence rule has no FREQ")
	}
	return r, nil
}

func parseUntil(v string) (time.Time, error) {
	for _, layout := range []string{"20060102T150405Z", "20060102T150405", "20060102"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid UNTIL %q", v)
}

func plural(f Frequency) string {
	switch f {
	case Daily:
		return "days"
	case Weekly:
		return "weeks"
	case Monthly:
		return "months"
	case Yearly:
		return "years"
	}
	return "periods"
}

func ordinal(n int) string {
	if n < 0 {
		if n == -1 {
			return "last day"
		}
		return strconv.Itoa(-n) + suffix(-n) + " last day"
	}
	return strconv.Itoa(n) + suffix(n)
}

func suffix(n int) string {
	if n%100 >= 11 && n%100 <= 13 {
		return "th"
	}
	switch n % 10 {
	case 1:
		return "st"
	case 2:
		return "nd"
	case 3:
		return "rd"
	}
	return "th"
}

func joinWords(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// String returns the original rule text.
func (r *Rule) String() string {
	if r == nil {
		return ""
	}
	return r.text
}
