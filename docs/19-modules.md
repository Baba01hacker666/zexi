# 19. Modules: adding a C library with one command

Zexi's extension story (chapter [17](17-extending.md)) is that a capability is a
**library file**, not a compiler change. This chapter is the packaging side of
that promise: `zexic add` installs a ready-made binding for a well-known C
library into your project, and the catalog — with the source of every package —
ships inside the compiler binary.

```sh
zexic add               # list the catalog, with live dependency status
zexic add sqlite        # write lib/sqlite.zx into this project
```

<!-- fragment -->
```zx
use sqlite;

fn main() -> i32 {
    let mut db = sqlite::memory()?;
    db.exec("create table notes(body text)")?;
    db.exec("insert into notes values('hello')")?;
    println(db.query("select body from notes")?);
    0
}
```

## 19.1 Why a package is one file

`zexic add sqlite` copies **one `.zx` file** into `lib/`. That file holds the
`extern "C"` declarations, the `c """..."""` glue, and a Zexi-shaped API on top.
There is nothing else:

| There is no | Because |
| --- | --- |
| manifest / lockfile | the loader already searches `lib/` for named imports (`use sqlite;`) |
| registry server or download step | the catalog and its sources are embedded in the compiler (`internal/registry`) |
| build flag or driver change | the module carries its own `#[link]`, `#[include]`, and C |
| version resolver | the installed file *is* the version — commit it and it is pinned |

The last row is the important one for reproducibility: installing a package
writes a normal source file into your tree, so `git diff` shows exactly what was
added, a review can read it, and a local edit survives (which is why `add`
refuses to overwrite without `--force`).

## 19.2 The catalog

| package | category | what it gives you | links |
| --- | --- | --- | --- |
| `sqlite` | storage | embedded SQL database: `open`, `exec`, `query`, prepared statements | `-lsqlite3` |
| `zlib` | data | DEFLATE `compress`/`decompress`, `bound`, `crc32` | `-lz` |
| `crypto` | security | SHA-256/512/1, MD5, HMAC-SHA256, constant-time compare | `-lcrypto` |
| `curl` | network | HTTP(S) client: TLS, redirects, timeouts, byte ceilings | `-lcurl` |
| `microhttpd` | network | native HTTP server daemon | `-lmicrohttpd` |

`zexic add` with no argument prints this list **with the state of your system**,
by compiling and linking a probe program per package:

```
package      category   what it gives you                                    system
sqlite       storage    embedded SQL database (libsqlite3)                   ready
curl         network    HTTP(S) client with TLS, redirects, and timeouts (libcurl)  needs curl/curl.h, libcurl
```

The status is not a guess from file paths: `cc` is asked to include the headers
and link the library, so a header in a directory `cc` does not search, or a
`libfoo.so.1` with no development symlink, is reported as missing — the same way
the real build would fail.

Installing is honest about the system too:

```sh
$ zexic add sqlite
installed lib/sqlite.zx — embedded SQL database (libsqlite3)
  use sqlite;
  system check: missing sqlite3.h
  install it with: apt install libsqlite3-dev  |  dnf install sqlite-devel  |  brew install sqlite
```

`add` still succeeds: the module is valid Zexi either way, and the warning tells
you what the build will need.

## 19.3 What `add` did to your project

`zexic add` writes into `<project>/lib/`, which is one of the places the loader
searches for named imports, in this order (`internal/driver/modules.go`):

1. the importing file's own directory,
2. its `lib/` folder,
3. the project root,
4. the root's `lib/` folder,
5. the standard library embedded in the compiler.

So the module works from anywhere in a project, and a `lib/sqlite.zx` you edit is
what gets compiled — the standard library is only a fallback.

```sh
zexic add sqlite --dir=vendor     # install somewhere else
zexic add sqlite --force          # replace a locally edited copy
zexic add sqlite --root=../app    # install into another project's lib/
```

