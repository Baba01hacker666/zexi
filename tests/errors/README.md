# Error fixtures

One `.zx` file per rejected program. `TestErrorFixtures` in
`internal/driver/error_fixtures_test.go` compiles each file as written and
requires that it fails with **exactly** the diagnostics its markers declare —
no missing ones, no undeclared ones.

A marker is a comment on any source line:

```zx
let x: i64 = "hi"; // error: E0308 mismatched types: expected `i64`, found `str`
```

- `// error: <CODE> [message substring]` declares an error diagnostic;
  `// warning: <CODE> [...]` declares a warning.
- The code is required; the message substring is optional but worth keeping —
  it pins down *which* message with that code you expect.
- Markers match diagnostics one-to-one, in any order. If the compiler starts
  emitting an extra diagnostic (even a helpful one), the fixture fails until
  the marker is added — so the suite stays precise.
- Fixtures live here only if they must fail. Positive programs belong in
  `tests/*.zx` (checked by `internal/driver`'s tests).

To grow coverage, drop in a new `.zx` file with its markers — no Go code
needed. Markers require a diagnostic code, so give any uncoded diagnostic a
stable `E....` code in `internal/diag` first.
