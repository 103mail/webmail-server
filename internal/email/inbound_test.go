package email

import "testing"

func TestDomainOf(t *testing.T) {
	tests := map[string]string{
		"a@example.test":   "example.test",
		"a@b@example.test": "example.test",
		"noatsign":         "noatsign",
		"@example.test":    "@example.test",
	}
	for in, want := range tests {
		if got := DomainOf(in); got != want {
			t.Errorf("DomainOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecodeNumericEntities(t *testing.T) {
	tests := map[string]string{
		// A well-formed address is untouched.
		"maurits@vdschee.nl": "maurits@vdschee.nl",
		// The obfuscation senders actually use.
		"sales@co&#109;mercial.ng": "sales@commercial.ng",
		"a&#46;b@example.test":     "a.b@example.test",
		// A decoded character that cannot appear in an address is neutralised.
		"a&#64;b": "a_b",
		// Out of range and non-ASCII collapse rather than corrupting.
		"x&#9999;y": "x_y",
		// Not an entity at all.
		"a&b":  "a&b",
		"&#1;": "&#1;",
	}
	for in, want := range tests {
		if got := DecodeNumericEntities(in); got != want {
			t.Errorf("DecodeNumericEntities(%q) = %q, want %q", in, got, want)
		}
	}
}
