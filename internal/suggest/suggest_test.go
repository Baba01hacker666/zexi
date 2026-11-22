package suggest

import (
	"strings"
	"testing"
)

func TestDistanceIgnoresCase(t *testing.T) {
	if d := Distance("Math", "math"); d != 0 {
		t.Fatalf("Distance(Math, math) = %d, want 0", d)
	}
	if d := Distance("pritnln", "println"); d != 2 {
		t.Fatalf("Distance(pritnln, println) = %d, want 2", d)
	}
}

func TestNameSuggestsCloseCandidate(t *testing.T) {
	got := Name("doulbe", []string{"double", "triple"})
	if !strings.Contains(got, "`double`") {
		t.Fatalf("Name = %q, want a suggestion of double", got)
	}
	if got := Name("zzz", []string{"double", "triple"}); got != "" {
		t.Fatalf("Name(zzz) = %q, want no suggestion", got)
	}
}

func TestClosestTieIsStable(t *testing.T) {
	// Both candidates are one edit away; sorted order decides, so repeated
	// calls over a reordered slice agree.
	a := Closest("ax", []string{"ab", "ac"})
	b := Closest("ax", []string{"ac", "ab"})
	if a != b {
		t.Fatalf("Closest is not stable: %q vs %q", a, b)
	}
	if a != "ab" {
		t.Fatalf("Closest = %q, want ab", a)
	}
}

func TestClosestSkipsExactName(t *testing.T) {
	if got := Closest("same", []string{"same", "sample"}); got != "sample" {
		t.Fatalf("Closest = %q, want sample", got)
	}
}
