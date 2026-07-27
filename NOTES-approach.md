# One binary per implementation: what it buys, what it costs

Investigation of the multi-binary layout in this repository against the single-binary harness in
`lneto/examples/_import_examples/httpbench`, and what it takes to get the same figures back out.

## What the split buys

**Binary size becomes a measurable result.** Measured here, stripped (`-trimpath -ldflags="-s -w"`,
go1.26.3, linux/amd64):

| Binary | Size |
|---|--:|
| `implementations/httphi` (httφ + rawsock + net) | 2.78 MB |
| equivalent `net/http` server (hello-world + memstat) | 5.82 MB |

That number does not exist in a single-binary harness, because both stacks are linked into it.

**The import graph stops lying.** In the old harness every measurement of httφ ran in a process that
had `net/http` linked, initialised and hot: its `sync.Pool`s, its `textproto` tables, its `init`
work. Per-request deltas mostly survived that, but every whole-process number (live heap at rest,
total mapped, thread count) was contaminated. Here a server links only what it serves.

**Whole-process metrics become comparable.** RSS, peak RSS (`VmHWM` in `/proc/<pid>/status`), thread
count, startup time and GC configuration are now attributable to one stack. They are also
language-agnostic: a Rust or C implementation can join the table later, which `runtime.MemStats`
never allows.

**TinyGo per implementation**, rather than a build tag through a program that also has to hold
`net/http`.

**Plotting stops being contortion.** gonum lives in a normal module here; lneto keeps its empty
`go.mod` and the `_import_examples` workaround disappears.

**Each implementation is readable.** A reviewer can see the whole server being measured on one
screen and judge whether it is the code they would have written.

## What the split costs

### 1. Conformance is no longer free

The property worth keeping from the old harness is not the table, it is this: both servers answered
with a digest of everything they parsed, the requester knew the expected digest, and **no row was
reported from a server that answered wrongly**. It was the benchmark and the conformance test at
once. With one binary per implementation, handlers drift apart silently and nothing notices.

Keep it by putting the corpus, the digest and the response contract in the root `httpbench` package,
have every implementation import it, and make the orchestrator verify each built binary over the
whole corpus before it measures a single row.

### 2. The corpus currently lives where this module cannot reach it

`lneto/internal/ltesto/httpgen.go` holds the generator and the digest. Go's `internal` rule is
path-prefix based, so `github.com/soypat/httpbench` cannot import it — that is why `rawsock` moved to
`x/`. Decide before writing more implementations:

- export it from lneto (`x/ltesto`, or an `httptest`-shaped package), or
- copy the generator into `httpbench` and let lneto keep its own copy for its tests.

The second is honest for a benchmark repository: the corpus is the benchmark's, not the library's.

### 3. In-band metrics measure themselves

`HandleMemstats` is convenient and language-neutral, but as an in-band measurement it has four
problems:

- `runtime.ReadMemStats` stops the world. Inside a measured window that shows up as latency.
- Serving the endpoint allocates ~2 kB on the `net/http` side and near zero on the httφ side, so the
  act of reading the counter perturbs each implementation by a different amount.
- `appendJSONDictKey(dst []byte, key string, val any, ...)` boxes a `uint64` into `any`: one
  allocation per key, per call. The metrics endpoint allocates, on the side of the comparison whose
  whole claim is that it does not.
- `MemStats` has no goroutine stacks, no thread count and no GC CPU. The old harness read those from
  `runtime/metrics`.

Keep the endpoint — it is the only thing a non-Go implementation can offer — but treat it as an
out-of-band sample: hit it on its own connection, **before and after** the measured window, never
during, and quote the endpoint's own cost by sampling twice back to back and subtracting. For Go
implementations, also expose the `runtime/metrics` subset the old sampler used
(`/gc/heap/allocs:{bytes,objects}`, `/memory/classes/heap/{objects,stacks}:bytes`,
`/memory/classes/total:bytes`, `/gc/cycles/total:gc-cycles`, `/cpu/classes/gc/total:cpu-seconds`,
`/sched/goroutines:goroutines`) plus `pprof.Lookup("threadcreate").Count()`.

