package store_test

import (
	"context"
	"sort"
	"testing"

	"github.com/103mail/webmail-server/internal/store"
	"github.com/103mail/webmail-server/internal/store/storetest"
)

// One parsed query rendered for both engines, so the two cannot drift apart
// without this failing (ADR 19).
func TestQueryRendersForBothEngines(t *testing.T) {
	tests := []struct {
		in, tsquery, fts5 string
	}{
		{"invoice", "('invoice')", `("invoice")`},
		{"invoice march", "('invoice' | 'march')", `("invoice" OR "march")`},
		{"+invoice +march", "'invoice' & 'march'", `("invoice" AND "march")`},
		{"+invoice march", "'invoice'", `("invoice")`},
		{"invoice -draft", "('invoice') & !('draft')", `("invoice") NOT ("draft")`},
		{"+invoice -draft -spam", "'invoice' & !('draft' | 'spam')", `("invoice") NOT ("draft" OR "spam")`},
		{"inv*", "('inv':*)", `("inv"*)`},
		{`"quarterly report"`, "(('quarterly' <-> 'report'))", `("quarterly" + "report")`},
		{`+"quarterly report" -"draft copy"`, "('quarterly' <-> 'report') & !(('draft' <-> 'copy'))",
			`("quarterly" + "report") NOT ("draft" + "copy")`},
		{"alice@example.com", "(('alice' <-> 'example' <-> 'com'))", `("alice" + "example" + "com")`},
		{"Héllo WORLD", "('héllo' | 'world')", `("héllo" OR "world")`},
		{"~fancy >more <less (grouped)", "('fancy' | 'more' | 'less' | 'grouped')",
			`("fancy" OR "more" OR "less" OR "grouped")`},
		// Quotes and FTS5 or tsquery operators in the input are never passed on.
		{`it's a "trap' OR 1=1 --`, "(('it' <-> 's') | 'a' | ('trap' <-> 'or' <-> '1' <-> '1'))",
			`("it" + "s" OR "a" OR "trap" + "or" + "1" + "1")`},
		{"NEAR(a b) col:x ^y", "(('near' <-> 'a') | 'b' | ('col' <-> 'x') | 'y')",
			`("near" + "a" OR "b" OR "col" + "x" OR "y")`},
	}
	for _, tc := range tests {
		q := store.ParseQuery(tc.in)
		if got := q.TSQuery(); got != tc.tsquery {
			t.Errorf("TSQuery(%q)\n got %s\nwant %s", tc.in, got, tc.tsquery)
		}
		if got := q.FTS5(); got != tc.fts5 {
			t.Errorf("FTS5(%q)\n got %s\nwant %s", tc.in, got, tc.fts5)
		}
	}
}

func TestEmptyQueries(t *testing.T) {
	for _, in := range []string{"", "   ", "+", "-", `""`, "***", "-only -excluded", "+-~"} {
		if q := store.ParseQuery(in); !q.Empty() {
			t.Errorf("ParseQuery(%q) is not empty", in)
		}
	}
	if store.ParseQuery("x").Empty() {
		t.Error(`ParseQuery("x") is empty`)
	}
}