## 19.4 The packages

### sqlite — an embedded database

Buffer-free and bounded: a `Db` owns a handle, `exec` returns the changed-row
count, `query` renders rows as `cell|cell` lines, and `prepare` gives a `Stmt`
you step yourself for large result sets.

<!-- fragment -->
```zx
use sqlite;

fn main() -> i32 {
    let mut db = match sqlite::open("app.db") {
        Ok(d) => d,
        Err(e) => { eprintln(e); return 1; }
    };
    match db.exec("create table if not exists notes(id integer primary key, body text)") {
        Ok(_) => {},
        Err(e) => { eprintln(e); return 1; }
    };
    let changes = match db.exec("insert into notes(body) values('hello')") {
        Ok(n) => n,
        Err(e) => { eprintln(e); return 1; }
    };
    println(f"inserted {changes}, last id {db.last_id()}");
    match db.query("select id, body from notes") {
        Ok(rows) => println(rows),        // 1|hello
        Err(e) => eprintln(e),
    };
    db.close();                            // closes deterministically
    0
}
```

Prepared statements bind by position and read by type:

<!-- fragment -->
```zx
use sqlite;

fn main() -> i32 {
    let mut db = sqlite::memory()?;
    db.run("create table t(name text, n integer)")?;
    let mut st = db.prepare("insert into t values(?1, ?2)")?;
    st.bind_text(1, "alpha")?;
    st.bind_int(2, 7)?;
    st.step()?;          // 0 = done
    st.finalize();
    0
}
```

`column_text` reads up to 4095 bytes and returns `Err` beyond that (ask
`column_int` or `substr()` for other shapes), and `column_is_null` distinguishes
SQL `NULL` from the empty string.

### crypto — digests and HMAC

<!-- fragment -->
```zx
use crypto;

fn main() -> i32 {
    println(crypto::sha256_hex("abc".as_bytes()));
    // ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad
    println(crypto::hmac_sha256_hex("key".as_bytes(), "payload".as_bytes()));
    if crypto::equal(a_digest, b_digest) {   // constant-time, for webhook checks
        println("same");
    }
    0
}
```

Also `sha512_hex`, `sha1_hex`, `md5_hex`, and the raw `*_bytes` forms for binary
protocols.

### zlib — compression

Buffer in, buffer out: the caller sets the ceiling and nothing allocates across
the boundary.

<!-- fragment -->
```zx
use zlib;

fn main() -> i32 {
    let text = "compress me";
    let mut packed: [u8; 128] = [0; 128];
    let n = zlib::compress_at(text.as_bytes(), &mut packed)?;
    let mut plain: [u8; 128] = [0; 128];
    let m = zlib::decompress(&packed[0..n], &mut plain)?;
    println(f"n={n} m={m} crc={zlib::crc32(text.as_bytes())}");
    0
}
```

`zlib::bound(n)` is the worst-case size to allocate; `decompressed_size(&src)`
reports the inflated size without a destination buffer.

### curl — an HTTP(S) client

Use `curl` when you need TLS or a real server's edge cases; use the standard
library's `net`/`http` when you want no external dependency at all.

<!-- fragment -->
```zx
use curl;

fn main() -> i32 {
    let r = match curl::get("https://example.com/") {
        Ok(r) => r,
        Err(e) => { eprintln(e); return 1; }
    };
    // A non-2xx status is still Ok: the transfer succeeded.
    if r.status != 200 { eprintln(f"status {r.status}"); }
    println(f"{len(r.body)} bytes of {r.content_type}");
    match curl::post_json("https://api.example.com/items", "{\"n\":1}") {
        Ok(r) => println(r.status),
        Err(e) => eprintln(e),
    }
    0
}
```

`get_limit(url, max_bytes, timeout_ms)` is the bounded form: the ceiling is
enforced inside libcurl's write callback, so an oversized response is a clean
`Err` instead of a memory surprise. Also `post`, `head`, `delete`, and a general
`fetch(method, url, body, content_type, max_bytes, timeout_ms)`.

