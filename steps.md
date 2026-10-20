# Zexi — what to do next

Written 2026-10-20. Every fact below marked **[verified]** was checked by running a
command in this repo on that date; the command is given so you can re-check it.
Facts marked **[carried]** come from work already committed and covered by the
passing CI run, but were not re-executed while writing this file.

---

## 0. Current state — read this first

### 0.1 The branch is pushed and green

```
$ git log --oneline -7
f9003f6 docs: document Vec<T>, the attributes, and string ordering
1e53881 feat: allow ordering comparisons on str and String
f09ed2e perf: give String a capacity so appends are amortized O(1)
488ab29 fix: reserve the predeclared type names
082fffe feat: implement the M1 attributes (repr(C), inline, no_mangle, export)
4c11cd4 feat: add the builtin growable array Vec<T> (roadmap M1 collections)
354e0ad fix: repair two name-binding miscompilations and add a real panic runtime
68d9262 lint: adopt a curated golangci-lint config and fix its findings   <- base
```

PR **#6** (`arena/aa2472ad-zexi` → `main`) is **OPEN** and all checks pass:

| Check | Result |
| --- | --- |
| Build and test | pass, 37s |
| CodeRabbit | pass |
| Codeac Code Quality | pass |
| Codeac analyze results | pass |

**[verified]** `gh pr checks 6`.

### 0.2 ⚠️ The sandbox loses `/tmp` and resets `.git` between sessions

Two things bit us and will bite again:

1. **The Go toolchain lives in `/tmp` and is gone.** There is no `go` on this
   box and no cached tarball. **[verified]** `find / -maxdepth 4 -name go -type f`
   returns nothing; `/tmp/gobuild` does not exist.
2. **The local clone was silently reset to the base commit** `68d9262` while the
   working tree kept all the edits, so everything looked like uncommitted work.
   Nothing was lost — the six commits were on the remote the whole time, and the
   working tree was byte-identical to `f9003f6`. **[verified]** by comparing each
   untracked file against `git show origin/arena/aa2472ad-zexi:<path>`.

**The recovery, if you ever see this again:**

```bash
git fetch origin arena/aa2472ad-zexi
git diff --stat origin/arena/aa2472ad-zexi     # confirm the tree matches
git reset origin/arena/aa2472ad-zexi           # mixed reset: moves the pointer,
                                               # keeps the working tree
git status --short                             # must be empty
```

Do **not** `git checkout .` or `git clean -fd` when you see a wall of modified
files after a sandbox reset — that destroys the work. Reset the pointer first,
then look.

### 0.3 First action in a new session: rebuild the toolchain

`codeload.github.com` is reachable (**[verified]** HTTP 200 for the go1.22.5
tarball). Google's hosts, `go.dev`, `raw.githubusercontent.com`, and GitHub
release assets are **not**. So the only route is building Go from source.

The bootstrap chain cannot be shortened — each stage needs the previous:

| Stage | Version | Bootstrap needs | Note |
| --- | --- | --- | --- |
| 1 | go1.4.3 | C compiler only | **must** use `CGO_ENABLED=0` — Go 1.4 cannot build `runtime/cgo` against Debian's default-PIE gcc (`-r and -pie may not be used together`) |
| 2 | go1.17.13 | go1.4 | |
| 3 | go1.20.14 | go1.17 | |
| 4 | go1.22.5 | go1.20.14 | this is the one you actually use |

