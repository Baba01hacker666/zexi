package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"zexi/internal/driver"
)

// TestRunCacheKeyTracksInputs checks that the key is stable for unchanged
// inputs and changes when either the source or a compilation option does.
func TestRunCacheKeyTracksInputs(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "main.zx")
	if err := os.WriteFile(src, []byte("fn main() { println(1); }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := driver.Options{OptLevel: "2"}
	k1, ok := runCacheKey([]string{src}, opts)
	if !ok || k1 == "" {
		t.Fatal("runCacheKey failed on a readable file")
	}
	k2, _ := runCacheKey([]string{src}, opts)
	if k1 != k2 {
		t.Fatalf("key changed without a change: %s vs %s", k1, k2)
	}
	kOtherOpt, _ := runCacheKey([]string{src}, driver.Options{OptLevel: "0"})
	if kOtherOpt == k1 {
		t.Fatal("optimization level is not part of the key")
	}
	if err := os.WriteFile(src, []byte("fn main() { println(2); }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	k3, _ := runCacheKey([]string{src}, opts)
	if k3 == k1 {
		t.Fatal("source content is not part of the key")
	}
}

// TestRunCacheStoreAndLookup publishes a build and finds it again, without
// leaving the temporary build path behind.
func TestRunCacheStoreAndLookup(t *testing.T) {
	dir := t.TempDir()
	built := filepath.Join(dir, "build.tmp")
	if err := os.WriteFile(built, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("ab", 32)
	got, ok := cacheStore(dir, key, built)
	if !ok {
		t.Fatal("cacheStore failed")
	}
	if _, err := os.Stat(built); !os.IsNotExist(err) {
		t.Fatalf("the temporary build still exists at %s", built)
	}
	hit, ok := cacheLookup(dir, key)
	if !ok || hit != got {
		t.Fatalf("cacheLookup = %q, %v; want %q", hit, ok, got)
	}
	// Publishing the same key again is harmless and returns the same path.
	built2 := filepath.Join(dir, "build2.tmp")
	if err := os.WriteFile(built2, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	again, ok := cacheStore(dir, key, built2)
	if !ok || again != got {
		t.Fatalf("second store = %q, %v; want the existing entry", again, ok)
	}
}
