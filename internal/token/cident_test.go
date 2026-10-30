package token_test

import (
	"testing"

	"zexi/internal/token"
)

func TestIsCIdentifier(t *testing.T) {
	ok := []string{"n", "total_count", "_x", "x1", "value2", "self", "s", "a_len"}
	for _, name := range ok {
		if !token.IsCIdentifier(name) {
			t.Errorf("IsCIdentifier(%q) = false, want true", name)
		}
	}
	// C keywords and anything that is not a plain identifier cannot be written
	// into a `c` body under its source name.
	bad := []string{"", "1x", "int", "float", "struct", "unsigned", "for", "return", "bool",
		"total count", "a-b", "naïve", "x.y", "x*y"}
	for _, name := range bad {
		if token.IsCIdentifier(name) {
			t.Errorf("IsCIdentifier(%q) = true, want false", name)
		}
	}
}

func TestIsCKeyword(t *testing.T) {
	for _, kw := range []string{"int", "static", "while", "_Bool"} {
		if !token.IsCKeyword(kw) {
			t.Errorf("IsCKeyword(%q) = false, want true", kw)
		}
	}
	for _, kw := range []string{"main", "Zexi", "zx_Foo", "len"} {
		if token.IsCKeyword(kw) {
			t.Errorf("IsCKeyword(%q) = true, want false", kw)
		}
	}
}
