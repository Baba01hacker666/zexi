// bench/matmul/matmul.rs — the same benchmark in Rust. argv[1] is the scale.
// Must print the same `matmul <scale> <checksum>` line as matmul.zx and
// matmul.c. Every value is a multiple of 0.125, so the sum is exact.

const N: usize = 256;

fn main() {
    let mut scale: i64 = std::env::args()
        .nth(1)
        .and_then(|s| s.parse().ok())
        .unwrap_or(1);
    if scale < 1 {
        scale = 1;
    }
    let mut a = vec![0.0f64; N * N];
    let mut b = vec![0.0f64; N * N];
    let mut c = vec![0.0f64; N * N];
    for i in 0..N * N {
        a[i] = (i % 7) as f64 * 0.5;
        b[i] = (i % 13) as f64 * 0.25;
    }
    let mut round: i64 = 0;
    while round < 20 * scale {
        for r in 0..N {
            for k in 0..N {
                let av = a[r * N + k];
                for col in 0..N {
                    c[r * N + col] = c[r * N + col] + av * b[k * N + col];
                }
            }
        }
        round += 1;
    }
    let mut sum = 0.0f64;
    for i in 0..N * N {
        sum += c[i];
    }
    println!("matmul {} {}", scale, (sum * 8.0) as i64);
}
