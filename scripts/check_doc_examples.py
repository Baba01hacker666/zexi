#!/usr/bin/env python3
"""Compile every ```zx / ```zexi block of the markdown docs with zexic.

    python3 scripts/check_doc_examples.py [docs...] [--run] [--exe path/to/zexic] [-v]

A block compiles as written or, failing that, wrapped: single-line `use` /
`import` / `mod ...;` declarations hoisted to the top, the rest of the block
inside `fn main() { }`, or an empty `fn main() {}` appended after it — so a
document can show a statement snippet or a set of declarations without the
scaffolding. A block holding a live line (code before a `// error` note) must
fail to compile in every form. An HTML comment on the line before a fence marks
the exceptions: `<!-- fragment -->` skips a block that is not meant to compile
on its own (an API sketch, a partial listing), and `<!-- check-only -->`
compiles a program without running it.

With --run, each complete program is also run and its output compared with a
bare ``` block that directly follows it, as the docs show their examples'
output. The compiler is --exe, else a `zexic` is built once with `go build`.
File imports (`use "helpers.zx";`) are satisfied with empty stub files so a
block can show the shape of an import.

The report is in document order: a FAIL line per failing block with the
compiler's diagnostic or the differing output, and a summary line last; -v adds
a line for every block that passed. Examples are written under --out-dir
(default build/doc_examples).

The point is simple: docs that show code should not be able to drift away
from code that compiles.
"""
import argparse
import os
import re
import shutil
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DOCS = ["README.md", "workings.md"] + sorted(
    os.path.join("docs", f) for f in os.listdir(os.path.join(ROOT, "docs"))
    if f.endswith(".md")
)

IMPORT_RE = re.compile(r'^\s*(?:use|import|mod)\s+"([^"]+)"\s*;')


def read_blocks(path):
    """The ```zx/```zexi blocks: (first line number, lines, expected output, marker)."""
    with open(path, encoding="utf-8") as f:
        lines = f.read().split("\n")
    blocks = []
    i = 0
    while i < len(lines):
        m = re.match(r"^```(\w*)", lines[i])
        if not (m and m.group(1) in ("zx", "zexi")):
            i += 1
            continue
        marker = ""
        if i > 0:
            mm = re.match(r"^<!--\s*(fragment|check-only)\s*-->", lines[i - 1])
            if mm:
                marker = mm.group(1)
        start = i + 1
        body = []
        i += 1
        while i < len(lines) and not lines[i].startswith("```"):
            body.append(lines[i])
            i += 1
        i += 1
        # A bare ``` block right after the program is its expected output.
        expected = None
        j = i
        while j < len(lines) and lines[j].strip() == "":
            j += 1
        if j < len(lines) and lines[j].strip() == "```":
            expected = []
            j += 1
            while j < len(lines) and not lines[j].startswith("```"):
                expected.append(lines[j])
                j += 1
        blocks.append((start, body, expected, marker))
    return blocks


def has_expected_error(body):
    """A live line (code before the note) that ends in a `// error` note."""
    for line in body:
        code = line.split("//")[0]
        if code.strip() and re.search(r"//\s*error\b", line):
            return True
    return False


def strip_noise(line):
    """Blank out comments and string-literal contents, for brace counting."""
    out = []
    i, n = 0, len(line)
    while i < n:
        c = line[i]
        if c == '"':
            # Raw strings like r#"..."#: skip to the matching close.
            hashes = 0
            j = i - 1
            while j >= 0 and line[j] == "#":
                hashes += 1
                j -= 1
            if j >= 0 and line[j] == "r":
                close = '"' + "#" * hashes
                k = line.find(close, i)
                i = (k + len(close)) if k >= 0 else n
                out.append(" ")
                continue
            i += 1
            while i < n and line[i] != '"':
                if line[i] == "\\":
                    i += 1
                i += 1
            i += 1
            out.append(" ")
            continue
        if c == "/" and i + 1 < n and line[i + 1] == "/":
            break
        if c == "#" and not (i + 1 < n and line[i + 1] in '"<'):
            break  # a `#` line comment, not r#" or an attribute
        out.append(c)
        i += 1
    return "".join(out)


ITEM_RE = re.compile(
    r"^\s*(?:use|import|mod|extern|fn|struct|enum|impl|const|type)\b")
ATTR_RE = re.compile(r"^\s*#\[[^\]]*\]\s*$")


def split_items_stmts(body):
    """Split a block into top-level items and the statements between them.

    A construct that starts with an item keyword at brace depth 0 is an item
    and runs until its `;` or its closing `}`; everything else is a statement.
    Attribute-only lines attach to the construct after them."""
    items, stmts = [], []
    pending = []  # attribute lines waiting for their construct
    depth = 0
    in_item = False
    for line in body:
        code = strip_noise(line)
        if depth == 0 and not in_item:
            if ATTR_RE.match(line):
                pending.append(line)
                continue
            if ITEM_RE.match(line):
                items.extend(pending)
                pending = []
                in_item = True
            else:
                stmts.extend(pending)
                pending = []
        (items if in_item else stmts).append(line)
        depth += code.count("{") - code.count("}")
        depth = max(depth, 0)
        if in_item and depth == 0:
            s = code.rstrip()
            if s.endswith(";") or s.endswith("}"):
                in_item = False
    items.extend(pending)
    return items, stmts


HAS_MAIN_RE = re.compile(r"^\s*fn\s+main\b", re.M)


