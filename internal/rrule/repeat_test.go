package rrule

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Skip("no zoneinfo:", err)
	}
	return loc
}

func instant(v string) time.Time {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func ptr[T any](v T) *T { return &v }

func starts(occurrences []Occurrence) []string {
	out := make([]string, len(occurrences))
	for i, o := range occurrences {
		out[i] = o.Start.UTC().Format(time.RFC3339)
	}
	return out
}

func equal(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", what, got, want)
		}
	}
}

// The example of RFC 15.2: every Tuesday, six times, one cancelled. The
// local time of day is kept, so after the change to winter time on the 25th
// of October the UTC time would move; here all six are in winter time.
func TestWeeklyWithACancelledOccurrence(t *testing.T) {
	s := Series{
		Start: instant("2026-11-03T08:00:00Z"), End: instant("2026-11-03T09:00:00Z"),
		Location:  zone(t, "Europe/Amsterdam"),
		Repeat:    &Repeat{Frequency: "weekly", Weekdays: []string{"tu"}, Count: ptr(6)},
		Overrides: []Override{{Occurrence: "2026-11-10T08:00:00Z", Status: StatusCancelled}},
	}
	got := s.Expand(instant("2027-01-01T00:00:00Z"))
	equal(t, "starts", starts(got), []string{
		"2026-11-03T08:00:00Z", "2026-11-10T08:00:00Z", "2026-11-17T08:00:00Z",
		"2026-11-24T08:00:00Z", "2026-12-01T08:00:00Z", "2026-12-08T08:00:00Z",
	})
	if !got[1].Cancelled() || got[0].Cancelled() || got[1].Key != "2026-11-10T08:00:00.000Z" {
		t.Errorf("the override did not land on the second occurrence: %+v", got[1])
	}
	if !got[5].End.Equal(instant("2026-12-08T09:00:00Z")) {
		t.Errorf("an occurrence lasts as long as the meeting: %v", got[5].End)
	}
}

// A daily meeting at nine in Amsterdam stays at nine local across the end of
// summer time, so its UTC time moves by an hour.
func TestTheLocalTimeOfDayIsKept(t *testing.T) {
	s := Series{
		Start: instant("2026-10-24T07:00:00Z"), End: instant("2026-10-24T07:30:00Z"),
		Location: zone(t, "Europe/Amsterdam"),
		Repeat:   &Repeat{Frequency: "daily", Count: ptr(3)},
	}
	equal(t, "starts", starts(s.Expand(instant("2027-01-01T00:00:00Z"))), []string{
		"2026-10-24T07:00:00Z", "2026-10-25T08:00:00Z", "2026-10-26T08:00:00Z",
	})
}

// 02:30 does not exist in New York on the 8th of March 2026 and moves forward
// by the hour of the gap; 01:30 on the 1st of November occurs twice and is
// taken the first time (RFC 15.6).
func TestDaylightSavingGapsAndOverlaps(t *testing.T) {
	ny := zone(t, "America/New_York")
	gap := localInstant(2026, time.March, 8, 2, 30, 0, ny)
	if got := gap.In(ny).Format("15:04"); got != "03:30" || !gap.Equal(instant("2026-03-08T07:30:00Z")) {
		t.Errorf("a start in the gap is %s local (%v), want 03:30", got, gap)
	}
	twice := localInstant(2026, time.November, 1, 1, 30, 0, ny)
	if !twice.Equal(instant("2026-11-01T05:30:00Z")) {
		t.Errorf("a start that occurs twice is %v, want the first, 05:30Z", twice)
	}
}

// The 31st does not exist in every month, and a month without it has no
// occurrence; -1 is the last day of each month.
func TestMonthDays(t *testing.T) {
	utc := time.UTC
	s := Series{
		Start: instant("2026-01-31T10:00:00Z"), End: instant("2026-01-31T11:00:00Z"), Location: utc,
		Repeat: &Repeat{Frequency: "monthly", Count: ptr(4)},
	}
	equal(t, "the 31st", starts(s.Expand(instant("2027-01-01T00:00:00Z"))), []string{
		"2026-01-31T10:00:00Z", "2026-03-31T10:00:00Z", "2026-05-31T10:00:00Z", "2026-07-31T10:00:00Z",
	})
	s.Repeat = &Repeat{Frequency: "monthly", MonthDays: []int{-1}, Count: ptr(3)}
	equal(t, "the last day", starts(s.Expand(instant("2027-01-01T00:00:00Z"))), []string{
		"2026-01-31T10:00:00Z", "2026-02-28T10:00:00Z", "2026-03-31T10:00:00Z",
	})
}

