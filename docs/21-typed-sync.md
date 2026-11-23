# 21. Typed synchronization

`next_http.md` §5 lists two language gaps that hurt server code: there is no
typed `Mutex<T>` / `RwLock<T>` (only a raw pthread mutex inside a `c """..."""`
block), and channels carry only `i64` with no `select` or timeout. This chapter
covers what the standard library now offers for the first gap and for the
"with a timeout" half of the second. The remaining items in §5 — typed
channels, traits, non-blocking I/O, and passing richer values across threads —
still need compiler work; see §21.4.

## 21.1 `Mutex<T>` and `MutexGuard<T>`

`use sync;` embeds the module and links `-pthread`. A `Mutex<T>` owns the value
it protects, so the type carries both the lock and the data:

```zx
use sync;

fn main() -> i32 {
    let counter = Mutex::<i64>::new(0);
    let mut guard = counter.lock();
    *guard.get_mut() += 1;
    println(guard.get());   // 1
    guard.unlock();
    0
}
```

`lock(&self)` blocks until the mutex is free and returns a `MutexGuard<T>` that
borrows it. `get()` returns `&T`, `get_mut()` returns `&mut T`, and `unlock(self)`
releases the mutex. `try_lock()` returns `Option<MutexGuard<T>>` — `None` when
someone else holds the lock. `ok()` reports whether the pthread object was
created and `free()` destroys it (call both only when no guard is alive).

Guards are not `Copy`, and user-defined `Drop` is not implemented yet, so a
guard must be released by calling `unlock()` (often via `defer g.unlock()`).
There is no RAII release at scope exit. A mutex is not reentrant: calling
`lock()` again while holding it deadlocks, which is why `try_lock()` exists.

## 21.2 `RwLock<T>`

`RwLock<T>` allows many readers or one writer. Its guards are
`RwLockReadGuard<T>` (`get()`) and `RwLockWriteGuard<T>` (`get()` / `get_mut()`),
both with `unlock(self)`. `try_read()` and `try_write()` are the non-blocking
forms.

```zx
use sync;

fn main() -> i32 {
    let cfg = RwLock::<String>::new(String::from("ready"));
    let r = cfg.read();
    println(r.get());       // ready
    r.unlock();

    let mut w = cfg.write();
    w.get_mut().push_str("!");
    println(w.get());       // ready!
    w.unlock();
    0
}
```

Because a generic type parameter is opaque (docs/12-generics.md §12.2), `T`
can be stored, borrowed, moved, and dropped — `Mutex<String>` and
`RwLock<Vec<i64>>` work — but a generic body cannot compare or call methods on
`T` without knowing its concrete type.

## 21.3 Channel timeouts and `select`

The `thread` module's `Chan` still carries `i64` values, but waiting on one no
longer has to be indefinite:

- `recv_timeout(&mut v, ms)` returns `1` with a value, `0` when the channel is
  closed and drained, `2` on timeout.
- `send_timeout(value, ms)` returns `1` on success, `0` when closed, `2` on
  timeout.
- `chan_select2(a, b, ms, &mut which, &mut out)` waits for the first value on
  either of two handles. It returns `1` with `which` set to `0` (first channel)
  or `1` (second) and the value in `out`, `0` when both are closed and drained,
  `2` on timeout. If both are ready, the first channel wins.

```zx
use thread;

fn main() -> i32 {
    let a = Chan::new(1);
    let b = Chan::new(1);
    let mut which: i64 = -1;
    let mut v: i64 = 0;

    println(chan_select2(a.id(), b.id(), 20, &mut which, &mut v)); // 2
    b.send(7);
    println(chan_select2(a.id(), b.id(), 100, &mut which, &mut v)); // 1
    println(which, v);                                             // 1 7
    0
}
```

`recv_timeout` and `send_timeout` use a real `pthread_cond_timedwait`, so they
wake as soon as the channel changes. `chan_select2` polls the two channels on a
1 ms tick: that keeps the two channel mutexes independent (no lock-ordering
hazard) at the cost of at most 1 ms of extra latency. An invalid handle counts
as closed.

## 21.4 Typed channels

`tchan_new::<T>(cap)` creates a typed channel and returns an **i64 handle**.
The queue is a `Vec<T>` living in C-allocated memory, sized with the new
`sizeof::<T>()` compiler builtin (docs/12-generics.md §12.15); a pthread mutex
and condition variable protect it. A worker gets the handle — still an `i64`,
so it crosses the thread boundary like any other — and reaches the queue
through a raw pointer cast:

```zx
use thread;

fn producer(h: i64) -> i64 {
    let s = String::from("hello");
    tchan_send::<String>(h, s);      // moves an owning String into the queue
    tchan_close::<String>(h);
    0
}

fn main() -> i32 {
    let ch = tchan_new::<String>(2);
    let t = Thread::spawn(producer, ch);
    let mut v = String::from("");
    while tchan_recv::<String>(ch, &mut v) { println(v); }
    t.join();
    tchan_free::<String>(ch);        // drops buffered values and the C memory
    0
}
```

`tchan_send` moves `T` into the queue and blocks while it is full; `tchan_recv`
moves the oldest value into `out` and blocks while empty; `tchan_try_recv` is
the non-blocking form; `tchan_close` wakes every waiter and lets buffered
values drain; `tchan_free` releases everything. `String`, `Vec`, and other
owning values work because the Zexi assignment `*out = v` is a move, so there
is no double free. This is how richer values now reach a worker: through a
typed channel handle rather than a general `Arc<T>`.

## 21.5 Shared values across threads

`shared_new::<T>(v)` puts an owning `T` in C-allocated memory behind an atomic
refcount and returns an i64 handle. `shared_clone` hands the same value to
another worker, `shared_get` borrows it, and `shared_drop` releases one
reference; the last drop returns `Some(value)` so the Zexi destructor runs
exactly once. The counter is a `zxt_atomic` slot, released with the memory on
the final drop.

```zx
use thread;

fn worker(h: i64) -> i64 {
    println(shared_get::<String>(h));
    shared_drop::<String>(h);
    0
}

fn main() -> i32 {
    let h = shared_new::<String>(String::from("payload"));
    let h2 = shared_clone::<String>(h);
    let t = Thread::spawn(worker, h2);
    t.join();
    shared_drop::<String>(h);
    0
}
```

`shared_get` returns a plain `&T` with no tracked lifetime — only i64 crosses a
thread boundary, so the borrow checker cannot see the handle. Keep at least one
reference alive for as long as any borrow (or any other thread) can use the
value; dropping the last reference while another thread is inside
`shared_get` is a use-after-free. For mutable shared state, pair a `Shared<T>`
with a `sync::Mutex<T>`.

## 21.6 What is still missing

- **`dyn Trait` and `Reader`/`Writer` stream traits.** Traits with bounds and
  static dispatch now exist ([`22-traits.md`](22-traits.md)), but there is no
  trait object and no generic body/stream abstraction to hang I/O off.
- **Non-blocking I/O / async.** A blocked read still holds a thread. This is
  the largest of the §5 gaps and is not addressed here.
