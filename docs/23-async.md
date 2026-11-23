# 23. Async tasks and non-blocking I/O

`next_http.md` §5 lists the last language gap: a blocked read holds a thread,
so throughput is limited by `workers / handler_time`. The `async` module adds
stackful tasks and a readiness scheduler. A task that waits for I/O suspends
**inside its call stack**; while all tasks are waiting, one scheduler thread
blocks in `poll()` on all their descriptors at once. A waiting request costs a
poll entry, not a thread.

<!-- check-only -->
```zx
use async;
use net;

fn reader(fd: i64) -> i64 {
    await(fd);              // suspend until fd is readable
    println("readable");
    return 0;
}

fn main() -> i32 {
    let srv = listen_tcp("127.0.0.1", 8080);
    let t = async_spawn(reader, srv.fd as i64);
    async_run();            // drive every task to completion
    0
}
```

## 23.1 The pieces

| Function | What it does |
| --- | --- |
| `async_spawn(f: fn(i64) -> i64, arg)` | schedule `f(arg)` on a fresh fiber; returns a task handle (0 on failure) |
| `await(fd)` | compiler builtin: suspend the current task until `fd` is readable, then return `fd` |
| `yield_now()` | let another task run without waiting for I/O |
| `async_run()` | run the scheduler until no live tasks remain |

`await` is a compiler builtin (sema checks one integer descriptor, codegen
emits the runtime call). It needs `use async;` in the program — that is where
the C runtime lives. Outside a spawned task it is a no-op, so the same function
can be called directly in a test.

## 23.2 How it works

Each task gets a 64 KiB stack via `ucontext`. `await` saves the current context
and switches back to the scheduler. The scheduler keeps one `pollfd` per
waiting task and blocks in `poll()` when nothing is runnable; a ready descriptor
marks its task runnable, and the scheduler switches back into that task's stack
at exactly the point where it suspended. Locals and borrows survive the switch
because the stack is preserved — there is no state-machine lowering.

Up to 256 live tasks. All tasks share one OS thread: handlers must not do long
CPU-bound work or blocking calls other than `await`. A task waiting on a
descriptor that nobody makes ready stalls the scheduler, just as a blocking
read would stall a thread. MIR does not lower `await` yet; use the C backend
(the default).

## 23.3 Limits

- No `async fn` keyword, no general `Future<T>` type, no `async` traits. An
  awaitable is a descriptor; `await` means "readiness of this fd".
- No multi-threaded task stealing. The readiness poll is a single loop; it is
  the same shape as the HTTP server's poller, moved into the language runtime.
- Task stacks are fixed size; deep recursion inside a task can overflow them.