func TestLeapDayRecursOnlyInLeapYears(t *testing.T) {
	s := Series{
		Start: instant("2028-02-29T12:00:00Z"), End: instant("2028-02-29T13:00:00Z"), Location: time.UTC,
		Repeat: &Repeat{Frequency: "yearly", Count: ptr(2)},
	}
	equal(t, "starts", starts(s.Expand(instant("2040-01-01T00:00:00Z"))), []string{
		"2028-02-29T12:00:00Z", "2032-02-29T12:00:00Z",
	})
}

// The first occurrence is always the start, even on a day the rule does not
// name, and it counts towards count; extra dates do not.
func TestTheStartIsAlwaysTheFirstOccurrence(t *testing.T) {
	s := Series{
		Start: instant("2026-11-02T08:00:00Z"), End: instant("2026-11-02T09:00:00Z"), Location: time.UTC,
		Repeat: &Repeat{Frequency: "weekly", Interval: ptr(2), Weekdays: []string{"tu", "th"}, Count: ptr(4),
			Extra: []string{"2026-12-30T08:00:00Z"}},
	}
	equal(t, "starts", starts(s.Expand(instant("2027-01-01T00:00:00Z"))), []string{
		"2026-11-02T08:00:00Z", "2026-11-03T08:00:00Z", "2026-11-05T08:00:00Z",
		"2026-11-17T08:00:00Z", "2026-12-30T08:00:00Z",
	})
}

func TestUntilAndAnUnboundedSeries(t *testing.T) {
	s := Series{
		Start: instant("2026-11-02T08:00:00Z"), End: instant("2026-11-02T09:00:00Z"), Location: time.UTC,
		Repeat: &Repeat{Frequency: "daily", Until: "2026-11-04T08:00:00Z"},
	}
	equal(t, "until", starts(s.Expand(instant("2027-01-01T00:00:00Z"))), []string{
		"2026-11-02T08:00:00Z", "2026-11-03T08:00:00Z", "2026-11-04T08:00:00Z",
	})
	first, last, bounded := s.Bounds()
	if !bounded || !first.Equal(s.Start) || !last.Equal(instant("2026-11-04T09:00:00Z")) {
		t.Errorf("Bounds = %v, %v, %v", first, last, bounded)
	}

	s.Repeat = &Repeat{Frequency: "weekly"}
	if got := s.Expand(instant("2026-11-30T00:00:00Z")); len(got) != 4 {
		t.Errorf("an unbounded series expanded to %d occurrences before the horizon, want 4", len(got))
	}
	if _, _, bounded := s.Bounds(); bounded {
		t.Error("a series without end reported a last end")
	}
}

// An override moves an occurrence, even from beyond the horizon into it, and
// one for an instant the rule does not give is ignored.
func TestOverridesMoveOccurrences(t *testing.T) {
	s := Series{
		Start: instant("2026-11-03T08:00:00Z"), End: instant("2026-11-03T09:00:00Z"), Location: time.UTC,
		Repeat: &Repeat{Frequency: "weekly", Count: ptr(3)},
		Overrides: []Override{
			{Occurrence: "2026-11-17T08:00:00Z", Start: "2026-11-09T08:00:00Z", Title: ptr("Moved")},
			{Occurrence: "2026-11-04T08:00:00Z", Status: StatusCancelled},
		},
	}
	got := s.Expand(instant("2026-11-12T00:00:00Z"))
	equal(t, "starts", starts(got), []string{"2026-11-03T08:00:00Z", "2026-11-09T08:00:00Z", "2026-11-10T08:00:00Z"})
	if got[1].Key != "2026-11-17T08:00:00.000Z" || !got[1].End.Equal(instant("2026-11-09T09:00:00Z")) ||
		*got[1].Override.Title != "Moved" {
		t.Errorf("the moved occurrence = %+v", got[1])
	}
	for _, o := range got {
		if o.Cancelled() {
			t.Errorf("an override for no occurrence cancelled %s", o.Key)
		}
	}
}

