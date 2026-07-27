# Service-Oriented Ingestion Runtime

Status: **superseded 2026-07-26** by `document-flow-services.md`, which keeps
the five-service decomposition but inverts the topology: one hub service
decides and settles, four worker services do the work, every edge is
request/response, and a "document" — not an identity — is what crosses the
wire. The `Service` interface, the loopback REST boundary, the per-run bearer
token, the per-service log files, and the dashboard rectangles below all
survive unchanged. What is replaced is D1's producer→consumer chaining, the
fire-and-forget `Feed`, and the three deadlock rules that chaining required.
Read this document for why services exist; read the successor for who calls
whom.

Original status: shipped 2026-07-26. Superseded the single-coordinator composition
in `concurrent-streaming-pipeline.md`; `modules/pipeline/concurrent.py` is
deleted. That document's dataflow, its narrow email-compaction barrier, and
every one of its non-negotiable invariants are retained unchanged — this
design only replaces *who owns* each phase, giving each concern a named
service with its own queue, worker pool, REST interface, log file, and
dashboard rectangle.

Implementation: `modules/services/` (`base`, `state`, `api`, `discovery`,
`emails`, `pdftotext`, `summarisation`, `embedding`, `orchestrator`).

## Runtime shape: threads, not processes

Services are threads in one process. Two reasons, in order of weight:

1. **One relational writer.** Five processes writing SQLite would make
   entity-id assignment, and therefore chunk identity, depend on process
   scheduling — breaking system acceptance invariant 2. The single owning
   thread below is what keeps identity deterministic.
2. **The GIL is not the constraint.** This interpreter is a standard build
   (`Py_GIL_DISABLED=0`, `sys._is_gil_enabled()` is true), but every hot phase
   releases the GIL anyway: `hashlib` for discovery, `subprocess` for
   ocrmypdf/pdftotext, sockets for inference. A measured cold run keeps 10 OCR
   workers and 8 embedding workers genuinely busy at once.

The REST boundary is real HTTP over loopback rather than method calls, so
lifting a service into its own process later is a transport change, not a
redesign. The relational writer is the one thing that could not follow it.

## Goal

Full ingestion is a set of five independent services connected by queues.
Every service is the same shape: an inbound queue, a bounded worker pool, a
REST API, live statistics, its own log file, and one open→close lifecycle.

```text
                    ┌──────────────────┐
                    │ 1. Discovery     │  walk · hash · integrity
                    └───┬──────────┬───┘
             emails     │          │     pdfs
                    ┌───▼──────┐   │
                    │ 2. Emails│   │
                    └─┬──────┬─┘   │
          attached pdf│      │     │
                    ┌─▼──────▼─────▼───┐
                    │ 3. PDF-to-Text   │  ocrmypdf · pdftotext
                    └────────┬─────────┘
                             │ text
   ┌──────────────────┐      │
   │ 4. Summarisation │──┐   │           (opened at email close)
   └──────────────────┘  │   │
                       ┌─▼───▼────────────┐
                       │ 5. Plain-text    │  embed · publish vector
                       │    Embedding     │
                       └──────────────────┘
```

"Service A feeds service B" means: A's worker, having produced a durable
result, POSTs an item identity to B's REST API and moves on. A never waits for
B, and never calls B's Python objects.

## Non-negotiable invariants

The seven invariants of `concurrent-streaming-pipeline.md` are carried over
verbatim and are not restated here. This design adds four:

- **S1. One relational writer.** All SQLite mutation, review-log writes,
  canonical artifact publication, and chunk/FTS maintenance execute on the one
  `StateWriter` thread. Service workers submit a callable and await its
  result; they never hold the connection. This is the shipped
  sole-coordinator invariant, now enforced by construction rather than by
  convention.
- **S2. Nothing is decided twice across the API.** A feed carries a candidate
  id, a document id, or a thread id — identity, resolved by the receiver.
  The one exception is the embedding lane, which carries the payload text and
  its content-addressed target path: the producer has *already* derived them
  deterministically, and re-deriving on the far side would mean a second
  database read for a value that cannot differ. What is forbidden is a
  decision that could come out differently depending on which side made it.
- **S3. Local, authenticated, ephemeral.** Every service binds
  `127.0.0.1:0` and requires a per-run bearer token minted with `secrets`.
  A run's ports and token exist only for that run's lifetime. Nothing is
  reachable off the loopback interface and no other local process can inject
  work into a corpus pipeline.
- **S4. A service is closed only when its input is closed *and* its queue is
  empty *and* every item it produced downstream has been accepted.** Closure
  is ordered by the orchestrator along the dependency graph; a service cannot
  close itself.

## D1. The `Service` interface, and its two backings

