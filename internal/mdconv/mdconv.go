// Package mdconv converts the HTML body a client composes into the plain
// markdown stored in g_message.body / g_chat_message.body. It is the Go
// counterpart of league/html-to-markdown as the PHP backend configures it:
// script and style nodes are removed, and unrecognised tags are stripped while
// their text is kept.
package mdconv

import (
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var whitespace = regexp.MustCompile(`\s+`)

// ToMarkdown converts an HTML fragment to markdown. Input that does not parse
// as HTML degrades to its text content rather than failing.
func ToMarkdown(input string) string {
	if strings.TrimSpace(input) == "" {
		return ""
	}
	nodes, err := html.ParseFragment(strings.NewReader(input), &html.Node{
		Type:     html.ElementNode,
		Data:     "body",
		DataAtom: atom.Body,
	})
	if err != nil {
		return collapse(input)
	}
	var b strings.Builder
	for _, n := range nodes {
		walk(&b, n, state{})
	}
	return tidy(b.String())
}

// Summary renders the first 200 characters of the markdown on a single line,
// which is what g_thread.body_summary holds.
func Summary(markdown string) string {
	s := collapse(markdown)
	if len([]rune(s)) <= 200 {
		return s
	}
	return string([]rune(s)[:200])
}

type state struct {
	quote  int  // retained so nested quotes do not re-enter the outer prefix
	inPre  bool // inside <pre>, so text is copied verbatim
	listNo int  // 0 = unordered, >0 = next number in an ordered list
	inList bool
}

func walk(b *strings.Builder, n *html.Node, st state) {
	switch n.Type {
	case html.TextNode:
		if st.inPre {
			b.WriteString(n.Data)
			return
		}
		text := collapse(n.Data)
		if text == "" {
			// Preserve a single separating space between inline elements.
			if n.Data != "" && strings.TrimSpace(n.Data) == "" && endsWithWord(b) {
				b.WriteString(" ")
			}
			return
		}
		if strings.HasPrefix(n.Data, " ") && endsWithWord(b) {
			b.WriteString(" ")
		}
		b.WriteString(text)
		if strings.HasSuffix(n.Data, " ") {
			b.WriteString(" ")
		}
		return
	case html.ElementNode:
		element(b, n, st)
		return
	case html.DocumentNode:
	default:
		// Comments and doctypes carry no content.
		return
	}
	children(b, n, st)
}

func element(b *strings.Builder, n *html.Node, st state) {
	switch n.DataAtom {
	case atom.Script, atom.Style, atom.Head, atom.Title:
		return // remove_nodes
	case atom.Br:
		b.WriteString("\n")
		return
	case atom.Hr:
		block(b)
		b.WriteString("---")
		block(b)
		return
	case atom.Img:
		alt := attr(n, "alt")
		src := attr(n, "src")
		if src != "" {
			b.WriteString("![" + alt + "](" + src + ")")
		} else if alt != "" {
			b.WriteString(alt)
		}
		return
	case atom.P, atom.Div, atom.Section, atom.Article:
		block(b)
		children(b, n, st)
		block(b)
		return
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		level := int(n.Data[1] - '0')
		block(b)
		b.WriteString(strings.Repeat("#", level) + " ")
		children(b, n, st)
		block(b)
		return
	case atom.Strong, atom.B:
		wrap(b, n, st, "**")
		return
	case atom.Em, atom.I:
		wrap(b, n, st, "*")
		return
	case atom.Del, atom.S:
		wrap(b, n, st, "~~")
		return
	case atom.Code:
		if st.inPre {
			children(b, n, st)
			return
		}
		wrap(b, n, st, "`")
		return
	case atom.Pre:
		block(b)
		b.WriteString("```\n")
		inner := state{quote: st.quote, inPre: true}
		children(b, n, inner)
		b.WriteString("\n```")
		block(b)
		return
	case atom.A:
		href := attr(n, "href")
		var inner strings.Builder
		children(&inner, n, st)
		text := strings.TrimSpace(inner.String())
		switch {
		case href == "":
			b.WriteString(text)
		case text == "":
			b.WriteString("<" + href + ">")
		case text == href:
			b.WriteString("<" + href + ">")
		default:
			b.WriteString("[" + text + "](" + href + ")")
		}
		return
	case atom.Blockquote:
		var quoted strings.Builder
		inner := st
		inner.quote = 0
		children(&quoted, n, inner)
		block(b)
		b.WriteString(prefixLines(tidy(quoted.String()), "> "))
		block(b)
		return
	case atom.Ul, atom.Ol:
		inner := st
		inner.inList = true
		inner.listNo = 0
		if n.DataAtom == atom.Ol {
			inner.listNo = 1
		}
		block(b)
		listItems(b, n, inner)
		block(b)
		return
	case atom.Table, atom.Tbody, atom.Thead, atom.Tr:
		children(b, n, st)
		return
	case atom.Td, atom.Th:
		children(b, n, st)
		b.WriteString(" ")
		return
	default:
		// strip_tags: unknown element, keep the content.
		children(b, n, st)
		return
	}
}

func listItems(b *strings.Builder, list *html.Node, st state) {
	number := st.listNo
	for c := list.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode || c.DataAtom != atom.Li {
			walk(b, c, st)
			continue
		}
		newline(b)
		if st.listNo > 0 {
			b.WriteString(strconv.Itoa(number) + ". ")
			number++
		} else {
			b.WriteString("- ")
		}
		children(b, c, st)
	}
}

func wrap(b *strings.Builder, n *html.Node, st state, marker string) {
	var inner strings.Builder
	children(&inner, n, st)
	text := inner.String()
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		b.WriteString(text)
		return
	}
	if strings.HasPrefix(text, " ") {
		b.WriteString(" ")
	}
	b.WriteString(marker + trimmed + marker)
	if strings.HasSuffix(text, " ") {
		b.WriteString(" ")
	}
}

func children(b *strings.Builder, n *html.Node, st state) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(b, c, st)
	}
}

// block ends the current line and leaves one blank line, which is how markdown
// separates block-level elements.
func block(b *strings.Builder) {
	s := b.String()
	if s == "" {
		return
	}
	trimmed := strings.TrimRight(s, " \t")
	if !strings.HasSuffix(trimmed, "\n\n") {
		if strings.HasSuffix(trimmed, "\n") {
			b.WriteString("\n")
		} else {
			b.WriteString("\n\n")
		}
	}
}

func newline(b *strings.Builder) {
	if b.Len() == 0 {
		return
	}
	if !strings.HasSuffix(b.String(), "\n") {
		b.WriteString("\n")
	}
}

// prefixLines puts marker in front of every line, which is how markdown marks a
// block quote.
func prefixLines(s, marker string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l == "" {
			lines[i] = strings.TrimRight(marker, " ")
			continue
		}
		lines[i] = marker + l
	}
	return strings.Join(lines, "\n")
}

func endsWithWord(b *strings.Builder) bool {
	s := b.String()
	if s == "" {
		return false
	}
	last := s[len(s)-1]
	return last != ' ' && last != '\n' && last != '>'
}

func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

func collapse(s string) string {
	return strings.TrimSpace(whitespace.ReplaceAllString(s, " "))
}

// tidy normalises trailing whitespace and collapses runs of blank lines.
func tidy(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	out := strings.Join(lines, "\n")
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(out)
}