```bash
mkdir -p /tmp/gobuild && cd /tmp/gobuild
for tag in go1.4.3 go1.17.13 go1.20.14 go1.22.5; do
  curl -sL "https://codeload.github.com/golang/go/tar.gz/refs/tags/$tag" -o "$tag.tar.gz"
  tar xzf "$tag.tar.gz"          # extracts to go-<tag>/, NOT go/
done
# stage 1 — note CGO_ENABLED=0
cd /tmp/gobuild/go-go1.4.3 && CGO_ENABLED=0 GOROOT_BOOTSTRAP=/nonexistent \
  GOROOT_FINAL=/tmp/gobuild/go1.4 ./make.bash
# stages 2-4
cd /tmp/gobuild/go-go1.17.13 && GOROOT_BOOTSTRAP=/tmp/gobuild/go1.4 \
  GOROOT_FINAL=/tmp/gobuild/go1.17 ./make.bash
cd /tmp/gobuild/go-go1.20.14 && GOROOT_BOOTSTRAP=/tmp/gobuild/go1.17 \
  GOROOT_FINAL=/tmp/gobuild/go1.20 ./make.bash
cd /tmp/gobuild/go-go1.22.5  && GOROOT_BOOTSTRAP=/tmp/gobuild/go1.20 \
  GOROOT_FINAL=/tmp/gobuild/go1.22.5 ./make.bash

cat > /tmp/env.sh <<'EOF'
export GOROOT=/tmp/gobuild/go1.22.5
export PATH=$GOROOT/bin:$PATH
export GOPATH=/tmp/gobuild/gopath
export GOCACHE=/tmp/gobuild/gocache
EOF
```

**Keep `GOPATH`/`GOCACHE` under `/tmp`.** Putting them under `/home/user` bloats
the persisted snapshot.

Then:

```bash
source /tmp/env.sh
cd /home/user/zexi
test -z "$(gofmt -l .)" && go vet ./... && go test ./...
go build -o /tmp/zexic ./cmd/zexic      # the driver for probing .zx programs
```

**If you would rather not spend the time:** every change can be gated on CI
instead. `gh pr checks 6` reports the result of the real `gofmt` + `vet` +
`go test` run. The cost is a ~40 s round trip per iteration and no ability to
run `.zx` probe programs locally — which matters a lot for steps 1 and 2 below,
since those need `/tmp/zexic run`.

---

## 1. Integer-literal type inference — **do this first**

### The problem

An unannotated integer literal is pinned to `i32` the moment it is checked, and
that pin does not move. Four shapes fail today:

```zx
fn next(i: i64) -> i64 { return i + 1; }
struct C { n: i64 }

fn main() -> i32 {
    let a = 0;   next(a);              // E0308 expected `i64`, found `i32`
    let k = 7;   C { n: k };           // E0308 mismatched types in field `n`
    let v: Vec<i64> = Vec::new();
    let i = 0;   i < v.len();          // E0308 `i32` and `usize`
    0
}
```

**[verified]** — all four reproduced with `/tmp/zexic check`. Annotating
(`let a: i64 = 0`) works, and a literal passed *directly* to the parameter
(`next(5)`) works. It is specifically the **variable** carrying a defaulted
literal that fails.

This is the single most likely thing to frustrate anyone writing real Zexi, so
it is worth more than any remaining feature.

### Why it is tractable

The expected type is **already threaded** to exactly the places that need it:

| Site | Location | Already passes `want`? |
| --- | --- | --- |
| Call arguments | `checkCallArgs`, `sema.go` | **yes** — `want = fn.Params[i]` → `c.checkExpr(a, want)` |
| Struct-literal fields | `sema.go:3864` | **yes** — `vt := c.checkExpr(fi.Val, f.Ty)` |
| Identifiers | `inferIdentExpr(x, expected)` | **yes** — signature takes `expected` |
| Binary operands | `sema.go:1797-1805` | only for *literals*, not variables |

**[verified]** all four by reading the source. So two of the three failing
shapes come for free from one hook in `inferIdentExpr`.

### The design

**Step 1 — mark defaulted literals, and stop sharing the singleton.**

`types.I32T` is a package-level singleton (`types.go:148`,
`I32T = &Type{Kind: I32}`) **[verified]**. Mutating it would corrupt every
`i32` in the program. So the default path in `infer` (around `sema.go:1512`,
the `return types.I32T` after the widening check) must allocate a **fresh**
instance and record it:

