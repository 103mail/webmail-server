package api

import (
	"reflect"
	"testing"
)

func TestSplitPath(t *testing.T) {
	tests := []struct {
		path   string
		entity string
		id     string
	}{
		{"/api/v1/email/mailbox/1", "mailbox", "1"},
		{"/api/v1/email/mailbox", "mailbox", ""},
		{"/api/v1/auth/token", "token", ""},
		{"/api/v1/calendar/meeting/1,2,3", "meeting", "1,2,3"},
		{"/api/v1/email/", "", ""},
		{"/api/v1/email", "", ""},
	}
	for _, tc := range tests {
		entity, id := splitPath(tc.path)
		if entity != tc.entity || id != tc.id {
			t.Errorf("splitPath(%q) = %q, %q; want %q, %q", tc.path, entity, id, tc.entity, tc.id)
		}
	}
}

func TestParseID(t *testing.T) {
	tests := map[string]int64{"1": 1, "42": 42, "0": 0, "": 0, "abc": 0, "-1": 0, "1.5": 0}
	for in, want := range tests {
		if got := parseID(in); got != want {
			t.Errorf("parseID(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseIDs(t *testing.T) {
	got, ok := parseIDs("1,2,3")
	if !ok || !reflect.DeepEqual(got, []int64{1, 2, 3}) {
		t.Errorf("parseIDs(\"1,2,3\") = %v, %v", got, ok)
	}
	for _, bad := range []string{"", "1,,2", "1,x", "0", "1,0"} {
		if _, ok := parseIDs(bad); ok {
			t.Errorf("parseIDs(%q) succeeded, want rejection", bad)
		}
	}
}

func TestDecodeBase64(t *testing.T) {
	// Standard, raw and URL alphabets all appear in practice.
	for _, in := range []string{"aGVsbG8=", "aGVsbG8", "aGVsbG8="} {
		got, err := decodeBase64(in)
		if err != nil || string(got) != "hello" {
			t.Errorf("decodeBase64(%q) = %q, %v", in, got, err)
		}
	}
	if got, err := decodeBase64(""); err != nil || got != nil {
		t.Errorf("decodeBase64(\"\") = %v, %v; want nil, nil", got, err)
	}
	if _, err := decodeBase64("not valid base64!!"); err == nil {
		t.Error("decodeBase64 accepted invalid input, want an error")
	}
}

func TestRequestBoolPtrMatchesPHPBooleanFilter(t *testing.T) {
	truthy := []any{true, "1", "true", "TRUE", "on", "yes", 1.0}
	for _, v := range truthy {
		r := &request{Data: map[string]any{"k": v}}
		if p := r.boolPtr("k"); p == nil || !*p {
			t.Errorf("boolPtr(%v) = %v, want true", v, p)
		}
	}
	falsy := []any{false, "0", "false", "off", "no", "", 0.0}
	for _, v := range falsy {
		r := &request{Data: map[string]any{"k": v}}
		if p := r.boolPtr("k"); p == nil || *p {
			t.Errorf("boolPtr(%v) = %v, want false", v, p)
		}
	}
	// An absent key must stay absent so the field is left untouched.
	r := &request{Data: map[string]any{}}
	if p := r.boolPtr("k"); p != nil {
		t.Errorf("boolPtr on absent key = %v, want nil", p)
	}
}

func TestRequestStrings(t *testing.T) {
	r := &request{Data: map[string]any{"a": []any{"x", "y"}, "b": "z", "c": "", "d": 1.0}}
	if got, ok := r.strings("a"); !ok || !reflect.DeepEqual(got, []string{"x", "y"}) {
		t.Errorf("strings(a) = %v, %v", got, ok)
	}
	if got, ok := r.strings("b"); !ok || !reflect.DeepEqual(got, []string{"z"}) {
		t.Errorf("strings(b) = %v, %v", got, ok)
	}
	if got, ok := r.strings("c"); !ok || got != nil {
		t.Errorf("strings(c) = %v, %v; want nil, true", got, ok)
	}
	if _, ok := r.strings("d"); ok {
		t.Error("strings on a number succeeded, want rejection")
	}
	if got, ok := r.strings("missing"); !ok || got != nil {
		t.Errorf("strings(missing) = %v, %v; want nil, true", got, ok)
	}
}

func TestRequestAttachments(t *testing.T) {
	r := &request{Data: map[string]any{"attachments": []any{
		map[string]any{"fileName": "a.txt", "mimeType": "text/plain", "fileData": "aGk="},
	}}}
	got, ok := r.attachments()
	if !ok || len(got) != 1 {
		t.Fatalf("attachments = %v, %v", got, ok)
	}
	if got[0].FileName != "a.txt" || string(got[0].FileData) != "hi" {
		t.Errorf("attachment = %+v", got[0])
	}

	// A missing fileName or mimeType is a bad request, as is a non-array.
	for _, bad := range []any{
		[]any{map[string]any{"mimeType": "text/plain"}},
		[]any{map[string]any{"fileName": "a.txt"}},
		"not an array",
		[]any{"not an object"},
	} {
		r := &request{Data: map[string]any{"attachments": bad}}
		if _, ok := r.attachments(); ok {
			t.Errorf("attachments(%v) succeeded, want rejection", bad)
		}
	}
}

func TestValuesToMapHandlesRepeatedAndBracketKeys(t *testing.T) {
	got := valuesToMap(map[string][]string{
		"single":      {"a"},
		"repeated":    {"a", "b"},
		"bracketed[]": {"a"},
	})
	if got["single"] != "a" {
		t.Errorf("single = %v", got["single"])
	}
	if !reflect.DeepEqual(got["repeated"], []any{"a", "b"}) {
		t.Errorf("repeated = %v", got["repeated"])
	}
	if !reflect.DeepEqual(got["bracketed"], []any{"a"}) {
		t.Errorf("bracketed = %v", got["bracketed"])
	}
}
