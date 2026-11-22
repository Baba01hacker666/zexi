// bench/strbuild/strbuild.rs — the same benchmark in Rust. argv[1] is the
// scale. Must print the same `strbuild <scale> <len> <hash>` line as
// strbuild.zx and strbuild.c.

fn main() {
    let mut scale: i64 = std::env::args()
        .nth(1)
        .and_then(|s| s.parse().ok())
        .unwrap_or(1);
    if scale < 1 {
        scale = 1;
    }
    let mut s = String::new();
    let mut i: i64 = 0;
    while i < 800000 * scale {
        s.push_str("item ");
        s.push_str(&i.to_string());
        s.push('\n');
        i += 1;
    }
    let mut h: i64 = 0;
    for &b in s.as_bytes() {
        h = (h * 131 + b as i64) % 2147483647;
    }
    println!("strbuild {} {} {}", scale, s.len(), h);
}