One interface, so the REST layer, the dashboard, the log wiring, and the
lifecycle are written once:

```python
class Service(ABC):
    name: ClassVar[str]           # "discovery", "emails", …
    detail: ClassVar[str]         # one-line dashboard subtitle
    def submit(self, item: dict) -> bool      # accept one work item
    def close(self) -> None                   # close input, drain, settle
    def abort(self) -> None                   # interrupt path
    def stats(self) -> ServiceStats           # live, lock-consistent
```

Two backings implement it, because the codebase already contains two proven
execution machines and neither should be rewritten to gain a REST door:

| backing | execution | services |
|---|---|---|
| `QueueBackedService` | own `ServiceQueue` + `WorkerPool` | Discovery, Emails |
| `PoolBackedService` | an existing `BoundedInferenceDispatcher` / `StreamingPdfProducer` | PDF-to-Text, Summarisation, Embedding |

`ServiceStats` is a frozen dataclass read under one lock, so derived figures
can never disagree:

```text
accepted  queued  in_flight  done  failed  skipped  state  since
```

`PoolBackedService` maps the wrapped machine's existing `QueueSnapshot` onto
that shape. The dispatchers keep their separate endpoint budgets: embedding
and summarisation remain independent capacity, exactly as
`embedding-queue-and-workers.md` decision 2 requires.

## D2. `StateWriter` — the coordinator, made explicit

`StateWriter` owns the `sqlite3.Connection`, the `ReviewLog`, and one
dedicated thread. Its only public surface:

```python
writer.run(fn, *args)      # execute on the writer thread, return the result
writer.post(fn, *args)     # execute on the writer thread, return a Future
```

Every stage object a service composes (`EmailStage`, `PdfTextStage`,
`ThreadStage`, …) is invoked *inside* `run()`, so all of their existing
`self.conn` usage lands on the one thread that owns the connection. No stage
implementation changes.

`ingest all` opens its connection with `Database.open(handed_off=True)`,
which drops sqlite3's `check_same_thread` guard. That guard cannot express
"owned by exactly one thread that is not the creator", which is the actual
rule here; `StateWriter.assert_owner()` enforces the stricter one, and every
other entry point (named stages, query, daemon, accuracy) keeps the sqlite3
check.

`run()` called *from* the writer thread executes inline rather than
deadlocking on itself — composing two stages correctly must not be punished.
`set_idle()` gives the writer work to do between units, which is how PDF
completions keep landing while a long summary drain holds it.

Two deadlocks this shape can create, and how they are avoided:

- The Emails worker runs its parse *on* the writer. It therefore pokes
  PDF-to-Text from outside that unit, never inside it.
- PDF-to-Text's `submit()` runs on an HTTP thread and needs the writer, so it
  uses `post()` (fire-and-forget) rather than `run()`. Blocking there would
  let a request wait on a writer that is busy serving the request's own
  producer.

A service worker that needs the database blocks on `run()`. That is
deliberate: relational settlement was always serial, and pretending otherwise
would trade a real invariant for an imaginary speedup. The parallelism that
matters — hashing, OCR, inference — happens before the worker ever calls
`run()`.

## D3. Transport

`http.server.ThreadingHTTPServer` per service, bound to `127.0.0.1:0`;
`httpx.Client` with keep-alive per feed. Four endpoints, identical everywhere:

| method | path | meaning |
|---|---|---|
| `GET` | `/health` | service name, state, port |
| `GET` | `/stats` | the `ServiceStats` fields |
| `POST` | `/items` | `{"items": [...]}` → accept; returns `202` |
| `POST` | `/close` | close input (orchestrator only) |

`POST /items` returns as soon as the items are queued. Submission is buffered
and never blocks a producer (streaming invariant 5: *execution* is bounded,
submission is not). A `503` is returned once input is closed, and the caller
treats it as a fatal wiring error, not as backpressure.

`ServiceHost` owns the servers, the token, the client pool, and the address
book. `ServiceHost.feed(source, target)` is how a service reaches a
downstream, so no service holds a reference to another service object.

A `Feed` is not a bare client: it is the producer's own outbound queue plus a
sender thread that drains it into the target's API in batches of up to 64.
That is what makes "A feeds B" non-blocking — a worker's `send()` returns as
soon as the item is queued, and the loopback round-trip happens elsewhere.
`flush()` is how the orchestrator proves a closing producer's items were
actually accepted, and it re-raises any delivery failure the sender recorded.
One sender per lane, so submission order is preserved and a failure is
reproducible.

## D4. Per-service log files

`modules/logs.py` gains `open_service_log(config, run_id, service)`. It
returns a `Log` bound to the logger `pocket_advisor.service.<name>`, which