The universal fallback needs no cooperation from the server at all: read `VmRSS`/`VmHWM` from
`/proc/<pid>/status` around the window. Measured at rest: httφ 4.25 MB, `net/http` 4.32 MB — RSS is
coarse, so it belongs next to the allocation numbers, not instead of them.

### 4. Orchestration gains a build step

Ports, readiness, kill-and-restart per row already existed through self-exec. New: building N
binaries, keeping their flags meaning the same thing, and making sure a stale binary is never
measured. `go build -o bin/<impl> ./implementations/<impl>` before each matrix, size recorded at that
moment.

### 5. Fairness has to be re-stated per implementation

`Flags` standardises `-sz-req`, `-sz-usr`, `-J`. A `net/http` implementation has no counterpart for
most of them; `-sz-req` maps to `Server.MaxHeaderBytes` and the rest do not map at all. Whatever
mapping is chosen must be printed in the report header, as the old one printed its limits row.

### 6. Size comparisons need discipline

Strip and `-trimpath` both sides; never link driver or plotting code into a server binary; state that
the httφ binary still links `net` for `net.Conn` and `net.Addr`.

## Defects in the current code

Found while reading and running it, all in the new repository:

- `flags.go:72` — `Listen` ends with `return l, nil`, discarding `err` from `rawlistener.Listen` and
  `net.Listen`. A failed bind is reported as a working listener.
- `flags.go:93` — `rawConn.Close` returns the connection to the pool but never closes the socket. The
  peer is never told the answer ended: `curl http://127.0.0.1:PORT/hello-world` hangs and exits 124.
  File descriptors accumulate, and a pooled connection can be handed to a new accept while the old
  socket is still open.
- `implementations/httphi/main.go:104` — `buf = appendJSONDictKey(dst, "totalalloc", ...)` writes into
  `buf`, and the next line appends `mallocs` to `dst` instead. `totalalloc` never reaches the answer.
- `implementations/httphi/main.go:111` — `val any` boxing allocates per key, as above.
- `implementations/httphi/main.go:64` — unreachable `return nil`; `go vet ./...` fails, so the gate is
  red today.
- `HandleHelloWorld` stages no `Content-Length`, so the answer is delimited by connection close — which
  the `Close` bug above never performs.

## Figures: trim to five

The old harness drew six. Recommended set, in the order a reader should meet them:

1. **Heap allocated per request**, log Y, one line per implementation, X = workload. The headline.
2. **Header flood**: heap per request against header block size, log/log, with the "one byte of heap
   per byte on the wire" reference line. Bounded versus unbounded, in one picture.
3. **Static footprint**: grouped bars of stripped binary size, RSS at rest, and heap held per idle
   connection. This figure is new and is what the split makes possible; it absorbs the old
   per-connection figure.
4. **Throughput** against open connections.
5. **p99 latency** against open connections.

Dropped: *allocations per request*. It tells the same story as heap per request, and the count is
already in the table for anyone who wants it.

## How to get those figures here

Everything below `results.json` is portable: `results.go` and `plot.go` from the old harness have no
lneto imports and can be moved across as they are.

1. Move the corpus and digest into this module (decision in §2), with their tests.
2. Move `protocol.go`: the response contract both servers answer with, and the zero-allocation parser
   the requester checks it with. Every implementation must answer that shape, including the ones added
   later.
3. Move `driver.go` (pre-serialised requests, refusal retry, answer validation) and `stats.go`,
   re-pointed at the metrics endpoint plus `/proc` instead of `SIGUSR1`.
4. Write the orchestrator: for each implementation, build → record size → start → wait for readiness →
   verify corpus → sample → warm → sample → load → sample → read `VmHWM` → kill. One process per row,
   as before.
5. Move `results.go` and extend `Result` with `binaryBytes`, `rssAtRest`, `rssPeak`; extend `Report`
   with the per-implementation flag mapping.
6. Move `plot.go`, delete `drawAllocsPerRequest`, and add the static-footprint figure. Series names
   already read `lneto+raw.Conn/req`, `stdlib+net.Conn keep-alive`.
7. Only then add implementations. Each new one is: a `main.go`, a corpus verification run, and a row.
