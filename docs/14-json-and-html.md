# 14. Building websites: `json` and `html`

Two dependency-free modules do most of the work of a server-rendered site: `json`
for machine-readable APIs and `html` for safe markup. Both are small, both are
embedded in the compiler, and both return owned `String`s you can hand straight
to a `Response`.

```zx
use http;
use html;
use json;
```

## 14.1 Why a "fat" value instead of an enum

Most JSON libraries model a document as a recursive enum
(`Null | Bool | Num | Str | Array | Object`). Zexi can *declare* that today —
recursive types work when the recursion goes through `Vec` or `HashMap` — but a
pattern match cannot yet move an owned payload out of a **borrowed** enum, and
every accessor on `&Value` would need to. So `json::Value` is a struct with a
`kind` tag and one field per payload:

| Field | Meaning |
| --- | --- |
| `kind` | `json::NULL`, `BOOL`, `INT`, `NUM`, `STR`, `ARR`, `OBJ` |
| `b`, `i`, `n`, `s` | the boolean, integer, double, and string payloads |
| `items` | array elements (`Vec<Value>`) |
| `fields` | object members (`Vec<Field>`, insertion-ordered) |

The unused fields simply stay empty. The payoff is that every accessor works
through a reference (`&Value`), objects keep the order the document used, and
the same document always prints the same way — which makes tests and cached
bodies stable. The cost is a wider value: roughly a hundred bytes per node
rather than a tagged union of the payload.

## 14.2 Parsing and reading

<!-- check-only -->
```zx
use json;

fn main() -> i32 {
    let raw = "{\"name\": \"zexi\", \"stars\": 7, \"tags\": [\"lang\", \"fast\"]}";
    let doc = match json::parse(raw) {
        Ok(v) => v,
        Err(e) => {
            println("bad json:", e);
            return 1;
        }
    };

    println(doc.get_str("name"));        // zexi
    println(doc.get_int("stars"));       // 7
    println(doc.get_int("missing", -1)); // -1

    let tags = doc.get("tags");
    println(tags.len(), tags.item(0).text());
    0
}
```

`parse` returns `Result<Value, String>`; the error text names the byte offset
where the document stopped making sense (`json: expected `,` or `]` at byte 5`).
`json::valid(raw)` is the same check without building a value.

| Call | Result |
| --- | --- |
| `json::parse(raw)` | `Result<Value, String>` |
| `json::valid(raw)` | bool |
| `v.is_null/is_bool/is_int/is_num/is_number/is_str/is_array/is_object()` | kind test |
| `v.as_i64()`, `v.as_f64()`, `v.as_bool()`, `v.text()` | payload, converted where sensible |
| `v.len()`, `v.is_empty()`, `v.item(i)` | array/object size and one array element |
| `v.get(key)`, `v.get_str/get_int/get_f64/get_bool(key, fallback)` | object member, by value |
| `v.has(key)`, `v.index_of(key)`, `v.key_at(i)`, `v.value_at(i)` | object inspection and iteration |
| `v.clone()` | deep copy |

**Numbers keep their identity.** A JSON integer that fits in `i64` stays an
integer (`kind` is `INT`) and prints exactly, so `9007199254740993` does not turn
into `9007199254740992`. Anything with a fraction or an exponent becomes a
double (`kind` is `NUM`) and prints in the shortest form that reads back as the
same value (`0.3333333333333333`, not `0.333333`). Integers too large for `i64`
fall back to a double.

## 14.3 Building a response

<!-- check-only -->
```zx
use http;
use json;

fn stats(req: &Request) -> Response {
    let mut doc = json::object();
    doc.set_str("lang", "zexi");
    doc.set_int("requests", requests_served());

    let mut tags = json::array();
    tags.push_str("http");
    tags.push_str("threads");
    doc.set("tags", tags);

    let body = json::stringify(&doc);
    return Response::json(200, body.as_str());
}
```

`set` replaces an existing member in place (keeping its position) and appends a
new one otherwise; `push` turns the value into an array if it was something
else, so builders do not need kind checks. `json::quote(s)` renders a single
JSON string literal — handy for embedding a value in a page or a log line.

| Call | Purpose |
| --- | --- |
| `json::null()`, `json::of_bool`, `of_int`, `of_num`, `of_str`, `of_string` | scalars (`of_string` takes ownership of a `String`) |
| `json::array()`, `json::object()` | empty containers |
| `v.push(x)`, `v.push_str(s)`, `v.push_int(n)` | append to an array |
| `v.set(key, x)`, `set_str`, `set_int`, `set_f64`, `set_bool`, `set_null` | insert or replace a member |
| `v.remove(key) -> Value` | delete a member, returning it |
| `json::stringify(&v)` | compact text |
| `json::pretty(&v, indent = 2)` | indented text |
| `json::quote(s)` | one escaped JSON string literal |

## 14.4 Escaping is not optional

`html` exists because every value that came from a request — query parameter,
form field, cookie, header, database row — is markup waiting to happen. `escape`
converts the five dangerous characters; `template` does it for you for every
`{{name}}` it substitutes.

<!-- check-only -->
```zx
use html;

fn main() -> i32 {
    println(html::escape("<b>Tom & Jerry's</b>"));
    // &lt;b&gt;Tom &amp; Jerry&#39;s&lt;/b&gt;

    let mut vars: HashMap<String, String> = HashMap::new();
    vars.insert(String::from("name"), String::from("<script>alert(1)</script>"));
    println(html::template("<p>Hello, {{name}}!</p>", &vars));
    // <p>Hello, &lt;script&gt;alert(1)&lt;/script&gt;!</p>
    0
}
```

| Call | Purpose |
| --- | --- |
| `html::escape(s)` | text-safe (`& < > " '`) |
| `html::attr(s)` | the same, for a quoted attribute value |
| `html::url_query(s)` | percent-encode a query/path value |
| `html::page(title, body)` | a complete HTML5 document (title escaped, body trusted) |
| `html::page_with(title, head, body)` | the same with extra `<head>` markup |
| `html::template(tpl, vars)` | `{{name}}` escaped, `{{name\|raw}}` verbatim, `{{{{` for a literal `{{` |
| `html::template_pairs(tpl, pairs)` | the same with alternating key/value `Vec<String>` |
| `html::tag(name, text)`, `heading(level, text)`, `link(href, text)` | one escaped element |
| `html::list(items)`, `ordered_list(items)` | `<ul>`/`<ol>` of escaped items |
| `html::table(headers, rows)` | a table of escaped cells (`Vec<String>`, `Vec<Vec<String>>`) |
| `html::json_script(id, json)` | JSON in a `<script>` tag that cannot close it early |

`{{name|raw}}` is the opt-out, for markup you built yourself (for example a
`html::list` fragment passed into a layout). Everything else is escaped, and a
name that is missing from the map renders as nothing rather than as `{{name}}`.

## 14.5 A whole site

[`examples/site.zx`](../examples/site.zx) puts the pieces together: a shared
layout through `html::template`, three pages, an escaped `?name=` page, and a
`/api/stats` endpoint that reports the worker pool's own counters as JSON. It is
served with `listen_workers`, so several visitors are answered at once.

```sh
zexic run examples/site.zx
curl http://127.0.0.1:8090/
curl 'http://127.0.0.1:8090/hello?name=world'
curl http://127.0.0.1:8090/api/stats
```

Tests for both modules live in [`tests/json_lib.zx`](../tests/json_lib.zx) and
[`tests/html_lib.zx`](../tests/html_lib.zx); they assert exact output, including
escape sequences, unicode, and pretty-printed layouts.
