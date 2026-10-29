# 11. Concurrency: threads, channels, and atomics

Zexi's `thread` module gives real OS threads (pthreads) with a small,
handle-based API: **fork–join threads**, **i64 channels**, and **lock-free i64
atomics**. Import it with `use thread;` (or `import thread.task;`). The pthread
shim and `-pthread` are linked only when the module is used.

```zx
use thread;

fn square(x: i64) -> i64 { return x * x; }

fn main() -> i32 {
    let t = Thread::spawn(square, 21);
    println(t.join());          // 441
    0
}
```

## 11.1 Why arguments, not closures

Zexi does not have closures yet, so a spawned function cannot capture the
variables around it. Instead a spawned function **takes its inputs as
arguments** and **returns its result by joining**:

| Entry point | Runs on a new thread | Result |
| --- | --- | --- |
| `Thread::spawn(f, arg)` / `spawn(f, arg)` | `f(arg)` with `f: fn(i64) -> i64` | `f`'s `i64` return |
| `Thread::spawn2(f, a, b)` / `spawn2(...)` | `f(a, b)` with `f: fn(i64, i64) -> i64` | `f`'s `i64` return |
| `Thread::spawn_void(f)` / `spawn_void(f)` | `f()` with `f: fn()` | `0` |

`join(handle)` / `Thread.join()` blocks until the thread finishes and returns its
result. Joining an unknown or already-joined handle returns `0` rather than
misbehaving, so a double join is harmless.

```zx
import thread;

fn range_sum(lo: i64, hi: i64) -> i64 {
    let mut s: i64 = 0;
    let mut i: i64 = lo;
    while i < hi { s += i; i += 1; }
    return s;
}

fn main() -> i32 {
    let a = Thread::spawn2(range_sum, 0, 100);
    let b = Thread::spawn2(range_sum, 100, 200);
    println(a.join() + b.join());   // 19900
    0
}
```

## 11.2 Channels

A `Chan` carries `i64` values and blocks when full (send) or empty (receive).
`close` wakes every waiter and lets buffered values drain, so a `while recv`
loop terminates.

```zx
use thread;

fn feed(ch: i64) -> i64 {
    let mut i: i64 = 1;
    while i <= 5 {
        chan_send(ch, i);
        i += 1;
    }
    chan_close(ch);
    return 0;
}

fn main() -> i32 {
    let ch = Chan::new(4);              // capacity is clamped to 1..65536
    let p = Thread::spawn(feed, ch.id());
    let mut total: i64 = 0;
    let mut v: i64 = 0;
    while ch.recv(&mut v) { total += v; }
    p.join();
    println(total);                     // 15
    0
}
```

Because a worker cannot capture the `Chan` value, it receives the raw handle
from `ch.id()` and uses the free functions. Both forms exist:

| Owning method | Free function (for workers) |
| --- | --- |
| `Chan::new(cap)` | `chan_new(cap)` |
| `ch.send(v) -> bool` | `chan_send(handle, v) -> bool` |
| `ch.recv(&mut v) -> bool` | `chan_recv(handle, &mut v) -> bool` |
| `ch.try_recv(&mut v) -> bool` | `chan_try_recv(handle, &mut v) -> bool` |
| `ch.close()` | `chan_close(handle)` |
| `ch.len() -> i64` | `chan_len(handle) -> i64` |
| `ch.closed() -> bool` | `chan_closed(handle) -> bool` |

`send` returns `false` if the channel is closed. `recv` blocks; it returns
`false` only when the channel is closed *and* drained.

Because a full channel blocks senders, a producer and consumer must run
**concurrently**: draining in the same thread *after* joining the producers
deadlocks as soon as the buffer fills. Size the channel to the whole workload,
or drain while the producers are still running.

## 11.3 Atomics

An `Atomic` is a lock-free `i64` cell for counters shared without a channel:

```zx
use thread;

fn bump(counter: i64) -> i64 {
    let mut i: i64 = 0;
    while i < 1000 { atomic_add(counter, 1); i += 1; }
    return i;
}

fn main() -> i32 {
    let c = Atomic::new(0);
    let mut hs: [i64; 4] = [0; 4];
    let mut k: usize = 0;
    while k < 4 { hs[k] = spawn(bump, c.id()); k += 1; }
    k = 0;
    while k < 4 { join(hs[k]); k += 1; }
    println(c.get());                   // 4000
    0
}
```

| Owning method | Free function |
| --- | --- |
| `Atomic::new(v)` | `atomic_new(v)` |
| `a.add(d) -> i64` | `atomic_add(handle, d) -> i64` |
| `a.get() -> i64` | `atomic_get(handle) -> i64` |
| `a.set(v)` | `atomic_set(handle, v)` |

