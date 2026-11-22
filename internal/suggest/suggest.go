// Package suggest turns a misspelled name into a "did you mean" hint.
//
// Every part of the compiler that reports an unknown name — the checker, the
// module loader, and the package registry — uses this one implementation, so a
// typo gets the same answer no matter which layer noticed it. Matching ignores
// case, because `Math` for `math` is a spelling slip, not a different name.
package suggest

import (
	"fmt"
	"sort"
	"strings"
)

// Distance is the Levenshtein edit distance between two names, compared
// case-insensitively. It is the metric Closest uses; it is exported for callers
// that need to rank candidates themselves.
func Distance(a, b string) int {
	a, b = strings.ToLower(a), strings.ToLower(b)
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

// Closest returns the candidate nearest to name, or "" when none is close
// enough. An exact case-insensitive match is preferred over an edit-distance
// match, and candidates are considered in sorted order so a tie is stable
// across runs (callers build candidate lists from Go maps, whose order is
// randomized).
func Closest(name string, candidates []string) string {
	sorted := make([]string, 0, len(candidates))
	seen := map[string]bool{}
	for _, cand := range candidates {
		if cand == "" || cand == name || seen[cand] {
			continue
		}
		seen[cand] = true
		sorted = append(sorted, cand)
	}
	sort.Strings(sorted)
	best, bestD := "", 3
	for _, cand := range sorted {
		d := Distance(name, cand)
		if d < bestD {
			best, bestD = cand, d
		}
	}
	return best
}

// Name returns " (did you mean `x`?)" for a misspelled name, and "" when no
// candidate is within two edits. It is the shape the checker appends to an
// unknown-name diagnostic.
func Name(name string, candidates []string) string {
	best := Closest(name, candidates)
	if best == "" {
		return ""
	}
	return fmt.Sprintf(" (did you mean `%s`?)", best)
}

func min3(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}
