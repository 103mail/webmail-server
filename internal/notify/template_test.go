package notify

import "testing"

func TestRender(t *testing.T) {
	params := map[string]string{"name": "Joost", "empty": ""}
	tests := []struct {
		name     string
		template string
		escape   bool
		want     string
	}{
		{"no placeholder", "Hello", true, "Hello"},
		{"substitution", "Hello {{name}}", true, "Hello Joost"},
		{"spaces tolerated", "Hello {{ name }}", true, "Hello Joost"},
		{"repeated", "{{name}} and {{name}}", true, "Joost and Joost"},
		{"unknown renders empty", "Hello {{nope}}!", true, "Hello !"},
		{"empty value", "[{{empty}}]", true, "[]"},
		{"unclosed stays literal", "Hello {{name", true, "Hello {{name"},
		{"escaping off", "{{name}}", false, "Joost"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := render(tc.template, params, tc.escape); got != tc.want {
				t.Errorf("render(%q) = %q, want %q", tc.template, got, tc.want)
			}
		})
	}
}

func TestRenderEscapesHTML(t *testing.T) {
	// A parameter is attacker-influenced: a display name comes from whoever
	// registered it.
	params := map[string]string{"name": `<script>alert("x")</script>`}
	got := render("Hi {{name}}", params, true)
	want := "Hi &lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;"
	if got != want {
		t.Errorf("render = %q, want %q", got, want)
	}
}
