package rrule

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Repeat is the recurrence of a meeting as RFC 15.6 writes it: the parts of
// an RRULE this package implements, as JSON. It is what a meeting stores and
// what travels between Domains (ADR 22). Interval and Count are pointers so
// that an explicit zero is refused rather than read as absent.
type Repeat struct {
	Frequency string   `json:"frequency"`
	Interval  *int     `json:"interval,omitempty"`
	Count     *int     `json:"count,omitempty"`
	Until     string   `json:"until,omitempty"`
	Weekdays  []string `json:"weekdays,omitempty"`
	MonthDays []int    `json:"month_days,omitempty"`
	Extra     []string `json:"extra,omitempty"`
}

// Override changes one occurrence of a series (RFC 15.6). A field left out
// is the meeting's; the strings are pointers so an empty title can be set.
type Override struct {
	Occurrence      string  `json:"occurrence"`
	Status          string  `json:"status,omitempty"`
	Start           string  `json:"start,omitempty"`
	End             string  `json:"end,omitempty"`
	Title           *string `json:"title,omitempty"`
	Description     *string `json:"description,omitempty"`
	DescriptionHTML *string `json:"description_html,omitempty"`
}

// ErrUnknownMember is a member of a repeat or an override this package does
// not know, or a combination RFC 15.6 does not allow. Unlike anywhere else in
// an envelope it is refused, because a rule read with a part ignored expands
// into the wrong dates.
type ErrUnknownMember struct{ Reason string }

func (e *ErrUnknownMember) Error() string { return e.Reason }

func unknown(format string, args ...any) error {
	return &ErrUnknownMember{Reason: fmt.Sprintf(format, args...)}
}

// strict decodes b into v, refusing members v does not have.
func strict(b []byte, v any, what string) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return unknown("%s: %v", what, err)
	}
	return nil
}

// UnmarshalJSON reads a repeat and nothing it does not know.
func (r *Repeat) UnmarshalJSON(b []byte) error {
	type plain Repeat
	var p plain
	if err := strict(b, &p, "repeat"); err != nil {
		return err
	}
	*r = Repeat(p)
	return nil
}

// UnmarshalJSON reads an override and nothing it does not know.
func (o *Override) UnmarshalJSON(b []byte) error {
	type plain Override
	var p plain
	if err := strict(b, &p, "override"); err != nil {
		return err
	}
	*o = Override(p)
	return nil
}

// The statuses of an occurrence.
const (
	StatusConfirmed = "confirmed"
	StatusCancelled = "cancelled"
)

// WeekdayNames are the RFC 15.6 names of the days of the week.
var WeekdayNames = map[string]time.Weekday{
	"mo": time.Monday, "tu": time.Tuesday, "we": time.Wednesday, "th": time.Thursday,
	"fr": time.Friday, "sa": time.Saturday, "su": time.Sunday,
}

// The bounds of RFC 15.6.
const (
	maxValue   = 1000
	maxEntries = 1000
)

// IntervalOrDefault is the interval of a rule, 1 when it states none.
func (r *Repeat) IntervalOrDefault() int {
	if r.Interval == nil {
		return 1
	}
	return *r.Interval
}

