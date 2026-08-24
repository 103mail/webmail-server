// Package rrule expands the recurrence rules stored in g_schedule.repeat_rule.
//
// It implements the subset of RFC 5545 that the calendar UI produces: FREQ of
// DAILY, WEEKLY, MONTHLY or YEARLY, with INTERVAL, COUNT, UNTIL, BYDAY and
// BYMONTHDAY. Parts outside that subset are reported by Parse rather than
// silently ignored, so a rule is never expanded into the wrong dates.
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

// Occurrences returns up to limit start times at or after dtstart. Expansion
// stops at COUNT, UNTIL or limit, whichever comes first.
func (r *Rule) Occurrences(dtstart time.Time, limit int) []time.Time {
	if r == nil || limit <= 0 {
		return nil
	}
	var out []time.Time
	// Bound the walk so a rule that matches nothing cannot spin forever.
	const maxSteps = 100000
	seen := 0
	for cursor, steps := dtstart, 0; steps < maxSteps; steps++ {
		if len(out) >= limit {
			break
		}
		if r.Count > 0 && seen >= r.Count {
			break
		}
		if !r.Until.IsZero() && cursor.After(r.Until) {
			break
		}
		if r.matches(cursor, dtstart) {
			seen++
			out = append(out, cursor)
		}
		cursor = r.step(cursor, dtstart)
		if cursor.IsZero() {
			break
		}
	}
	return out
}

// matches reports whether a candidate satisfies the BY* parts.
func (r *Rule) matches(t, dtstart time.Time) bool {
	if len(r.ByDay) > 0 {
		found := false
		for _, wd := range r.ByDay {
			if t.Weekday() == wd {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(r.ByMonthDay) > 0 {
		last := daysInMonth(t.Year(), t.Month())
		found := false
		for _, d := range r.ByMonthDay {
			day := d
			if day < 0 {
				day = last + 1 + day
			}
			if t.Day() == day {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	// For weekly rules with BYDAY the interval applies to whole weeks, so a
	// candidate in a skipped week is not an occurrence.
	if r.Freq == Weekly && r.Interval > 1 {
		if weeksBetween(dtstart, t)%r.Interval != 0 {
			return false
		}
	}
	return true
}

// step advances the cursor to the next candidate instant.
func (r *Rule) step(t, dtstart time.Time) time.Time {
	switch r.Freq {
	case Daily:
		return t.AddDate(0, 0, r.Interval)
	case Weekly:
		if len(r.ByDay) > 0 {
			// Walk day by day; matches() filters to the selected weekdays and
			// applies the week interval.
			return t.AddDate(0, 0, 1)
		}
		return t.AddDate(0, 0, 7*r.Interval)
	case Monthly:
		if len(r.ByMonthDay) > 0 || len(r.ByDay) > 0 {
			return t.AddDate(0, 0, 1)
		}
		return addMonths(t, r.Interval, dtstart.Day())
	case Yearly:
		return t.AddDate(r.Interval, 0, 0)
	}
	return time.Time{}
}

// addMonths advances by n months, clamping to the last day of the target month
// so that the 31st does not roll into the next month.
func addMonths(t time.Time, n, preferredDay int) time.Time {
	year, month := t.Year(), int(t.Month())+n
	year += (month - 1) / 12
	month = (month-1)%12 + 1
	if month < 1 {
		month += 12
		year--
	}
	day := preferredDay
	if last := daysInMonth(year, time.Month(month)); day > last {
		day = last
	}
	return time.Date(year, time.Month(month), day, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
}

func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// weeksBetween counts whole weeks between two instants, measured from the
// Monday of each week so that a rule does not shift across a week boundary.
func weeksBetween(a, b time.Time) int {
	startA := startOfWeek(a)
	startB := startOfWeek(b)
	return int(startB.Sub(startA).Hours() / (24 * 7))
}

func startOfWeek(t time.Time) time.Time {
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	offset := (int(day.Weekday()) + 6) % 7 // Monday as the first day
	return day.AddDate(0, 0, -offset)
}

// HumanReadable renders the rule as the sentence stored in
// g_schedule.repeat_rule_human_readable.
func (r *Rule) HumanReadable() string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	if r.Interval == 1 {
		b.WriteString(r.Freq.String())
	} else {
		b.WriteString("every " + strconv.Itoa(r.Interval) + " " + plural(r.Freq))
	}
	if len(r.ByDay) > 0 {
		names := make([]string, len(r.ByDay))
		for i, wd := range r.ByDay {
			names[i] = wd.String()
		}
		b.WriteString(" on " + joinWords(names))
	}
	if len(r.ByMonthDay) > 0 {
		days := make([]string, len(r.ByMonthDay))
		for i, d := range r.ByMonthDay {
			days[i] = ordinal(d)
		}
		b.WriteString(" on the " + joinWords(days))
	}
	switch {
	case r.Count > 0:
		b.WriteString(", " + strconv.Itoa(r.Count) + " times")
	case !r.Until.IsZero():
		b.WriteString(", until " + r.Until.Format("2 January 2006"))
	}
	return b.String()
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