```go
// checker gains:
//   pending map[*types.Type]*pendingInt
type pendingInt struct{ text string }

// in the literal case, on the `expected == nil` branch only:
t := &types.Type{Kind: types.I32}      // fresh — never types.I32T
if c.pending == nil { c.pending = map[*types.Type]*pendingInt{} }
c.pending[t] = pendingInt{text: x.Text}
return t
```

Leave the existing "does not fit in i32 → widen to i64/u64" branch alone.

**Step 2 — adopt a concrete type on first demand.**

```go
// adoptInt retypes a binding that was defaulted from a bare integer literal,
// when this use demands a concrete integer type. It mutates in place, so every
// use of the binding agrees. Returns nil when it does not apply.
func (c *checker) adoptInt(t, want *types.Type) *types.Type {
    p, ok := c.pending[t]
    if !ok || want == nil || !want.Kind.IsInt() {
        return nil
    }
    if !intLitFits(p.text, want) {
        return nil      // let the normal E0080/E0308 path produce the message
    }
    t.Kind = want.Kind
    delete(c.pending, t)
    return t
}
```

Call it at the top of `inferIdentExpr`, on the `lookupScope` hit:

```go
if t := c.lookupScope(x.Name); t != nil {
    c.adoptInt(t, expected)
    return c.inferIdent(x.Name, x.Sp)
}
```

In-place mutation is safe here because step 1 guarantees the instance is private
to this binding. The pointer really is stored unmodified —
`bindPattern` → `c.declare(pt.Name, ty)` → `c.scope[len-1][name] = ty`
**[verified]** — so `let x = 0; let y = x;` shares the instance and retyping `x`
retypes `y` too. That matches Rust and is the right answer for the same value.

**Step 3 — extend the binary-operator adoption to variables.**

At `sema.go:1797-1805` the existing switch adopts for literals only. Add two
cases:

```go
if !types.Equal(lt, rt) {
    switch {
    case isIntLiteral(x.L) && rt.Kind.IsNumeric():
        lt = c.checkExpr(x.L, rt)
    case isIntLiteral(x.R) && lt.Kind.IsNumeric():
        rt = c.checkExpr(x.R, lt)
    case c.adoptInt(lt, rt) != nil:      // NEW — no reassignment needed:
    case c.adoptInt(rt, lt) != nil:      // NEW — adoptInt mutates in place
    }
}
```

The `types.Equal` check immediately after then succeeds.

**Step 4 — the range path needs its own fix.**

`for i in 0..n` does **not** go through `inferBinary`, so step 3 does not cover
it. The code at `sema.go:4154-4155` is:

```go
lt := c.checkExpr(rng.L, nil)      // `0` -> pending i32
c.checkExpr(rng.R, lt)             // `n` (i64) checked against want=i32 -> E0308
```

Note the direction: `rng.R` is checked against `lt`, but `rng.R` is the
*identifier* and `lt` is the pending literal — so the `inferIdentExpr` hook
cannot help, because `n` is not the pending binding. The pending `lt` has to
adopt instead:

```go
lt := c.checkExpr(rng.L, nil)
rt := c.checkExpr(rng.R, nil)
if c.adoptInt(lt, rt) == nil {
    c.checkExpr(rng.R, lt)   // keeps today's literal adoption and its error
}
```

`lt.Kind.IsInt()` on the next line still holds after adoption, so the
"range bounds must be integers" check is unaffected.

**[derived by reading the source, not by running it — there was no toolchain
available when this was written. Confirm with `for i in 0..some_i64 {}` before
considering step 1 done.]**

### Acceptance

A new `internal/driver` test (follow `wantOutput` in
`codegen_scoping_test.go`) covering all four shapes above, **plus** these must
keep failing exactly as they do today:

- `let x = 0; some_f64_fn(x)` → still `E0308`. The `want.Kind.IsInt()` guard is
  what keeps floats out; do not widen it.
- `let x = 300; let y: i8 = x;` → still `E0080`. `intLitFits` is what enforces
  it; do not skip that check.
