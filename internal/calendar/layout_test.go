package calendar

import "testing"

// The layout helpers decide how overlapping meetings share the width of a day
// column, so they are exercised directly.

func TestWidthFractionsSingleMeeting(t *testing.T) {
	got := widthFractions([][2]int64{{0, 10}})
	assertInts(t, got, []int{1})
}

func TestWidthFractionsTwoOverlapping(t *testing.T) {
	got := widthFractions([][2]int64{{0, 10}, {5, 15}})
	assertInts(t, got, []int{2, 2})
}

func TestWidthFractionsSeparateGroupsAreIndependent(t *testing.T) {
	// Two overlap, then a third stands alone.
	got := widthFractions([][2]int64{{0, 10}, {5, 15}, {20, 30}})
	assertInts(t, got, []int{2, 2, 1})
}

func TestWidthFractionsTouchingRangesDoNotOverlap(t *testing.T) {
	got := widthFractions([][2]int64{{0, 10}, {10, 20}})
	assertInts(t, got, []int{1, 1})
}

func TestWidthFractionsThreeConcurrent(t *testing.T) {
	got := widthFractions([][2]int64{{0, 30}, {5, 15}, {10, 20}})
	assertInts(t, got, []int{3, 3, 3})
}

func TestHorizontalPositionsStack(t *testing.T) {
	got := horizontalPositions([][2]int64{{0, 10}, {5, 15}, {6, 16}})
	assertInts(t, got, []int{0, 1, 2})
}

func TestHorizontalPositionsReuseFreedTrack(t *testing.T) {
	// The third meeting starts after the first ends, so it reuses track 0.
	got := horizontalPositions([][2]int64{{0, 10}, {5, 15}, {10, 20}})
	assertInts(t, got, []int{0, 1, 0})
}

func TestLayoutHandlesEmptyDay(t *testing.T) {
	if got := widthFractions(nil); len(got) != 0 {
		t.Errorf("widthFractions(nil) = %v, want empty", got)
	}
	if got := horizontalPositions(nil); len(got) != 0 {
		t.Errorf("horizontalPositions(nil) = %v, want empty", got)
	}
}

func assertInts(t *testing.T, got, want []int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("index %d = %d, want %d (full: %v)", i, got[i], want[i], got)
		}
	}
}