// The same searches find the same rows on the engine under test, through the
// real indexes and the triggers or generated columns that keep them current.
func TestMatch(t *testing.T) {
	db := storetest.Open(t)
	ctx := context.Background()
	thread := func(subject string) int64 {
		id, err := store.Insert(ctx, db, `INSERT INTO "g_thread" ("subject_line") VALUES (?)`, subject)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	quarterly := thread("Quarterly report, final")
	draft := thread("Quarterly report draft")
	invoice := thread("Invoice for March from billing@example.com")
	accent := thread("Café opening")
	edited := thread("Placeholder")
	if _, err := db.ExecContext(ctx, `UPDATE "g_thread" SET "subject_line" = ? WHERE "id" = ?`,
		"Renamed agenda", edited); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		search string
		want   []int64
	}{
		{"quarterly", []int64{quarterly, draft}},
		{"QUARTERLY", []int64{quarterly, draft}},
		{"quarterly -draft", []int64{quarterly}},
		{"+quarterly +final", []int64{quarterly}},
		{"final invoice", []int64{quarterly, invoice}},
		{"quart*", []int64{quarterly, draft}},
		{`"report final"`, []int64{quarterly}},
		{`"final report"`, nil},
		{"billing@example.com", []int64{invoice}},
		{"example", []int64{invoice}},
		{"café", []int64{accent}},
		{"cafe", nil},
		{"placeholder", nil},
		{"agenda", []int64{edited}},
		{"report,", []int64{quarterly, draft}},
		{"-quarterly", nil},
		{`"`, nil},
	}
	for _, tc := range tests {
		cond, args := db.Match("g_thread", store.ParseQuery(tc.search))
		rows, err := db.QueryContext(ctx, `SELECT "id" FROM "g_thread" WHERE `+cond+` ORDER BY "id"`, args...)
		if err != nil {
			t.Fatalf("search %q: %v", tc.search, err)
		}
		var got []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			got = append(got, id)
		}
		rows.Close()
		sort.Slice(tc.want, func(i, j int) bool { return tc.want[i] < tc.want[j] })
		if len(got) != len(tc.want) || (len(got) > 0 && !equal(got, tc.want)) {
			t.Errorf("search %q found %v, want %v", tc.search, got, tc.want)
		}
	}

	// Deleting a row takes it out of the index.
	if _, err := db.ExecContext(ctx, `DELETE FROM "g_thread" WHERE "id" = ?`, draft); err != nil {
		t.Fatal(err)
	}
	cond, args := db.Match("g_thread", store.ParseQuery("draft"))
	if n, err := store.SelectInt(ctx, db, `SELECT COUNT(*) FROM "g_thread" WHERE `+cond, args...); err != nil || n != 0 {
		t.Errorf("deleted thread still found: %d, %v", n, err)
	}
}

// Every searchable table has an index the condition can use.
func TestMatchEveryTable(t *testing.T) {
	db := storetest.Open(t)
	ctx := context.Background()
	customer, err := store.Insert(ctx, db, `INSERT INTO "customer" ("name") VALUES (?)`, "c")
	if err != nil {
		t.Fatal(err)
	}
	thread, err := store.Insert(ctx, db, `INSERT INTO "g_thread" ("subject_line") VALUES (?)`, "s")
	if err != nil {
		t.Fatal(err)
	}
	chat, err := store.Insert(ctx, db, `INSERT INTO "g_chat" ("type", "created_by_email") VALUES ('direct', ?)`, "a@x.test")
	if err != nil {
		t.Fatal(err)
	}
	inserts := map[string]struct {
		query string
		args  []any
	}{
		"g_message": {`INSERT INTO "g_message" ("g_thread_id", "sender_email", "sender_name", "body") VALUES (?,?,?,?)`,
			[]any{thread, "a@x.test", "Needle Sender", "body text"}},
		"contact": {`INSERT INTO "contact" ("customer_id", "email", "name") VALUES (?,?,?)`,
			[]any{customer, "needle@x.test", "Someone"}},
		"g_chat_message": {`INSERT INTO "g_chat_message" ("g_chat_id", "sender_email", "sender_name", "body") VALUES (?,?,?,?)`,
			[]any{chat, "a@x.test", "A", "a needle in a haystack"}},
	}
	for table, ins := range inserts {
		if _, err := db.ExecContext(ctx, ins.query, ins.args...); err != nil {
			t.Fatalf("insert into %s: %v", table, err)
		}
		cond, args := db.Match(table, store.ParseQuery("needle"))
		n, err := store.SelectInt(ctx, db, `SELECT COUNT(*) FROM "`+table+`" WHERE `+cond, args...)
		if err != nil {
			t.Fatalf("search %s: %v", table, err)
		}
		if n != 1 {
			t.Errorf("search %s found %d rows, want 1", table, n)
		}
	}
	if cond, _ := db.Match("user", store.ParseQuery("x")); cond != "FALSE" {
		t.Errorf("a table without an index matched with %q", cond)
	}
}

func equal(a, b []int64) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