- Two conflicting integer demands on one binding → first wins, second reports
  `E0308`. That matches Rust and is diagnosable; do not try to be cleverer.

### Risks

- **The singleton trap** is the one real hazard. Grep for any other path that
  returns `types.I32T` for an unannotated literal and make sure none of them
  feed `adoptInt`. `sema.go:2847`, `2949`, `2967`, `4158`, `4789` all mention
  `I32T` **[verified]** — check each before shipping.
- The mutation happens during sema, and `c.info.TypeOf[...]` for both the binding
  and the literal expression points at the same instance, so codegen (which runs
  strictly after sema) sees a consistent picture. Confirm with a probe that
  prints the value, not just the type.

---

## 2. `HashMap<K, V>`

The last missing M1 collection. Blocked on a design decision, not on plumbing.

**Blocker: there is no `Hash` trait.** **[verified]** `grep -rn "Hash\b"
internal/types/types.go` returns nothing, and `internal/ast/ast.go` has no
`TraitDecl` at all.

Two routes:

- **(a) Built-in hashing for a closed set of key types** — `i64`/`i32`/`usize`/
  `bool`/`char`/`str`/`String`. Mirrors exactly how `Vec<T>` was done and needs
  no trait system. **Recommended:** it unblocks `HashMap` without waiting on
  step 5, and the seam can be widened later.
- **(b) A real `Hash` trait.** Correct long-term, but that is step 5 and is much
  larger.

### Implementation, following the `Vec<T>` template

`Vec` is the working model and it is fresh in the tree — copy its structure:

| Concern | Vec file | What to mirror |
| --- | --- | --- |
| Type kind | `types/types.go` — `VecKind` after `StringKind` | add `MapKind`; `IsCopy()` false, `NeedsDrop()` true |
| Sema | `internal/sema/vec.go` (~192 lines) | ctor sigs, method sigs, `resolveVecType` → `resolveMapType`, `vecElem` → key/value extraction |
| Codegen | `internal/codegen/c/vec.go` (~419 lines) | `mapTypeName`, `collectMapTypes`, forward decls, runtime helpers, method dispatch |
| Dispatch sites | 6 places in `sema/sema.go` | `resolveType`, `inferCall`, `inferMethodCall`, `inferLen`, `inferIndex`, `inferFor` |
| Wiring | ~11 places in `codegen/c/codegen.go` | `gen` struct fields, `emitPrefix` ordering, `ctype`, `typeMangle`, `typeDefs`, `collectDropTypes`, `emitDropFunc`, `runtime()`, `zeroValue`, `lenValue`, `indexValue`, `methodCallValue`, `callValue` |

Suggested surface: `HashMap::new()`, `insert(k, v) -> Option<V>`, `get(k) ->
Option<&V>` (or by value for `Copy` V), `remove(k) -> Option<V>`,
`contains_key(k) -> bool`, `len()`, `is_empty()`, `clear()`.

### Gotchas already learned from `Vec` — do not repeat them

- **Never copy a handle that owns a buffer.** In `for x in v` the handle must be
  read in place; a copy aliases the buffer and frees it twice.
- **A temporary receiver must be registered in the *enclosing* drop frame**, via
  `g.registerDrop` *before* `openLoop`, so `emitDropsFrom(loopFrame)` on `break`
  skips it but scope exit still frees it.
- **Runtime helpers go after `dropFuncs`** — they need the `zx_drop_*`
  prototypes. `runtime()` runs in `emitPrefix()` before bodies;
  `charRuntime()`/`lateRuntime()` run *after* bodies. **This ordering bug bit
  twice already.** Anything a function body needs must be forward-declared or
  emitted up front.
- **Handle type defs must precede struct defs that hold one by value.** Emit
  them at the top of `typeDefs` via the forward-decl/def pair.
- **`clone` on a non-`Copy` element must be rejected at sema** (`E0277`), not
  silently shallow-copied. Same rule applies to `HashMap` values.
