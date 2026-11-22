// bench/match/match.rs — the same benchmark in Rust. argv[1] is the scale.
// Must print the same `match <scale> <checksum>` line as match.zx and
// match.c. All arithmetic wraps explicitly, matching the C twin's -fwrapv.

fn main() {
    let mut scale: i64 = std::env::args()
        .nth(1)
        .and_then(|s| s.parse().ok())
        .unwrap_or(1);
    if scale < 1 {
        scale = 1;
    }
    let mut acc: i64 = 1;
    let mut k: i64 = 0;
    while k < 50000000 * scale {
        let v = (k.wrapping_mul(7).wrapping_add(13)) % 101;
        acc = match k & 7 {
            0 => acc.wrapping_add(v),
            1 => acc.wrapping_sub(v),
            2 => acc.wrapping_mul(3).wrapping_add(v),
            3 => acc ^ v,
            4 => acc.wrapping_mul(2) ^ v,
            5 => acc.wrapping_neg().wrapping_add(v),
            6 => acc.wrapping_add(v.wrapping_mul(2)),
            _ => acc.wrapping_sub(v.wrapping_mul(3)),
        };
        k += 1;
    }
    println!("match {} {}", scale, acc);
}
