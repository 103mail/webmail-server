package rrule

import "testing"

func mustParse(t *testing.T, s string) *Rule {
	t.Helper()
	r, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): %v", s, err)
	}
	return r
}

func TestParseEmpty(t *testing.T) {
	r, err := Parse("")
	if err != nil || r != nil {
		t.Fatalf("Parse(\"\") = %v, %v; want nil, nil", r, err)
	}
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
	if r.Freq != Daily || r.Count != 1 {
		t.Errorf("RRULE: prefix not handled: %+v", r)
	}
}
