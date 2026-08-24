package mdconv

import "testing"

func TestToMarkdown(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain text", "<p>Hello</p>", "Hello"},
		{"two paragraphs", "<p>One</p><p>Two</p>", "One\n\nTwo"},
		{"bold", "<p>a <strong>b</strong> c</p>", "a **b** c"},
		{"italic", "<p>a <em>b</em> c</p>", "a *b* c"},
		{"link", `<p><a href="https://x.test">site</a></p>`, "[site](https://x.test)"},
		{"bare link", `<p><a href="https://x.test">https://x.test</a></p>`, "<https://x.test>"},
		{"heading", "<h2>Title</h2>", "## Title"},
		{"line break", "<p>a<br>b</p>", "a\nb"},
		{"unordered list", "<ul><li>a</li><li>b</li></ul>", "- a\n- b"},
		{"ordered list", "<ol><li>a</li><li>b</li></ol>", "1. a\n2. b"},
		{"blockquote", "<blockquote><p>quoted</p></blockquote>", "> quoted"},
		{"code", "<p>use <code>x</code></p>", "use `x`"},
		{"script removed", "<p>a</p><script>alert(1)</script>", "a"},
		{"style removed", "<style>p{color:red}</style><p>a</p>", "a"},
		{"unknown tag stripped", "<p>a <span>b</span> c</p>", "a b c"},
		{"entities decoded", "<p>a &amp; b &lt; c</p>", "a & b < c"},
		{"whitespace collapsed", "<p>a   \n  b</p>", "a b"},
		{"image with alt", `<p><img src="x.png" alt="pic"></p>`, "![pic](x.png)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ToMarkdown(tc.in); got != tc.want {
				t.Errorf("ToMarkdown(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSummaryTruncatesToRunes(t *testing.T) {
	long := ""
	for i := 0; i < 300; i++ {
		long += "é"
	}
	if n := len([]rune(Summary(long))); n != 200 {
		t.Errorf("Summary length = %d runes, want 200", n)
	}
}

func TestSummaryCollapsesNewlines(t *testing.T) {
	if got, want := Summary("a\n\nb"), "a b"; got != want {
		t.Errorf("Summary = %q, want %q", got, want)
	}
}

func TestNestedBlockquote(t *testing.T) {
	got := ToMarkdown("<blockquote><p>outer</p><blockquote><p>inner</p></blockquote></blockquote>")
	want := "> outer\n>\n> > inner"
	if got != want {
		t.Errorf("nested quote\n got %q\nwant %q", got, want)
	}
}

func TestQuoteWithMultipleParagraphs(t *testing.T) {
	got := ToMarkdown("<blockquote><p>a</p><p>b</p></blockquote>")
	want := "> a\n>\n> b"
	if got != want {
		t.Errorf("multi-paragraph quote\n got %q\nwant %q", got, want)
	}
}

func TestMalformedHTMLDegradesToText(t *testing.T) {
	if got := ToMarkdown("<p>unclosed"); got != "unclosed" {
		t.Errorf("got %q, want %q", got, "unclosed")
	}
}