### microhttpd — a native server

The daemon runs libmicrohttpd's polling thread and calls back into a function you
export with `#[no_mangle]` (chapter [17 §17.3](17-extending.md)). The signature is
fixed because both sides are in one translation unit, so a mismatch is a compile
error:

<!-- fragment -->
```zx
use microhttpd;

#[no_mangle]
fn zxmhd_dispatch(method: str, path: str, body: str,
                  out: &mut [u8; 65536], cap: i32) -> i32 {
    let reply = if path == "/health" { "ok" } else { "not found" };
    let b = reply.as_bytes();
    let mut i = 0;
    while i < len(b) && i + 1 < cap {
        out[i] = b[i];
        i += 1;
    }
    out[i] = 0;                       // the body is NUL terminated
    return if path == "/health" { 200 } else { 404 };
}

fn main() -> i32 {
    let d = microhttpd::start(8080, 1048576);   // port, max body bytes
    if d == 0 { eprintln(microhttpd::last_error()); return 1; }
    println("listening on 8080");
    microhttpd::wait(d);                         // blocks until stop()
    0
}
```

Because the reply is NUL terminated, responses are text. For binary bodies, the
standard library's `http` server is the better tool.

## 19.5 Writing your own package

A package is a `.zx` file in `lib/`. The rules are the ones chapter 17 already
sets for inline C, plus four conventions worth copying from the shipped modules:

1. **Prefix every symbol.** All C entry points are `zxsq_`, `zxc_`, `zxz_`,
   `zxn_`, `zxmhd_`, and every Zexi-side public item is namespaced by the module
   (`sqlite::open`). Blocks are concatenated into one translation unit, so an
   unprefixed `static` or macro collides with another package (chapter
   [18 §18.4](18-one-translation-unit.md)).
2. **Include what you use, in the block.** A block is compiled after the
   program's own code, so nothing provides its headers.
3. **Spell the generated type names.** A `&[u8]` parameter *is*
   `zx_slice_u8` (`{ T *ptr; size_t len; }`) and `&mut [u8; N]` is
   `struct zx_arrN_u8 *` (bytes are `buf->e`). Using an anonymous struct of the
   same shape is a conflicting-type error — which is the point of the shared unit.
4. **Wrap out-parameters in C.** Zexi cannot spell `sqlite3 **` or a
   `uLongf *`, so `open`-style calls go through a three-line C wrapper that
   returns the handle as an `i64`. That keeps the ABI explicit and the Zexi side
   free of pointer arithmetic.

The public API should return `Result` for anything that can fail, take `&[u8]`
rather than `String` for binary input, and never allocate across the boundary
without a documented ceiling.

```sh
# after writing lib/mylib.zx
zexic check .          # the module is type-checked with the program
zexic run .            # and built with its own #[link]/#[include]
```

## 19.6 Limits

* `add` installs only from the embedded catalog. There is no remote registry and
  no `add --git`; a package outside the catalog is a file you copy into `lib/`
  yourself (that is all `add` does).
* A package is text: it is as portable as the C library it binds, and it will
  not build on a platform without that library's development headers.
* The catalog is small on purpose. `sys`, `io`, `fs`, `net`, `http`, `thread`,
  `json`, `regex`, `text`, `codec`, `fmt`, `html`, `math`, and `random` are in
  the standard library and need no install; the catalog holds libraries the
  standard library deliberately does not vendor.
* `microhttpd` is the one package this repository cannot exercise end to end in
  its own CI (the daemon is not installed in the build image); its Zexi side is
  type-checked on every `go test`, and its C follows the documented
  `MHD_start_daemon`/`MHD_queue_response` protocol. `sqlite`, `zlib`, `crypto`,
  and `curl` are run against the real system libraries.
