# Workspace-Local Query Daemon

Status: **locked 2026-07-18**; native implementation lives in
`modules/daemon.py` and shares `modules.retrieval.run_search` with cold query
and accuracy execution.

The daemon removes repeated vector-matrix loading and inference-client
setup during an interactive retrieval session (model warmth itself is the
inference server's concern —
`docs/inference/inference-serving.md` decision 14). It changes resource
lifetime, not ranking or content semantics.

## Locked decisions

1. Each workspace has an independent Unix-domain socket and PID record below
   `<workspace-state>/runtime/`; there is no TCP listener or cross-workspace
   daemon.
2. The socket is mode `0600`. Requests and responses are newline-delimited
   JSON capped at 1 MiB. The server deliberately handles one request at a
   time — retrieval sessions are single-operator and request serialization
   keeps the daemon trivially simple.
3. Active leaf/thread matrices, their IDs, and a warm inference client load
   once into a typed `SearchResources`. Every request still calls the same
   native `run_search` used by cold CLI and the accuracy suite. No duplicate
   ranking path exists.
4. Searches are independent. The daemon retains no conversation history,
   answer state, or corpus narrative beyond the resources already required by
   retrieval.
5. `query` uses the daemon automatically when `query.daemon_auto` is true,
   falls back cold when it is unreachable, obeys `--no-daemon`, and fails
   closed with `--require-daemon`.
6. `status` reports the selected workspace, process ID, index
   fingerprint, loaded counts, and index build timestamps. Ingesting or
   switching index configuration requires a daemon restart before warm search
   uses the replacement matrix.
7. `stop` prefers the authenticated workspace socket shutdown operation. It
   never signals a PID that cannot be verified through that socket; stale
   runtime files are removed only when their PID is not alive.
8. Confirmed deletion of the active vector index or complete workspace state
   stops that workspace's daemon immediately before deletion. Other workspace
   daemons are untouched.

## CLI

```bash
./pocket-advisor.py --workspace <id> daemon serve [--idle-sec N]
./pocket-advisor.py --workspace <id> daemon status
./pocket-advisor.py --workspace <id> daemon stop

./pocket-advisor.py --workspace <id> query "question"
./pocket-advisor.py --workspace <id> query "question" --no-daemon
./pocket-advisor.py --workspace <id> query "question" --require-daemon
```

`idle-sec=0` means no idle exit. The daemon is optional for correctness and
never starts implicitly.

## Acceptance criteria

1. Two workspaces resolve different socket, PID, database, and vector paths.
2. A warm session loads retrieval resources once and multiple searches reuse
   them while producing the same result shape as cold `run_search`.
3. Auto fallback, forced cold, and required-daemon behavior are CLI-tested.
4. Ping, status, search, shutdown, malformed request, and unknown operation
   protocol paths have isolated fixture coverage.
5. Socket cleanup occurs on normal shutdown, idle exit, SIGINT, SIGTERM, and
   resource-load/bind failure.
6. Tests bind only temporary local Unix sockets and never open live workspace
   state or collection roots.
