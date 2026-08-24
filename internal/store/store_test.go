package store

import (
	"reflect"
	"testing"
)

func TestPlaceholders(t *testing.T) {
	tests := map[int]string{0: "NULL", 1: "?", 3: "?,?,?"}
	for n, want := range tests {
		if got := Placeholders(n); got != want {
			t.Errorf("Placeholders(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestArgs(t *testing.T) {
	if got := Args([]int64{1, 2}); !reflect.DeepEqual(got, []any{int64(1), int64(2)}) {
		t.Errorf("Args = %v", got)
	}
	if got := Args(nil); len(got) != 0 {
		t.Errorf("Args(nil) = %v, want empty", got)
	}
}