def forms(body):
    """Candidate programs for a block, in the order to try them."""
    text = "\n".join(body)
    out = [("as written", text)]

    items, stmts = split_items_stmts(body)
    item_text = "\n".join(items)
    stmt_text = "\n".join(stmts)
    has_main = HAS_MAIN_RE.search(text) is not None
    if any(s.strip() for s in stmts):
        if any(s.strip() for s in items) and not has_main:
            out.append(("items kept, statements wrapped in fn main",
                        item_text + "\nfn main() {\n" + stmt_text + "\n}\n"))
        if not has_main:
            out.append(("statements wrapped in fn main",
                        "fn main() {\n" + stmt_text + "\n}\n"))
    if not has_main:
        out.append(("empty fn main appended", text + "\nfn main() {}\n"))
    return out


class Checker:
    def __init__(self, zexic, out_dir, run, verbose):
        self.zexic, self.out_dir, self.run, self.verbose = zexic, out_dir, run, verbose

    def compile_text(self, text, name):
        path = os.path.join(self.out_dir, name + ".zx")
        with open(path, "w", encoding="utf-8", newline="\n") as f:
            f.write(text)
        # File imports are shown for their shape; satisfy them with stubs.
        for m in IMPORT_RE.finditer(text):
            stub = os.path.join(self.out_dir, m.group(1))
            os.makedirs(os.path.dirname(stub) or self.out_dir, exist_ok=True)
            if not os.path.exists(stub):
                with open(stub, "w", encoding="utf-8") as f:
                    f.write("// stub for doc example import\n")
        p = subprocess.run([self.zexic, "check", path],
                           capture_output=True, text=True,
                           encoding="utf-8", errors="replace")
        out = (p.stdout + p.stderr).strip()
        return p.returncode == 0, out, path

    def run_text(self, text, name):
        ok, out, path = self.compile_text(text, name)
        if not ok:
            return ok, out, path
        try:
            p = subprocess.run([self.zexic, "run", path],
                               capture_output=True, text=True,
                               encoding="utf-8", errors="replace",
                               stdin=subprocess.DEVNULL, timeout=15)
        except subprocess.TimeoutExpired:
            return False, ("program did not exit (a server or a read loop? "
                           "mark its block <!-- check-only -->"), path
        return p.returncode == 0, (p.stdout + p.stderr), path

    def check(self, block):
        """Return None when the block behaves, else a failure description."""
        start, body, expected, marker = block
        if marker == "fragment" or not any(s.strip() for s in body):
            return None
        name = self.name_for(block)
        want_error = has_expected_error(body)

        compiled = []
        for label, text in forms(body):
            if self.run and not want_error and label == "as written" and marker != "check-only":
                ok, out, path = self.run_text(text, name)
            else:
                ok, out, path = self.compile_text(text, name)
            compiled.append((label, ok, out, path))
            if ok:
                if want_error:
                    return ("line %d: expected a compile error, but the block compiled (%s)"
                            % (start, label))
                if self.run and label == "as written" and marker != "check-only":
                    problem = self.compare_output(name, out, expected, path)
                    if problem:
                        return problem
                if self.verbose:
                    print("  ok  %s:%d (%s)" % (self.doc, start, label))
                return None

        if want_error:
            if self.verbose:
                print("  ok  %s:%d (rejected as expected)" % (self.doc, start))
            return None
        label, ok, out, _ = compiled[0]
        return ("line %d: block does not compile (tried %s)\n%s"
                % (start, ", ".join(l for l, _, _, _ in compiled), indent(out)))

    def compare_output(self, name, out, expected, path):
        if expected is None:
            if self.verbose and out.strip():
                print("  out %s\n%s" % (name, indent(out)))
            return None
        want = "\n".join(expected)
        if out != want and out != want + "\n":
            return ("%s: output mismatch\n--- got ---\n%s--- want ---\n%s"
                    % (name, indent(out), indent(want)))
        return None

    def name_for(self, block):
        base = os.path.splitext(os.path.basename(self.doc))[0]
        return "%s-L%d" % (base, block[0])


def indent(text):
    return "".join("    " + line + "\n" for line in text.rstrip("\n").split("\n"))


def find_compiler(exe):
    if exe:
        return exe
    go = shutil.which("go")
    if not go:
        raise SystemExit("no --exe given and no `go` on PATH to build zexic with")
    out = os.path.join(ROOT, "build", "doc_examples", "zexic")
    os.makedirs(os.path.dirname(out), exist_ok=True)
    p = subprocess.run([go, "build", "-o", out, "./cmd/zexic"], cwd=ROOT)
    if p.returncode != 0:
        raise SystemExit("go build ./cmd/zexic failed")
    return out


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("docs", nargs="*", help="markdown files (default: README.md and docs/*.md)")
    ap.add_argument("--run", action="store_true", help="run complete programs and compare their output")
    ap.add_argument("--exe", help="path to a built zexic")
    ap.add_argument("--out-dir", default=os.path.join(ROOT, "build", "doc_examples"))
    ap.add_argument("-v", "--verbose", action="store_true")
    args = ap.parse_args()

    os.makedirs(args.out_dir, exist_ok=True)
    zexic = find_compiler(args.exe)
    checker = Checker(zexic, args.out_dir, args.run, args.verbose)

    docs = args.docs or [os.path.join(ROOT, d) for d in DOCS]
    total = failed = 0
    for doc in docs:
        checker.doc = os.path.relpath(doc, ROOT)
        for block in read_blocks(doc):
            if block[3] == "fragment" or not any(s.strip() for s in block[1]):
                continue
            total += 1
            problem = checker.check(block)
            if problem:
                failed += 1
                print("FAIL %s:%d\n%s" % (checker.doc, block[0], problem))
    print("%d block(s) checked, %d failed" % (total, failed))
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