- **`pop`-style methods that return `Option<T>` must be lowered in codegen**, not
  in the C runtime: build the compound literal by reading the *real* `Some`/`None`
  tag indices out of `opt.En.Variants`. Do not hardcode tag 0/1.
- Verify the split-unit path too: build the same program with `--jobs=4` and
  confirm identical output.

---

## 3. `println(fmt, args)` formatting

Today `println` takes N space-separated values
(`sema.go:1932`, `if name == "print" || name == "println"`) **[verified]**. The
M1 goal is a real format string.

Suggested incremental path, each step independently shippable:

1. **Detect a literal first argument containing `{}`** and treat the rest as
   arguments. Keep the current space-separated behaviour when there are no `{}`
   placeholders, so nothing existing breaks.
2. Support `{}` only — no width, precision, or fill yet.
3. Type-check each `{}` against its argument; the argument must be printable
   (reuse the existing `E0277 cannot be printed` diagnostic).
4. Later: named fields (`{name}`) and `{:?}` debug formatting.

**Design note from the roadmap:** this is meant to use a monomorphized
formatting trait, not reflection. But a `{}`-only first version needs no trait
system at all — dispatch on the argument's static type in codegen, exactly as
`println` already does per value. Ship that; add the trait when step 5 lands.

Acceptance: a `tests/formatting.zx` golden-output program. Note that
`tests/formatting.zx` already exists **[verified]** — extend it rather than
adding a competing file.

---

## 4. Real per-module isolation

**Current state:** `internal/driver/modules.go` merges every module into one
flat namespace — `merged *ast.File` at `modules.go:110`, with the comment at
line 85 stating plainly that "Items are still flattened into the program for
sema" **[verified]**. Qualified paths, `as` aliases, and `pub` checks work;
unqualified cross-module names remain visible and ignore `pub`.

**Symptom to fix:** two modules can each define `fn helper()` and they collide;
and a private item is reachable unqualified from another module.

**Approach:** give sema a per-module scope stack instead of one global
`c.info.Fns` map. That is a real refactor of `collect`/`registerPrelude` and
touches every name-resolution path, so it is the largest item on this list
after traits. Do it **after** steps 1-3, and behind its own PR.

Smaller pieces that can land first and are useful on their own:

- re-exports (`pub use`)
- glob imports (`use foo::*`)
- a prelude module

---

## 5. Traits — the big one

**Blocked at the parser and AST, not at sema.** **[verified]**
`grep -n TraitDecl internal/ast/ast.go` returns nothing. There is no `trait`
item in `parseItem`'s switch either.

Order of work:

1. **AST + parser:** add `TraitDecl { Vis, Attrs, Name, Generics, Methods }`.
   Note `Attrs` now exists on `FnDecl` (`ast.go:39-41`), `StructDecl` (`:81`),
   and `EnumDecl` (`:103`) **[verified]** — follow the same shape and thread
   attributes through `parseItem` the way `parseFn(vis, attrs, start)` already
   does.
2. **`impl Trait for Type`** blocks, collected alongside the existing `impl`
   blocks.
3. **Trait bounds on generic parameters**, enforced at monomorphization. There
   is already a `unify(pat, got, bindings)` at `sema.go:2283` and
   `unifyNominal` at `:2353` **[verified]** — extend rather than replace.
4. Then, in order of usefulness: `Clone`/`Copy`/`Drop` → `Iterator` →
   operator traits → `From`/`Into` → `dyn Trait` (M1 late).

**Payoff worth noting:** `Clone` is what unblocks `Vec<String>.clone()` and
`for x in vec_of_strings`, both of which are rejected today (`E0277` and
`E0507`) purely because there is no element-wise clone. That is a visible
limitation documented in the cheatsheet, so it is a good first target once the
trait plumbing exists.

---

## 6. Smaller items

- **`extern "C"` structs.** Functions already work in both directions
  (**[verified]** — the documented `glue.c`/`main.zx` round trip builds with
  `--csrc` and exits 0). Structs need a parser case in `parseExternFn` and a
  layout mapping onto the existing `#[repr(C)]` path, which is already the
  default layout.
