// bench/sort/sort.rs — the same benchmark in Rust. argv[1] is the scale.
// Must print the same `sort <scale> <checksum>` line as sort.zx and sort.c.
// The LCG wraps exactly like the C twin (`-fwrapv`), so the checksums agree.

const N: usize = 400000;

fn quicksort(a: &mut [i64], lo: i64, hi: i64) {
    if lo >= hi {
        return;
    }
    let pivot = a[hi as usize];
    let mut i = lo;
    let mut j = hi - 1;
    while i <= j {
        if a[i as usize] < pivot {
            i += 1;
        } else {
            a.swap(i as usize, j as usize);
            j -= 1;
        }
    }
    a.swap(i as usize, hi as usize);
    quicksort(a, lo, i - 1);
    quicksort(a, i + 1, hi);
}

fn main() {
    let mut scale: i64 = std::env::args()
        .nth(1)
        .and_then(|s| s.parse().ok())
        .unwrap_or(1);
    if scale < 1 {
        scale = 1;
    }
    let mut a: Vec<i64> = vec![0; N as usize];
    let mut checksum: i64 = 0;
    let mut round: i64 = 0;
    while round < 4 * scale {
        let mut x: i64 = 12345 + round;
        let mut i: i64 = 0;
        while i < N as i64 {
            x = x
                .wrapping_mul(6364136223846793005)
                .wrapping_add(1442695040888963407);
            a[i as usize] = (x & i64::MAX) % 1000000;
            i += 1;
        }
        quicksort(&mut a, 0, N as i64 - 1);
        checksum += a[0] + a[N / 2] + a[N - 1];
        let mut i: i64 = 0;
        while i < N as i64 {
            checksum += a[i as usize] % 97;
            i += 1;
        }
        round += 1;
    }
    println!("sort {} {}", scale, checksum);
}
