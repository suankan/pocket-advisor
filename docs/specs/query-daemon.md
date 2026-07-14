# Spec: session-warm query daemon

Status: IMPLEMENTED 2026-07-13. Local Unix-socket process that keeps
embed + rerank models (and the vector matrix) loaded for an interactive
or agent session so each `query.py` call does not cold-start weights.

Related: docs/specs/warm-eval.md (in-process warm for `eval.py` only).
This daemon covers **interactive / agent multi-query** use.

## Goal

Cut repeated model-load cost when the user or agent runs many
`query.py` searches in one working session (rephrasings, thread
follow-ups, mid-draft lookups). Ranking math stays identical to cold
CLI `run_search`; only residency of weights changes.

## Non-goals

- Not a generative LLM chat session — no conversation history, no
  answer synthesis. Each request is an independent search.
- Not a cloud service — **localhost Unix socket only**, mode `0600`,
  under `workspaces/state/` (gitignored, machine-local).
- Not required for correctness — if the daemon is down, `query.py`
  falls back to cold in-process search (unless `--require-daemon`).
- Not a replacement for warm eval — `eval.py --mode warm` still loads
  in-process for harness isolation (no socket dependency).

## Design

### Process

```
venv/bin/python scripts/query_daemon.py serve   # foreground (or background)
# loads vectors + embed backend + rerank backend once
# listens on config.QUERY_DAEMON_SOCKET
# optional idle exit after QUERY_DAEMON_IDLE_SEC with no requests

venv/bin/python scripts/query_daemon.py status
venv/bin/python scripts/query_daemon.py stop
```

### Protocol (newline-delimited JSON over Unix stream socket)

Client → server (one JSON object + `\n`):

```json
{"op": "search", "question": "…", "top_k": 15,
 "include_privileged": false, "after": null, "before": null,
 "thread": null, "no_thread_context": false}
{"op": "ping"}
{"op": "status"}
{"op": "shutdown"}
```

Server → client (one JSON object + `\n`):

```json
{"ok": true, "result": { /* same shape as query.py --json */ }}
{"ok": true, "pong": true, "pid": 123, "fingerprint": {…}}
{"ok": false, "error": "message"}
```

Single-threaded accept/handle loop (model backends are not assumed
thread-safe). Concurrent clients queue on the listen backlog.

### Client (`query.py`)

1. If `QUERY_DAEMON_AUTO` (default true) and socket exists and is
   connectable → send `search` via daemon.
2. Else → local cold `run_search` (load models for this process only).
3. Flags:
   - `--no-daemon` — always cold local
   - `--require-daemon` — fail if daemon unreachable (agent sessions
     that expect warm)

Print a one-line stderr notice when using the daemon so operators can
tell warm vs cold: `query: via daemon (warm)`.

### Shared warm load

`query.WarmResources` — same object used conceptually by the daemon
and by `eval.WarmQuerySession` (eval may wrap it). Loads:

- DB connection (+ migrate)
- `load_vector_index()`
- `embedding_backends.get_backend()`
- `rerank_backends.get_backend()` if `RERANK_ENABLED`

Each `search(...)` calls `run_search` with those resources; **no
shared query history**.

### Config (free knobs)

| Key | Default | Meaning |
|---|---|---|
| `query.daemon_auto` | true | Client tries daemon when socket live |
| `query.daemon_idle_sec` | 1800 | Exit after N idle seconds; `0` = never |
| socket/pid paths | `workspaces/state/query_daemon.sock` / `.pid` | Not user-facing names of case data |

### Security / privacy

- Socket only under local `workspaces/state/`; not exposed on TCP/LAN.
- Same privilege rules as `query.py` (default exclude privileged).
- Case data never leaves the machine (AGENTS.md rule 4).

### Fingerprint on status

`status` / `ping` returns embed backend fingerprint + rerank backend
id + vectors `built_at` so a client can detect a stale daemon after
config/index change (daemon should be restarted after
`ingest.py --embed text` or model config flips).

## Acceptance criteria

1. Spec + RUNBOOK + AGENTS common-ops mention start/stop/use.
2. `query_daemon.py serve` loads once; second search does not reload.
3. `query.py "q"` with daemon up uses socket; without falls back cold.
4. `--no-daemon` / `--require-daemon` work.
5. Idle timeout exits cleanly when configured.
6. Unit test for protocol framing without loading real models
   (mock handler or encode/decode helpers).
7. CHANGELOG entry; DESIGN if as-built paths/behavior changed.

## Verification

```bash
venv/bin/python scripts/test_query_daemon.py
# optional live (needs models + index):
venv/bin/python scripts/query_daemon.py serve &
venv/bin/python scripts/query.py "test question" --json
venv/bin/python scripts/query_daemon.py stop
```