- **Real NLL ownership checker.** Today `internal/sema/own.go` is a lexical,
  **name-keyed** move checker, not place/path-based, and joins are
  union-of-moved. Conditional moves and drop flags need place-based tracking.
- **Incremental + parallel builds**; LSP / `zexi fmt`.

---

## 7. Definition of done for every item

Run these, in this order, and do not report done until all are clean:

```bash
source /tmp/env.sh && cd /home/user/zexi
test -z "$(gofmt -l .)"   # must print nothing
go vet ./...        # must print nothing
go test ./...       # all packages ok; internal/driver takes ~8s
```

Plus, for anything touching codegen, run the `.zx` probes with the freshly
built `/tmp/zexic` — `go test` alone does not cover the split-unit path. Check
that too:

```bash
/tmp/zexic build prog.zx --jobs=4 -o prog && ./prog
```

Then push and confirm CI:

```bash
git push origin arena/aa2472ad-zexi
gh pr checks 6
```

**Never work on a branch other than `arena/aa2472ad-zexi`.**

---

## 8. Out of scope (roadmap §5.5 — do not add without strong evidence)

Macros, exceptions/unwinding, GC or a VM, operator overloading beyond traits,
implicit-conversion soup, inheritance, reflection/RTTI.

---

## 9. Known-good baselines — do not re-test

These were verified and are covered by tests; re-checking them wastes time.

**Fixed and regression-tested:**

- Nested `for` loops and `match` temporaries no longer shadow user bindings
  (`bindLoopVar` + `g.tmpName()`; `TestNestedLoopVariablesKeepTheirScope`,
  `TestMatchTemporariesDoNotShadowLocals`).
- Panics print `zexi panic: ...` and exit **101**, with stdout flushed first.
  `/` and `%` are checked in every profile; `+ - *` only at `--opt-level=0`
  (`TestRuntimePanics`, `TestOverflowChecksFollowsOptLevel`).
- `struct String` / `struct i32` / `enum String` etc. are `E0428`, not a raw C
  error (`TestBuiltinTypeNamesAreReserved`).
- `#[export]` functions that nothing in Zexi calls are still emitted
  (`TestExportedAttributes`).
- String appends grow geometrically (`TestStringBuilderGrowth`).
- `String` ordering by byte value, all six operators, any `str`/`String` mix.

**Verified correct:**

- No leaks: 100k-iteration loops over `String`, `enum { Text(String) }`,
  `[String; 3]`, reassignment-in-loop, `Vec<String>` (400k pushes), and
  temporary `Vec` in `for` including `break` all peak around 1.5 MB RSS.
- Div/mod truncation matches Rust on all eight sign combinations; casts
  (`1.5 as i32` = 1, `300 as i8` = 44, `-1 as u8` = 255, `255 as char` = 'ÿ').
- Float printing uses `%g`: `1.0`→`1`, `1e6`→`1e+06`, `-0.0`→`-0`.
- `defer` LIFO + block-scoped; `break`/`continue` from `match` in a loop;
  value-yielding `loop`; `if let`/`while let`; match guards; `?` propagation.

**Pre-existing failures — not caused by this branch, do not chase:**
`tests/easy_lib.zx` and `tests/modules_lib.zx` are library files with no
`main`; `tests/http_{client,server}.zx` and `examples/http_{client,server}.zx`
need a network / the `lib/http.zx` project layout. All six fail identically at
the base commit `68d9262`.

**Language gaps that are known and documented, not bugs:**

- Array slicing is unimplemented (`xs[1:3]` → `E0608`).
- `String` has no `upper`/`lower`/`index_of`/`replace`/`slice` — casing lives in
  the `text` stdlib module.
- `mut self` on a by-value method does not parse.
- `Vec<T>.clone()` and `for x in` a `Vec` need a `Copy` element (`E0277`/`E0507`)
  until step 5 lands `Clone`.