// Validate refuses the values and combinations RFC 15.6 does not allow.
func (r *Repeat) Validate() error {
	switch r.Frequency {
	case "daily", "weekly", "monthly", "yearly":
	default:
		return unknown("repeat.frequency is daily, weekly, monthly or yearly")
	}
	if r.Interval != nil && (*r.Interval < 1 || *r.Interval > maxValue) {
		return unknown("repeat.interval is 1 to 1000")
	}
	if r.Count != nil && (*r.Count < 1 || *r.Count > maxValue) {
		return unknown("repeat.count is 1 to 1000")
	}
	if r.Count != nil && r.Until != "" {
		return unknown("repeat has count and until")
	}
	if r.Until != "" {
		if _, err := ParseInstant(r.Until); err != nil {
			return unknown("repeat.until: %v", err)
		}
	}
	if r.Weekdays != nil {
		if r.Frequency != "daily" && r.Frequency != "weekly" {
			return unknown("repeat.weekdays is only for daily and weekly")
		}
		if len(r.Weekdays) == 0 || len(r.Weekdays) > 7 {
			return unknown("repeat.weekdays holds 1 to 7 days")
		}
		seen := map[string]bool{}
		for _, d := range r.Weekdays {
			if _, ok := WeekdayNames[d]; !ok || seen[d] {
				return unknown("repeat.weekdays holds an unknown or repeated day %q", d)
			}
			seen[d] = true
		}
	}
	if r.MonthDays != nil {
		if r.Frequency != "monthly" {
			return unknown("repeat.month_days is only for monthly")
		}
		if len(r.MonthDays) == 0 || len(r.MonthDays) > 31 {
			return unknown("repeat.month_days holds 1 to 31 days")
		}
		seen := map[int]bool{}
		for _, d := range r.MonthDays {
			if d == 0 || d < -31 || d > 31 || seen[d] {
				return unknown("repeat.month_days holds an invalid or repeated day %d", d)
			}
			seen[d] = true
		}
	}
	if len(r.Extra) > maxEntries {
		return unknown("repeat.extra holds more than 1000 instants")
	}
	for _, v := range r.Extra {
		if _, err := ParseInstant(v); err != nil {
			return unknown("repeat.extra: %v", err)
		}
	}
	return nil
}

// ValidateOverrides refuses overrides RFC 15.6 does not allow. It does not
// check that an occurrence is one the rule gives: an override for an instant
// the rule does not give is ignored, not refused.
func ValidateOverrides(overrides []Override, maxTitle, maxDescription, maxHTML int) error {
	if len(overrides) > maxEntries {
		return unknown("more than 1000 overrides")
	}
	seen := map[time.Time]bool{}
	for _, o := range overrides {
		at, err := ParseInstant(o.Occurrence)
		if err != nil {
			return unknown("override.occurrence: %v", err)
		}
		if seen[at] {
			return unknown("two overrides for occurrence %s", o.Occurrence)
		}
		seen[at] = true
		if o.Status != "" && o.Status != StatusConfirmed && o.Status != StatusCancelled {
			return unknown("override.status is confirmed or cancelled")
		}
		var start, end time.Time
		if o.Start != "" {
			if start, err = ParseInstant(o.Start); err != nil {
				return unknown("override.start: %v", err)
			}
		}
		if o.End != "" {
			if end, err = ParseInstant(o.End); err != nil {
				return unknown("override.end: %v", err)
			}
		}
		if o.Start != "" && o.End != "" && !end.After(start) {
			return unknown("override.end is not later than override.start")
		}
		if o.Title != nil && len(*o.Title) > maxTitle ||
			o.Description != nil && len(*o.Description) > maxDescription ||
			o.DescriptionHTML != nil && len(*o.DescriptionHTML) > maxHTML {
			return unknown("an override's title or description is too long")
		}
	}
	return nil
}

// ParseInstant reads an RFC 3339 UTC timestamp, as the wire writes them.
func ParseInstant(v string) (time.Time, error) {
	if !strings.HasSuffix(v, "Z") {
		return time.Time{}, fmt.Errorf("timestamp %q is not UTC", v)
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("timestamp %q: %w", v, err)
	}
	return t.UTC(), nil
}