// An all-day meeting lasts the same number of days in every occurrence, from
// midnight to midnight local, whatever the change of time in between.
func TestAllDayOccurrences(t *testing.T) {
	ams := zone(t, "Europe/Amsterdam")
	s := Series{
		Start: instant("2026-10-23T22:00:00Z"), End: instant("2026-10-24T22:00:00Z"), Location: ams, AllDay: true,
		Repeat: &Repeat{Frequency: "daily", Count: ptr(2)},
	}
	got := s.Expand(instant("2027-01-01T00:00:00Z"))
	if len(got) != 2 || !got[1].Start.Equal(instant("2026-10-24T22:00:00Z")) ||
		!got[1].End.Equal(instant("2026-10-25T23:00:00Z")) {
		t.Errorf("all-day occurrences = %+v", got)
	}
}

func TestRepeatRefusesWhatItDoesNotKnow(t *testing.T) {
	var r Repeat
	err := json.Unmarshal([]byte(`{"frequency":"weekly","by_set_pos":[1]}`), &r)
	var unknownMember *ErrUnknownMember
	if !errors.As(err, &unknownMember) {
		t.Fatalf("an unknown member was read: %v", err)
	}
	var m struct {
		Overrides []Override `json:"overrides"`
	}
	err = json.Unmarshal([]byte(`{"overrides":[{"occurrence":"2026-11-03T08:00:00Z","location":"x"}]}`), &m)
	if !errors.As(err, &unknownMember) {
		t.Errorf("an unknown override member was read: %v", err)
	}

	for name, r := range map[string]Repeat{
		"an unknown frequency":        {Frequency: "hourly"},
		"count and until":             {Frequency: "daily", Count: ptr(2), Until: "2026-12-01T00:00:00Z"},
		"interval zero":               {Frequency: "daily", Interval: ptr(0)},
		"count over 1000":             {Frequency: "daily", Count: ptr(1001)},
		"weekdays with monthly":       {Frequency: "monthly", Weekdays: []string{"mo"}},
		"month days with weekly":      {Frequency: "weekly", MonthDays: []int{1}},
		"a repeated weekday":          {Frequency: "weekly", Weekdays: []string{"mo", "mo"}},
		"day zero":                    {Frequency: "monthly", MonthDays: []int{0}},
		"no weekdays":                 {Frequency: "weekly", Weekdays: []string{}},
		"an until that is not UTC":    {Frequency: "daily", Until: "2026-12-01T00:00:00+01:00"},
		"an extra that is not a time": {Frequency: "daily", Extra: []string{"tomorrow"}},
	} {
		if err := r.Validate(); !errors.As(err, &unknownMember) {
			t.Errorf("%s was accepted: %v", name, err)
		}
	}
	if err := (&Repeat{Frequency: "weekly", Weekdays: []string{"tu", "th"}, Interval: ptr(2)}).Validate(); err != nil {
		t.Errorf("a valid rule was refused: %v", err)
	}
	if err := ValidateOverrides([]Override{
		{Occurrence: "2026-11-03T08:00:00Z"}, {Occurrence: "2026-11-03T08:00:00.000Z"},
	}, 255, 1<<20, 4<<20); !errors.As(err, &unknownMember) {
		t.Errorf("two overrides for one occurrence were accepted: %v", err)
	}
}

func TestFromRule(t *testing.T) {
	r, err := FromRule(mustParse(t, "FREQ=WEEKLY;INTERVAL=2;BYDAY=TU,TH;COUNT=6"))
	if err != nil || r.Frequency != "weekly" || *r.Interval != 2 || *r.Count != 6 ||
		len(r.Weekdays) != 2 || r.Weekdays[0] != "tu" || r.Weekdays[1] != "th" {
		t.Fatalf("FromRule = %+v, %v", r, err)
	}
	if got := r.HumanReadable(); got != "every 2 weeks on Tuesday and Thursday, 6 times" {
		t.Errorf("HumanReadable = %q", got)
	}
	if r, err := FromRule(mustParse(t, "FREQ=MONTHLY;BYMONTHDAY=-1")); err != nil || r.MonthDays[0] != -1 || r.Interval != nil {
		t.Errorf("FromRule(monthly) = %+v, %v", r, err)
	}
	if _, err := FromRule(mustParse(t, "FREQ=MONTHLY;BYDAY=MO")); err == nil {
		t.Error("a monthly rule on weekdays, which RFC 15.6 has no place for, was converted")
	}
	if r, err := FromRule(nil); r != nil || err != nil {
		t.Errorf("FromRule(nil) = %v, %v", r, err)
	}
}