`add` returns the **new** value. `add`/`get`/`set` are atomic; a sequence of
them is not, so read-modify-write across two calls is still a race.

## 11.4 Task pools: spreading work across threads

`Thread::spawn` is fork–join: one thread per unit of work, joined once. That is
the wrong shape for "run this function over ten thousand inputs" — creating ten
thousand threads costs more than the work. A `Pool` keeps a fixed set of worker
threads alive and feeds them jobs:

<!-- check-only -->
```zx
use thread;

fn square(x: i64) -> i64 {
    return x * x;
}

fn main() -> i32 {
    let pool = Pool::new();            // 0 = one worker per processor
    println(pool.map(square, 1, 101)); // sum of 1^2..100^2
    pool.free();
    0
}
```

A job is `fn(i64) -> i64` plus one argument, because a worker cannot capture any
Zexi value yet. For bigger fan-out, submit several jobs and collect their
results by the sequence number `submit` returns:

<!-- check-only -->
```zx
use thread;

fn work(x: i64) -> i64 { return x * 2; }

fn main() -> i32 {
    let pool = Pool::new(4);
    let a = pool.submit(work, 20);
    let b = pool.submit(work, 21);
    pool.wait();
    println(pool.result(a) + pool.result(b));  // 82
    pool.free();
    0
}
```

| Method | Purpose |
| --- | --- |
| `Pool::new(workers = 0)` | start a pool; 0 means one worker per `cpu_count()`, at least two |
| `pool.ok()`, `pool.workers()`, `pool.id()` | did it start, how many workers, raw handle |
| `pool.submit(f, arg) -> i64` | queue a job; returns its sequence number, or -1 when the queue is full |
| `pool.wait() -> i32` | block until every submitted job finished |
| `pool.result(seq) -> i64` | a finished job's return value (0 until it finishes) |
| `pool.done() -> i32` | jobs finished so far |
| `pool.map(f, lo, hi) -> i64` | run `f(i)` for every `i` in `lo..hi`, sum the results |
| `pool.free()` | stop the workers and release the pool |
| `cpu_count()`, `MAX_POOL_JOBS`, `MAX_WORKERS` | processor count and limits |

`map` is the one-liner for a parallel loop, and it also handles ranges bigger
than the job queue: when the queue fills it waits for it to drain, banks the
finished results, and carries on, so nothing is lost and nothing grows without
bound. Results are order-independent by construction (a sum), which is what
keeps parallel code deterministic and testable.

A pool holds up to **4096 queued jobs** and 256 workers. When the queue is full
but every job has finished, the next `submit` recycles it — collect any results
you still need before submitting a queue's worth of new work.

## 11.5 What can cross a thread boundary

Only **i64 values** — arguments, return values, channel payloads, atomic cells,
pool jobs (`fn(i64) -> i64` plus an argument), and the handles to
threads/channels/atomics/pools themselves. No `String`, struct,
enum, or reference can be passed to or from a thread yet. That restriction is
what makes the primitives here data-race-free: there is no shared Zexi memory to
race on, and the C-side tables are mutex- or atomic-guarded.

This is a deliberate preview. The full model (see
[`03-memory-model.md`](03-memory-model.md#37-concurrency-safety)) adds `Send`
and `Sync` so owned values and shared references can cross threads with
borrow-checked safety. Until then:

- Use channels to move results between threads.
- Use atomics for counters, not for coordinating complex state.
- Sleep with `sleep_ms` from the `sys` module if a worker needs to yield.

## 11.6 Limits and lifetime

- At most **256 live threads**, **256 channels**, **256 atomics**, and **256
  pools** per process. Thread slots are reused after `join`; channel, atomic,
  and pool slots are not reclaimed, so long-running programs should create a
  bounded number of them (these are meant to be long-lived). A pool releases its
  workers and job memory in `free`, but its slot stays marked used.
- A pool holds up to **4096 jobs** and **256 workers**; `Pool::new` caps the
  request at 64. Channel capacity is clamped to `1..65536` values.
- Channel capacity is clamped to `1..65536` values.
- Handles are packed `(generation, slot)`, so a stale handle from a reused slot
  fails cleanly (returns `0`/`false`) instead of aliasing the new object.
- There is no thread-local storage, no cancellation, and no `join_all`; join
  handles explicitly.

## 11.7 Runnable example

[`tests/threads.zx`](../tests/threads.zx) exercises threads, channels, and
atomics; [`tests/pool.zx`](../tests/pool.zx) exercises the pool. Both are
compiled and run by the test suite, and both print the same output on every run.
A minimal fork–join program:

```sh
zexic run tests/threads.zx
```