// FormatInstant writes an instant the way the wire does, so that the same
// occurrence is the same string on every Domain.
func FormatInstant(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// Series is a meeting's timing: its first occurrence, its zone, its rule and
// its overrides. A single meeting has no Repeat.
type Series struct {
	Start, End time.Time
	Location   *time.Location
	AllDay     bool
	Repeat     *Repeat
	Overrides  []Override
}

// Occurrence is one occurrence of a series.
type Occurrence struct {
	// Key is the start the occurrence has by the rule, or its entry in
	// extra, as FormatInstant writes it. It is what overrides and answers
	// name it by, and it does not change when the occurrence is moved.
	Key        string
	Start, End time.Time
	// Override is the override that applies, if any.
	Override *Override
}

// Cancelled reports whether an override cancelled the occurrence.
func (o Occurrence) Cancelled() bool {
	return o.Override != nil && o.Override.Status == StatusCancelled
}

// maxOccurrences bounds the expansion of a series without end, however far
// a reader looks ahead.
const maxOccurrences = 100000

// Expand returns the occurrences of a series that start before until, in
// order, with their overrides applied. An override for an instant the rule
// does not give is ignored. Expansion follows RFC 15.6: in the local time of
// the zone, from the date of the first occurrence, weeks from Monday, a start
// in a daylight saving gap moved forward by the gap and one that occurs twice
// taken the first time.
func (s Series) Expand(until time.Time) []Occurrence {
	byKey := map[string]*Override{}
	horizon := until
	for i := range s.Overrides {
		o := &s.Overrides[i]
		if at, err := ParseInstant(o.Occurrence); err == nil {
			byKey[FormatInstant(at)] = o
			// An override may move an occurrence from beyond the horizon
			// into it, so the rule is expanded as far as any override names.
			if !at.Before(horizon) {
				horizon = at.Add(time.Second)
			}
		}
	}

	starts := s.ruleStarts(horizon)
	if s.Repeat != nil {
		for _, v := range s.Repeat.Extra {
			if at, err := ParseInstant(v); err == nil && !containsInstant(starts, at) {
				starts = append(starts, at)
			}
		}
		sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })
	}

	var out []Occurrence
	for _, start := range starts {
		o := Occurrence{Key: FormatInstant(start), Start: start, End: s.endFor(start)}
		if override := byKey[o.Key]; override != nil {
			o.Override = override
			if override.Start != "" {
				if at, err := ParseInstant(override.Start); err == nil {
					o.Start, o.End = at, s.endFor(at)
				}
			}
			if override.End != "" {
				if at, err := ParseInstant(override.End); err == nil {
					o.End = at
				}
			}
		}
		if o.Start.Before(until) {
			out = append(out, o)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

// Bounds are the earliest start and the latest end of any occurrence. A
// series without end has no last end, and reports false.
func (s Series) Bounds() (time.Time, time.Time, bool) {
	unbounded := s.Repeat != nil && s.Repeat.Count == nil && s.Repeat.Until == ""
	var limit time.Time
	if unbounded {
		limit = s.Start.AddDate(1, 0, 0)
	} else {
		limit = s.Start.AddDate(10000, 0, 0)
	}
	occurrences := s.Expand(limit)
	first, last := s.Start, s.End
	for _, o := range occurrences {
		if o.Start.Before(first) {
			first = o.Start
		}
		if o.End.After(last) {
			last = o.End
		}
	}
	return first, last, !unbounded
}

// endFor is when an occurrence that starts at start ends: as long after it
// as the meeting lasts, and for an all-day meeting the same number of days.
func (s Series) endFor(start time.Time) time.Time {
	if !s.AllDay {
		return start.Add(s.End.Sub(s.Start))
	}
	days := daysBetween(s.Start.In(s.Location), s.End.In(s.Location))
	local := start.In(s.Location)
	return localInstant(local.Year(), local.Month(), local.Day()+days, 0, 0, 0, s.Location)
}

func daysBetween(a, b time.Time) int {
	da := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	db := time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
	return int(db.Sub(da).Hours() / 24)
}

func containsInstant(list []time.Time, t time.Time) bool {
	for _, v := range list {
		if v.Equal(t) {
			return true
		}
	}
	return false
}

// ruleStarts lists the starts the rule gives before horizon: the first
// occurrence, which is always the meeting's start, and each later date of
// the rule at the local time of day of that start.
func (s Series) ruleStarts(horizon time.Time) []time.Time {
	starts := []time.Time{s.Start}
	r := s.Repeat
	if r == nil {
		return starts
	}
	var until time.Time
	if r.Until != "" {
		until, _ = ParseInstant(r.Until)
	}
	count := 0
	if r.Count != nil {
		count = *r.Count
	}
	local := s.Start.In(s.Location)
	first := civil{local.Year(), local.Month(), local.Day()}
	h, m, sec := local.Clock()
	interval := r.IntervalOrDefault()

	done := func(at time.Time) bool {
		return (count > 0 && len(starts) >= count) || (!until.IsZero() && at.After(until)) ||
			!at.Before(horizon) || len(starts) >= maxOccurrences
	}
	if count == 1 {
		return starts
	}
	for period := 0; period < maxOccurrences; period += interval {
		// A period that begins past the horizon or the rule's end has no
		// occurrence left to give, and nor does any after it.
		begins := s.periodStart(first, period)
		at := localInstant(begins.year, begins.month, begins.day, 0, 0, 0, s.Location)
		if !at.Before(horizon) || (!until.IsZero() && at.After(until)) {
			return starts
		}
		for _, d := range s.datesIn(first, period) {
			if !first.before(d) {
				continue
			}
			at := localInstant(d.year, d.month, d.day, h, m, sec, s.Location)
			if done(at) {
				return starts
			}
			starts = append(starts, at)
		}
	}
	return starts
}

// civil is a date on the calendar of the series' zone.
type civil struct {
	year  int
	month time.Month
	day   int
}

func (c civil) before(o civil) bool {
	if c.year != o.year {
		return c.year < o.year
	}
	if c.month != o.month {
		return c.month < o.month
	}
	return c.day < o.day
}

func normalized(y int, m time.Month, d int) civil {
	t := time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
	return civil{t.Year(), t.Month(), t.Day()}
}

// periodStart is the first date of the period'th day, week, month or year
// after the one the series starts in.
func (s Series) periodStart(first civil, period int) civil {
	switch s.Repeat.Frequency {
	case "daily":
		return normalized(first.year, first.month, first.day+period)
	case "weekly":
		monday := mondayOf(first)
		return normalized(monday.year, monday.month, monday.day+7*period)
	case "monthly":
		return normalized(first.year, first.month+time.Month(period), 1)
	}
	return civil{first.year + period, time.January, 1}
}

// datesIn lists the dates the rule gives in one period, in order. Dates that
// do not exist, such as the 31st of a short month, are left out.
func (s Series) datesIn(first civil, period int) []civil {
	r := s.Repeat
	switch r.Frequency {
	case "daily":
		d := normalized(first.year, first.month, first.day+period)
		if r.Weekdays != nil && !hasWeekday(r.Weekdays, weekdayOf(d)) {
			return []civil{}
		}
		return []civil{d}
	case "weekly":
		monday := mondayOf(first)
		days := []time.Weekday{weekdayOf(first)}
		if r.Weekdays != nil {
			days = days[:0]
			for _, name := range r.Weekdays {
				days = append(days, WeekdayNames[name])
			}
		}
		var out []civil
		for _, wd := range days {
			offset := (int(wd) + 6) % 7
			out = append(out, normalized(monday.year, monday.month, monday.day+7*period+offset))
		}
		sort.Slice(out, func(i, j int) bool { return out[i].before(out[j]) })
		return out
	case "monthly":
		month := normalized(first.year, first.month+time.Month(period), 1)
		last := daysIn(month.year, month.month)
		wanted := []int{first.day}
		if r.MonthDays != nil {
			wanted = r.MonthDays
		}
		seen := map[int]bool{}
		var days []int
		for _, d := range wanted {
			if d < 0 {
				d = last + 1 + d
			}
			if d >= 1 && d <= last && !seen[d] {
				seen[d] = true
				days = append(days, d)
			}
		}
		sort.Ints(days)
		out := make([]civil, 0, len(days))
		for _, d := range days {
			out = append(out, civil{month.year, month.month, d})
		}
		return out
	case "yearly":
		year := first.year + period
		if first.day > daysIn(year, first.month) {
			return []civil{}
		}
		return []civil{{year, first.month, first.day}}
	}
	return nil
}

func daysIn(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func weekdayOf(d civil) time.Weekday {
	return time.Date(d.year, d.month, d.day, 12, 0, 0, 0, time.UTC).Weekday()
}

func mondayOf(d civil) civil {
	return normalized(d.year, d.month, d.day-(int(weekdayOf(d))+6)%7)
}

func hasWeekday(names []string, wd time.Weekday) bool {
	for _, name := range names {
		if WeekdayNames[name] == wd {
			return true
		}
	}
	return false
}

// localInstant is the instant a local date and time of day fall on in loc.
// A time in a daylight saving gap does not exist and moves forward by the
// length of the gap; a time that occurs twice is taken the first time (RFC
// 15.6). Go's time.Date leaves both cases unspecified, so the candidates are
// worked out from the offsets either side of the date.
func localInstant(year int, month time.Month, day, hour, min, sec int, loc *time.Location) time.Time {
	wall := time.Date(year, month, day, hour, min, sec, 0, time.UTC)
	_, before := wall.Add(-48 * time.Hour).In(loc).Zone()
	_, after := wall.Add(48 * time.Hour).In(loc).Zone()
	var found []time.Time
	for _, offset := range []int{before, after} {
		at := wall.Add(-time.Duration(offset) * time.Second)
		local := at.In(loc)
		if local.Year() == year && local.Month() == month && local.Day() == day &&
			local.Hour() == hour && local.Minute() == min && local.Second() == sec {
			found = append(found, at)
		}
	}
	if len(found) == 0 {
		// In the gap: read with the offset from before it, the instant lands
		// after the change, as late as the gap is long.
		return wall.Add(-time.Duration(before) * time.Second).UTC()
	}
	first := found[0]
	for _, at := range found[1:] {
		if at.Before(first) {
			first = at
		}
	}
	return first.UTC()
}

// FromRule turns an RRULE, as the calendar API takes one, into the JSON form
// a meeting stores. A part RFC 15.6 has no place for is refused.
func FromRule(r *Rule) (*Repeat, error) {
	if r == nil {
		return nil, nil
	}
	out := &Repeat{Frequency: r.Freq.String()}
	if r.Interval > 1 {
		n := r.Interval
		out.Interval = &n
	}
	if r.Count > 0 {
		n := r.Count
		out.Count = &n
	}
	if !r.Until.IsZero() {
		out.Until = FormatInstant(r.Until)
	}
	if len(r.ByDay) > 0 {
		if r.Freq != Daily && r.Freq != Weekly {
			return nil, &ErrUnsupported{Part: "BYDAY with FREQ=" + strings.ToUpper(r.Freq.String())}
		}
		for _, wd := range r.ByDay {
			out.Weekdays = append(out.Weekdays, strings.ToLower(wd.String()[:2]))
		}
	}
	if len(r.ByMonthDay) > 0 {
		if r.Freq != Monthly {
			return nil, &ErrUnsupported{Part: "BYMONTHDAY with FREQ=" + strings.ToUpper(r.Freq.String())}
		}
		out.MonthDays = append(out.MonthDays, r.ByMonthDay...)
	}
	if err := out.Validate(); err != nil {
		return nil, &ErrUnsupported{Part: err.Error()}
	}
	return out, nil
}

// HumanReadable renders a repeat as a short sentence, such as "weekly on
// Tuesday, 6 times".
func (r *Repeat) HumanReadable() string {
	if r == nil {
		return ""
	}
	var freq Frequency
	switch r.Frequency {
	case "daily":
		freq = Daily
	case "weekly":
		freq = Weekly
	case "monthly":
		freq = Monthly
	default:
		freq = Yearly
	}
	var b strings.Builder
	if n := r.IntervalOrDefault(); n == 1 {
		b.WriteString(r.Frequency)
	} else {
		b.WriteString("every " + strconv.Itoa(n) + " " + plural(freq))
	}
	if len(r.Weekdays) > 0 {
		names := make([]string, len(r.Weekdays))
		for i, d := range r.Weekdays {
			names[i] = WeekdayNames[d].String()
		}
		b.WriteString(" on " + joinWords(names))
	}
	if len(r.MonthDays) > 0 {
		days := make([]string, len(r.MonthDays))
		for i, d := range r.MonthDays {
			days[i] = ordinal(d)
		}
		b.WriteString(" on the " + joinWords(days))
	}
	switch {
	case r.Count != nil:
		b.WriteString(", " + strconv.Itoa(*r.Count) + " times")
	case r.Until != "":
		if t, err := ParseInstant(r.Until); err == nil {
			b.WriteString(", until " + t.Format("2 January 2006"))
		}
	}
	return b.String()
}
