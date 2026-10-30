package main

import "testing"

// TestParseArgsRejectsUnknownFlags keeps a mistyped flag from silently building
// the program with different settings than the user asked for.
func TestParseArgsRejectsUnknownFlags(t *testing.T) {
	c := parseArgs([]string{"--opt-lelvel=2", "main.zx"})
	if len(c.unknown) != 1 || c.unknown[0] != "--opt-lelvel=2" {
		t.Fatalf("unknown = %v, want the mistyped flag", c.unknown)
	}
	if c.optLevel != "" {
		t.Fatalf("optLevel = %q, want it untouched", c.optLevel)
	}
	// Program arguments after `--` are not compiler flags.
	c = parseArgs([]string{"main.zx", "--", "--not-a-flag"})
	if len(c.unknown) != 0 {
		t.Fatalf("program arguments were treated as flags: %v", c.unknown)
	}
	if len(c.progArgs) != 1 || c.progArgs[0] != "--not-a-flag" {
		t.Fatalf("progArgs = %v", c.progArgs)
	}
}

// TestParseArgsOptimizationShorthand accepts the spellings people type.
func TestParseArgsOptimizationShorthand(t *testing.T) {
	for _, tc := range []struct{ arg, want string }{
		{"-O0", "0"}, {"-O2", "2"}, {"-Os", "s"}, {"-Oz", "z"},
		{"--opt-level=3", "3"},
	} {
		c := parseArgs([]string{tc.arg, "main.zx"})
		if c.optLevel != tc.want {
			t.Errorf("%s gave optLevel %q, want %q", tc.arg, c.optLevel, tc.want)
		}
		if len(c.unknown) != 0 {
			t.Errorf("%s was reported as unknown", tc.arg)
		}
	}
}

// TestParseArgsAdd checks the module-manager flags.
func TestParseArgsAdd(t *testing.T) {
	c := parseArgs([]string{"sqlite", "--force", "--dir=vendor"})
	if len(c.positional) != 1 || c.positional[0] != "sqlite" {
		t.Fatalf("positional = %v", c.positional)
	}
	if !c.force || c.addDir != "vendor" {
		t.Fatalf("force=%v dir=%q", c.force, c.addDir)
	}
	if c := parseArgs([]string{"--list"}); !c.list {
		t.Fatal("--list was not recorded")
	}
}
