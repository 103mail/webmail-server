package notify

import (
	"html"
	"strings"
)

// render substitutes {{name}} placeholders, which is the subset of MintyPHP's
// Template syntax that email templates use.
//
// Values are HTML-escaped, matching Template::render's default escape of
// "html". A placeholder with no matching parameter renders as empty, which is
// how a template with an optional field degrades.
func render(template string, params map[string]string, escape bool) string {
	if !strings.Contains(template, "{{") {
		return template
	}
	var b strings.Builder
	rest := template
	for {
		before, after, found := strings.Cut(rest, "{{")
		b.WriteString(before)
		if !found {
			break
		}
		name, remainder, closed := strings.Cut(after, "}}")
		if !closed {
			// An unclosed placeholder is literal text, not an error: the
			// template is data and must not be able to fail a send.
			b.WriteString("{{")
			b.WriteString(after)
			break
		}
		value := params[strings.TrimSpace(name)]
		if escape {
			value = html.EscapeString(value)
		}
		b.WriteString(value)
		rest = remainder
	}
	return b.String()
}
