## How to use this RAG

When the user asks a question that requires facts from their personal
corpus (email/PDF in workspace <id>), you MUST query RAG before answering.

Query commands:

```bash
./pocket-advisor.py --workspace <workspace_id> query "question text" --json
./pocket-advisor.py --workspace <workspace_id> query "question text" --after 2024-01-01 --before 2024-12-31 --top-k 20 --json
./pocket-advisor.py --workspace <workspace_id> query "question text" --thread 42 --purpose correspondence --no-thread-context --json
```

THEN: Parse the JSON results:

Each leaf or document hit gives a source message-id/document, date, sender, and the
matched passage;

Each THREAD(sum) hit is a navigation summary, not
content — use it to locate the underlying emails, never to cite.

Compose the final answer strictly from those packets, citing every claim as
<message-id> (<date>, <sender>).

If the retrieved packets do not support a claim, state that explicitly.

Do not answer from prior knowledge when the question is about corpus contents.