- writes its own `<run-stem>-<service>.jsonl` beside the run log, and
- propagates to the run logger, so the complete run remains readable in one
  file.

The `caller` field keeps naming our own modules by path; only the logger
identity changes. Service logs are file-only by convention: a service uses
`.info()` for its own record and reserves `.notice()`/`.error()` for messages
that genuinely belong on the operator's terminal.

## D5. Dashboard rectangles

`IngestDashboard` gains a `Services` region rendering one bordered rectangle
per service, in feed order, each showing:

```text
┌ 3 · PDF-TO-TEXT ─── running ──┐
│ ocrmypdf · pdftotext          │
│ 12 queued · 4 in flight       │
│ 61 done · 1 failed            │
│ 4 workers · 0.8/s · 3m 12s    │
└───────────────────────────────┘
```

Border colour carries state (dim pending, cyan running, green closed, red
failed). The existing `Pipeline` panel of seven logical stages stays: reports,
named-stage commands, and `ingest_report.py` are defined in terms of those
stage names, and services are a different decomposition of the same run, not
a replacement for it. Active-work widgets and the event panel are unchanged.

Rectangles are laid out in a `Table.grid` that wraps by terminal width, so a
narrow terminal stacks them instead of truncating.

## D6. Lifecycle and closure order

The orchestrator (`ServiceIngest`) drives phases; services never sequence each
other.

1. Start `StateWriter`, host, and all five services.
2. Resume durable gaps: pending candidates and pending PDF documents are
   submitted before new discovery events, so an interrupted run converges.
3. Seed Discovery with the workspace's collections; it walks and hashes on its
   worker pool, settles each file's integrity decision through `StateWriter`,
   and feeds the resulting candidate to Emails or PDF-to-Text.
4. Close Discovery → the `discover` logical stage completes, after each
   collection's blob snapshot has been atomically installed at its own close.
5. Close Emails → the corpus-wide compaction barrier runs, every remaining
   authored body is published, and its leaf embeddings are fed. The `emails`
   stage completes.
6. Run `thread` on the writer.
7. Open Summarisation with the stale jobs; it generates on its pool while
   PDF-to-Text is still working. PDF settlement is serviced from the writer's
   idle callback, exactly as the shipped design does.
8. Close Summarisation, then close PDF-to-Text.
9. Close Embedding *last*: `EmbedStage` drains readiness work, converges gaps,
   and rebuilds both matrices. Only then is the run's inference finished.
10. `transactions`, then the report.

Ctrl+C aborts every service through one path: input closed, queued work
dropped, OCR process groups terminated, in-flight inference abandoned, servers
shut down, writer stopped. Unpublished entities remain durable pending gaps.

## What does not change

- The seven public logical stage names, their reports, and named-stage
  ordered-prefix execution. `ingest pdfs` still runs `discover…pdfs` serially
  with no services started.
- Chunk identity, thread keys, reply edges, summary digests, vector identity.
- `PdfTransformCache` gates, write-verify-publish, atomic vector publication,
  the blob-index snapshot rule, and the compaction barrier.
- Every existing stage class. Services compose them; they do not fork them.

## Acceptance criteria

All verified 2026-07-26.

1. Each of the five services answers `GET /health` and `GET /stats` on its own
   loopback port during a run, and returns `401` without the run token —
   `test_service_ingest.py`, `test_services.py`.
2. An item crosses a service boundary over HTTP; no service holds a reference
   to another service object — `ServiceHost.feed()` is the only path.
3. Relational work observes exactly one thread, and `assert_owner()` rejects
   any other — `test_services.py`, plus writer-thread assertions on every
   settlement seam in `test_service_ingest.py`.
4. The streaming criteria hold: an email reaches embedding while discovery is
   still hashing, an attached PDF is offered before discovery finishes, PDF
   settlement continues during summary generation, and replies compact
   identically — `test_service_ingest.py`.
5. Five service log files are written per run, each carrying that service's
   bind, milestones, and closing counts; the run log still contains every
   record through logger propagation.
6. The dashboard shows five rectangles with live per-service statistics, and
   stacks rather than truncating on a narrow terminal —
   `test_runtime_dashboard.py`.
7. Named-stage runs, non-TTY output, reporting, verification, and every
   existing fixture remain green — 22/22 `pocket-advisor.py test`.
8. A cold full ingest completes with real OCR and inference: 56 emails, 23
   attached PDF occurrences over 14 unique transforms on 10 workers, 197 leaf
   vectors published through the REST hop; re-running creates no duplicate
   entities and no new chunks.
9. `SIGINT` mid-run exits `130` in under a second, leaves no stray OCR child
   processes, records the interrupted run report, and the resumed run
   converges to the identical 197-vector index.
