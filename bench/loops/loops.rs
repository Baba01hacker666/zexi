// bench/loops/loops.rs — the same benchmark in Rust. argv[1] is the scale.
// Must print the same `loops <scale> <checksum>` line as loops.zx and loops.c.

fn main() {
    let mut scale: i64 = std::env::args()
        .nth(1)
        .and_then(|s| s.parse().ok())
        .unwrap_or(1);
    if scale < 1 {
        scale = 1;
    }
    let mut acc: i64 = 0;
    let mut i: i64 = 0;
    while i < 5000 * scale {
        let mut j: i64 = 0;
        while j < 5000 {
            acc = (acc + (i * 31 + j) % 1000003) % 1000003;
            j += 1;
        }
        i += 1;
    }
    println!("loops {} {}", scale, acc);
}
