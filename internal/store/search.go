package store

import (
	"strings"
	"unicode"
)

// Full-text search is the one thing the engines share no syntax for, so each
// keeps its own index and the query is rendered for it here (ADR 19).
// PostgreSQL has a stored generated tsvector column called "search" with a GIN
// index; SQLite has an FTS5 external-content table named after the table with
// an "_fts" suffix, kept current by triggers. Both are created by
// schema/search.*.sql.
//
// Both indexes see the same words: the searched text has its punctuation
// turned into spaces, and is then split into runs of letters and digits and
// lowercased without stemming or stopwords. The search string is split the
// same way here, so a query matches the same rows on either engine. Raw input
// never reaches an engine's query parser: every word is quoted on the way out.

// searchable lists the tables with a search index, and the columns it covers.
// The columns are documentation here; the index definitions are the schema.
var searchable = map[string][]string{
	"g_thread":       {"subject_line"},
	"g_message":      {"body", "sender_name"},
	"contact":        {"name", "email"},
	"g_chat_message": {"body", "sender_name"},
}

// Query is a search string parsed once, in the boolean-mode syntax the MySQL
// backend accepted: words are alternatives, +word is required, -word is
// excluded, "a phrase" matches the words in order, and word* matches a prefix.
// When any term is required the alternatives no longer narrow the result,
// which is how MySQL treats them too.
type Query struct {
	required, optional, excluded []term
}

// term is one word or phrase: one or more words in order, the last of which
// may be a prefix.
type term struct {
	words  []string
	prefix bool
}

// ParseQuery parses a search string. Operators MySQL accepts but this does not
// need (~ < > and parentheses) are ignored rather than rejected.
func ParseQuery(s string) Query {
	var q Query
	r := []rune(s)
	for i := 0; i < len(r); {
		if unicode.IsSpace(r[i]) {
			i++
			continue
		}
		op := rune(0)
		for i < len(r) && strings.ContainsRune("+-~<>()", r[i]) {
			if r[i] == '+' || r[i] == '-' {
				op = r[i]
			}
			i++
		}
		if i >= len(r) {
			break
		}
		var t term
		if r[i] == '"' {
			end := i + 1
			for end < len(r) && r[end] != '"' {
				end++
			}
			t.words = words(string(r[i+1 : end]))
			i = end + 1
		} else {
			end := i
			for end < len(r) && !unicode.IsSpace(r[end]) && r[end] != '"' {
				end++
			}
			text := strings.TrimRight(string(r[i:end]), ")")
			t.prefix = strings.HasSuffix(text, "*")
			t.words = words(strings.TrimRight(text, "*"))
			i = end
		}
		if len(t.words) == 0 {
			continue
		}
		switch op {
		case '+':
			q.required = append(q.required, t)
		case '-':
			q.excluded = append(q.excluded, t)
		default:
			q.optional = append(q.optional, t)
		}
	}
	return q
}

// words splits text the way both search indexes do.
func words(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

// positive is the terms a row must match: every required term, or else at
// least one of the alternatives.
func (q Query) positive() ([]term, bool) {
	if len(q.required) > 0 {
		return q.required, true
	}
	return q.optional, false
}

// Empty reports whether the query has nothing a row could match, such as a
// string of operators or only excluded words. Such a query matches nothing.
func (q Query) Empty() bool {
	terms, _ := q.positive()
	return len(terms) == 0
}

// Match returns a condition on table that holds for the rows matching q, and
// the arguments it takes. The table must be referenced by its own name in the
// query, not an alias, and must be one with a search index.
func (db *DB) Match(table string, q Query) (string, []any) {
	if _, ok := searchable[table]; !ok || q.Empty() {
		return "FALSE", nil
	}
	if db.Engine == Postgres {
		return `"` + table + `"."search" @@ to_tsquery('simple', ?)`, []any{q.TSQuery()}
	}
	return `"` + table + `"."id" IN (SELECT "rowid" FROM "` + table + `_fts" WHERE "` +
		table + `_fts" MATCH ?)`, []any{q.FTS5()}
}

// TSQuery renders the query as a PostgreSQL tsquery.
func (q Query) TSQuery() string {
	render := func(t term) string {
		parts := make([]string, len(t.words))
		for i, w := range t.words {
			parts[i] = "'" + w + "'"
		}
		if t.prefix {
			parts[len(parts)-1] += ":*"
		}
		if len(parts) == 1 {
			return parts[0]
		}
		return "(" + strings.Join(parts, " <-> ") + ")"
	}
	terms, all := q.positive()
	var out string
	if all {
		out = join(terms, " & ", render)
	} else {
		out = "(" + join(terms, " | ", render) + ")"
	}
	if len(q.excluded) > 0 {
		out += " & !(" + join(q.excluded, " | ", render) + ")"
	}
	return out
}

// FTS5 renders the query as an SQLite FTS5 MATCH expression.
func (q Query) FTS5() string {
	render := func(t term) string {
		parts := make([]string, len(t.words))
		for i, w := range t.words {
			parts[i] = `"` + w + `"`
		}
		if t.prefix {
			parts[len(parts)-1] += "*"
		}
		return strings.Join(parts, " + ")
	}
	terms, all := q.positive()
	op := " OR "
	if all {
		op = " AND "
	}
	out := "(" + join(terms, op, render) + ")"
	if len(q.excluded) > 0 {
		out += " NOT (" + join(q.excluded, " OR ", render) + ")"
	}
	return out
}

func join(terms []term, sep string, render func(term) string) string {
	parts := make([]string, len(terms))
	for i, t := range terms {
		parts[i] = render(t)
	}
	return strings.Join(parts, sep)
}
