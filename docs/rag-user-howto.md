# RAG User Howto

How to query the RAG engine. This is the interface contract for any agent
or caller that needs facts from the personal corpus.

## When to query

When the user asks a question that requires facts from their personal
corpus (email/PDF in a workspace), you MUST query RAG before answering.

Do not answer from prior knowledge when the question is about corpus
contents.

## Query CLI

```bash
./pocket-advisor.py --workspace <workspace_id> query "question text" --json
./pocket-advisor.py --workspace <workspace_id> query "question text" \
  --after 2024-01-01 --before 2024-12-31 --top-k 20 --json
./pocket-advisor.py --workspace <workspace_id> query "question text" \
  --thread 42 --purpose correspondence --no-thread-context --json
```

Query uses the selected workspace's native hybrid leaf/thread retriever.
It never searches unmounted collections or another workspace's database.

## JSON results

Each leaf or document hit gives a source message-id/document, date,
sender, and the matched passage.

Each THREAD(sum) hit is a navigation summary, not content — use it to
locate the underlying emails, never to cite.

## Answering rules

Compose the final answer strictly from the retrieved packets, citing
every claim as `<message-id> (<date>, <sender>)`.

If the retrieved packets do not support a claim, state that explicitly.

## Warm daemon

By default query uses the workspace's warm daemon when available and
falls back to cold retrieval otherwise:

```bash
./pocket-advisor.py --workspace <workspace_id> daemon serve
./pocket-advisor.py --workspace <workspace_id> daemon status
./pocket-advisor.py --workspace <workspace_id> daemon stop
./pocket-advisor.py --workspace <workspace_id> query "question" --no-daemon
./pocket-advisor.py --workspace <workspace_id> query "question" --require-daemon
```

`daemon serve` runs in the foreground and keeps the current leaf and
thread matrices plus a warm inference client loaded (model warmth is the
oMLX server's concern). Restart it after embedding or changing retrieval
model/index configuration. Its mode-`0600` Unix socket and PID record
live only below the selected workspace's `runtime/` directory.
