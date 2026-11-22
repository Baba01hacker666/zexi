# 20. Projects, files, and modules

A Zexi program is one program made of many files. Files are the unit you edit,
`mod` and `use`/`import` decide what each file may see, and the driver merges the
reachable tree into one program before checking it. This chapter is the
practical guide to that tree; chapter [19](19-modules.md) covers installing a
C-backed library, and chapter [18](18-one-translation-unit.md) explains why the
result is still a single C translation unit.

## 20.1 Convention-only projects

Pass a file or a directory to `zexic`:

```sh
zexic check app.zx          # one entry file
zexic run .                 # a project directory
zexic build . -o app        # standalone binary
```

For a directory, `zexic` looks for `main.zx`, then `src/main.zx`. That directory
is the **project root**, which is one of the places named imports are searched.
There is no manifest, no lockfile, and no build file:

```text
myapp/
├── main.zx                  # entry point
├── lib/
│   └── sqlite.zx            # installed package (chapter 19)
├── util/
│   ├── mod.zx               # optional facade: pub use of the files below
│   ├── strings.zx
│   └── paths.zx
├── report.zx                # reachable through `mod report;`
└── src/...                  # alternative: put main.zx under src/
```

Everything under the tree is source. The loader only reads files that are
actually reachable from the entry point; a file sitting in the project is not
compiled until something names it.

## 20.2 Four ways to split code

| Form | What it does | Reach it with |
| --- | --- | --- |
| `mod report;` | loads `report.zx` or `report/mod.zx` as a scoped module | `report::item` |
| `mod util { ... }` | an inline module in the current file | `util::item` |
| `use util;` | loads `util.zx`, or every `.zx` file in the `util/` folder, and imports its public names | unqualified `item`, or `util::item` |
| `use "helpers.zx";` | loads one neighbouring file by path | `helpers::item` |

A nested path works the same way: `use net::tcp;` looks for `net/tcp.zx`, while
`import http.server;` and `use http::server;` select the high-level HTTP facade.
File imports are relative to the importing file; named imports search, in order:

1. the importing file's directory,
2. its `lib/` folder,
3. the project root,
4. the root's `lib/` folder,
5. the standard library embedded in the compiler.

Local files win over the standard library, so a project can shadow a shipped
module by placing its own file in `lib/`.

## 20.3 Visibility: private by default

A top-level declaration is private to its file unless it is marked `pub`:

<!-- fragment -->
```zx
// util/strings.zx
pub fn join(xs: &Vec<String>, sep: str) -> String { /* ... */ }
fn scratch() -> i64 { 0 }          // not visible outside this file
```

<!-- fragment -->
```zx
// main.zx
mod util;

fn main() -> i32 {
    let s = util::strings::join(xs, ", ");   // only `pub` items resolve
    0
}
```

Qualified access checks the export table. A missing item is `E0412` and a
private one is `E0603`; a `mod` name used as a value or type is `E0433`.

`use pkg;` also brings the public names into unqualified scope, preserving the
M0 convenience. If two imports export the same name, the unqualified use is
`E0659` (ambiguous) while the qualified forms still work:

<!-- fragment -->
```zx
use alpha;
use beta;

fn main() -> i32 {
    shared();          // error: exported by both
    alpha::shared();   // fine
    0
}
```

Use an alias when the qualifier should be short, or when two packages would
otherwise fight over a name:

<!-- fragment -->
```zx
use alpha as a;
use beta as b;
a::shared();
```

`pub use` re-exports an imported public name through a facade, which is how a
directory package can present a small surface while keeping its helpers
private.

## 20.4 A folder package

`use util;` (or `mod util;`) with a `util/` directory loads the whole folder as
one package. All files share the package's export table, `main.zx` and files
whose names start with `_` are skipped, and the package exports the union of its
members' `pub` items.

<!-- fragment -->
```zx
// util/mod.zx — a facade over two implementation files
pub use strings;
pub use paths;
```

<!-- fragment -->
```zx
// util/strings.zx
pub fn join(xs: &Vec<String>, sep: str) -> String { /* ... */ }
```

<!-- fragment -->
```zx
// main.zx
use util;
let s = util::join(xs, ",");   // re-exported through lib/util/mod.zx
```

A package directory and `util.zx` are not both needed. Prefer one shape:
`util.zx` for a small module, `util/` when it has more than one file.

## 20.5 Cycles and duplicate files

Every file is loaded once by its canonical path (symlinks are resolved). Import
cycles are harmless: the module being loaded is registered before its imports are
followed, so `a` may use `b` while `b` uses `a`. There is no separate compilation
and no manifest to keep in sync, so a cycle cannot leave a stale half-built
object behind.

A file named directly on the command line is part of the entry program, exactly
as before modules existed. Files reached through `mod`/`use`/`import` get module
isolation: private names keep their source spelling but are not visible outside
their file.

## 20.6 Helpful diagnostics

Misspelled names now come with a suggestion in the modules, the checker, and the
package registry:

```text
error[E0412]: module `math` has no item named `doulbe` (did you mean `double`?)
error[E0609]: no field `xx` on type `Point` (did you mean `x`?)
error[E0599]: no method `nrom` on type `Point` (did you mean `norm`?)
error[E0599]: no variant `Reed` on enum `Color` (did you mean `Red`?)
error[E0412]: cannot find type `Pont` in this scope (did you mean `Point`?)
```

Import errors say what was searched and what was probably meant:

```text
error[E9022]: cannot find a library named `mat` (looked for mat.zx next to the
importing file, in its lib/ folder, at the project root, and in the standard
library) (did you mean `math`?)
   = help: import a file with `use "path.zx";`, or drop `mat.zx` into the
           project's lib/ folder
```

A privacy error explains the fix (`mark the declaration pub, or re-export it
with pub use`), and an ambiguous unqualified name suggests importing one package
under an alias. Matching ignores case, so `Math` points at `math`.

## 20.7 Checking a tree

`zexic check .` parses and type-checks the whole reachable tree without emitting
a binary. Because every module is loaded by the same loader, a mistake in a
private helper in a nested package is reported at its own file and line, not at
the import that reached it. `zexic run .` builds the same tree; the loader
detects and reports cycles rather than recursing forever.

## 20.8 What stays one program

This is still a whole-program compiler: `zexic` loads the reachable files and
checks them as one program, then emits one C translation unit (chapter
[18](18-one-translation-unit.md)). That is what lets a package carry its own
inline C, `#[link]`, and `#[include]` without a manifest, and it is why importing
a module costs nothing unless something calls into it. The trade-off is that
there is no separate object-file cache yet; incremental builds are a roadmap
item ([chapter 5](05-roadmap.md)).
