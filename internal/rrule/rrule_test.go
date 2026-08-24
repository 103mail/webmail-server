package rrule

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, s string) *Rule {
	t.Helper()
	r, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return r
}

func date(y int, m time.Month, d, h int) time.Time {
	return time.Date(y, m, d, h, 0, 0, 0, time.UTC)
}

func TestParseEmpty(t *testing.T) {
	r, err := Parse("")
	if err != nil || r != nil {
		t.Fatalf("Parse(\"\") = %v, %v; want nil, nil", r, err)
	}
}

func TestDaily(t *testing.T) {
	r := mustParse(t, "FREQ=DAILY;COUNT=3")
	got := r.Occurrences(date(2026, time.March, 1, 9), 10)
	want := []time.Time{date(2026, time.March, 1, 9), date(2026, time.March, 2, 9), date(2026, time.March, 3, 9)}
	assertTimes(t, got, want)
}

func TestDailyInterval(t *testing.T) {
	r := mustParse(t, "FREQ=DAILY;INTERVAL=3;COUNT=3")
	got := r.Occurrences(date(2026, time.March, 1, 9), 10)
	want := []time.Time{date(2026, time.March, 1, 9), date(2026, time.March, 4, 9), date(2026, time.March, 7, 9)}
	assertTimes(t, got, want)
}

func TestWeeklyByDay(t *testing.T) {
	// 2 March 2026 is a Monday.
	r := mustParse(t, "FREQ=WEEKLY;BYDAY=MO,WE;COUNT=4")
	got := r.Occurrences(date(2026, time.March, 2, 9), 10)
	want := []time.Time{
		date(2026, time.March, 2, 9), date(2026, time.March, 4, 9),
		date(2026, time.March, 9, 9), date(2026, time.March, 11, 9),
	}
	assertTimes(t, got, want)
}

func TestWeeklyEveryOtherWeekSkipsTheOddWeek(t *testing.T) {
	r := mustParse(t, "FREQ=WEEKLY;INTERVAL=2;BYDAY=MO;COUNT=3")
	got := r.Occurrences(date(2026, time.March, 2, 9), 10)
	want := []time.Time{date(2026, time.March, 2, 9), date(2026, time.March, 16, 9), date(2026, time.March, 30, 9)}
	assertTimes(t, got, want)
}

func TestMonthlyClampsToShortMonth(t *testing.T) {
	r := mustParse(t, "FREQ=MONTHLY;COUNT=3")
	got := r.Occurrences(date(2026, time.January, 31, 9), 10)
	want := []time.Time{date(2026, time.January, 31, 9), date(2026, time.February, 28, 9), date(2026, time.March, 31, 9)}
	assertTimes(t, got, want)
}

func TestUntilBounds(t *testing.T) {
	r := mustParse(t, "FREQ=DAILY;UNTIL=20260303T000000Z")
	got := r.Occurrences(date(2026, time.March, 1, 0), 10)
	want := []time.Time{date(2026, time.March, 1, 0), date(2026, time.March, 2, 0), date(2026, time.March, 3, 0)}
	assertTimes(t, got, want)
}

func TestLimitCapsUnboundedRule(t *testing.T) {
	r := mustParse(t, "FREQ=DAILY")
	if got := r.Occurrences(date(2026, time.March, 1, 9), 5); len(got) != 5 {
		t.Errorf("got %d occurrences, want 5", len(got))
	}
}

func TestByMonthDayNegativeMeansFromTheEnd(t *testing.T) {
	r := mustParse(t, "FREQ=MONTHLY;BYMONTHDAY=-1;COUNT=2")
	got := r.Occurrences(date(2026, time.January, 1, 9), 10)
	want := []time.Time{date(2026, time.January, 31, 9), date(2026, time.February, 28, 9)}
	assertTimes(t, got, want)
}

func TestUnsupportedPartIsReported(t *testing.T) {
	for _, s := range []string{"FREQ=HOURLY", "FREQ=WEEKLY;BYSETPOS=1", "FREQ=MONTHLY;BYDAY=2MO"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) succeeded, want an error", s)
		}
	}
}

func TestMissingFreqIsAnError(t *testing.T) {
	if _, err := Parse("INTERVAL=2"); err == nil {
		t.Error("Parse without FREQ succeeded, want an error")
	}
}

func TestRrulePrefixAccepted(t *testing.T) {
	r := mustParse(t, "RRULE:FREQ=DAILY;COUNT=1")
	if len(r.Occurrences(date(2026, time.March, 1, 9), 5)) != 1 {
		t.Error("RRULE: prefix not handled")
	}
}

func TestHumanReadable(t *testing.T) {
	tests := map[string]string{
		"FREQ=DAILY":                        "daily",
		"FREQ=WEEKLY;INTERVAL=2":            "every 2 weeks",
		"FREQ=WEEKLY;BYDAY=MO,FR":           "weekly on Monday and Friday",
		"FREQ=MONTHLY;BYMONTHDAY=1;COUNT=3": "monthly on the 1st, 3 times",
	}
	for in, want := range tests {
		if got := mustParse(t, in).HumanReadable(); got != want {
			t.Errorf("HumanReadable(%q) = %q, want %q", in, got, want)
		}
	}
}

func assertTimes(t *testing.T, got, want []time.Time) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d occurrences %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range got {
		if !got[i].Equal(want[i]) {
			t.Errorf("occurrence %d = %v, want %v", i, got[i], want[i])
		}
	}
}
