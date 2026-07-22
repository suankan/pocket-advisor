Here is a complete, production-ready **Design Document (Design.md)** tailored for your project. You can copy and paste this directly into your repository to serve as the baseline architecture for your `pocket-advisor` gateway.

---

# Design Document: Pocket Advisor RAG Gateway

**Status:** Draft / Proposed

**Author:** Pocket Advisor Team

**Date:** July 2026

**Pattern:** OpenAI-Compatible Reverse Proxy / Gateway

---

## 1. Executive Summary

The **Pocket Advisor RAG Gateway** is an API gateway that transforms the core `pocket-advisor` retrieval engine into an OpenAI-compatible web service.

By exposing standard `/v1/chat/completions` and `/v1/models` endpoints, the gateway allows **any** standard LLM client UI (e.g., Open WebUI, LibreChat, Chatbox, Cursor, or custom web apps) to seamlessly query the domain knowledge base without needing bespoke API integrations. The gateway handles request parsing, RAG context retrieval, prompt enrichment, dynamic LLM routing (supporting BYO-LLM), and Server-Sent Events (SSE) streaming.

---

## 2. High-Level Architecture

```
                        ARCHITECTURE DIAGRAM
┌────────────────────────┐
│   Client Layer         │  Open WebUI / LibreChat / Custom React App / Cursor
└───────────┬────────────┘
            │  POST /v1/chat/completions (OpenAI Schema)
            ▼
┌────────────────────────────────────────────────────────────────────────┐
│   Pocket Advisor RAG Gateway (FastAPI)                                 │
│                                                                        │
│   ┌──────────────────────┐      ┌──────────────────────────────────┐   │
│   │  Middleware & Auth   │ ───> │  RAG Pipeline                    │   │
│   │  (Bearer / Headers)  │      │  • Extract query from last msg   │   │
│   └──────────────────────┘      │  • Run pocket-advisor retrieval  │   │
│                                 │  • Format chunks & citations     │   │
│                                 └────────────────┬─────────────────┘   │
│                                                  │                     │
│   ┌──────────────────────────────────────────────▼─────────────────┐   │
│   │  Prompt Enricher & Router                                      │   │
│   │  • Inject context into System / User Message                   │   │
│   │  • Map target model & forward client credentials               │   │
│   └──────────────────────────────┬─────────────────────────────────┘   │
└──────────────────────────────────┼─────────────────────────────────────┘
                                   │  Enriched Request
                                   ▼
┌────────────────────────────────────────────────────────────────────────┐
│   Downstream LLM Providers                                             │
│   (Local Ollama / vLLM / OpenAI / Anthropic via LiteLLM Router)        │
└────────────────────────────────────────────────────────────────────────┘

```

---

## 3. Data Flow & Sequence

1. **Client Request:** The client sends a standard OpenAI `POST /v1/chat/completions` payload containing conversation history, target model name, and streaming flags.
2. **Query Extraction:** The gateway inspects the last `user` message in the `messages` array.
3. **Retrieval Phase:** The gateway passes the query to the core `pocket-advisor` retrieval pipeline (vector search, metadata filtering, optional re-ranking).
4. **Context Injection:** Retracted text chunks are formatted and injected into the payload.
5. **Upstream Dispatch:** The modified payload is dispatched via a multi-provider router (e.g., LiteLLM) to the requested backend LLM (Ollama, vLLM, OpenAI, Anthropic).
6. **Streaming Response:** The Gateway streams tokens back to the client as OpenAI-formatted Server-Sent Events (`data: {"choices": [{"delta": {"content": "..."}}]}`).
7. **Citations Payload:** During the final stream frame, structured citation metadata is appended inside `choices[0].delta.citations` or a custom SSE event frame.

---

## 4. API Interface Contract

### 4.1. List Models (`GET /v1/models`)

Exposes virtual models to client drop-downs, combining RAG presets with target LLM backends.

```json
{
  "object": "list",
  "data": [
    {
      "id": "pocket-advisor+llama3",
      "object": "model",
      "created": 1721472000,
      "owned_by": "pocket-advisor"
    },
    {
      "id": "pocket-advisor+gpt-4o",
      "object": "model",
      "created": 1721472000,
      "owned_by": "pocket-advisor"
    }
  ]
}

```

### 4.2. Chat Completions (`POST /v1/chat/completions`)

#### Request Schema

Supports standard OpenAI properties alongside optional RAG parameters inside an extended `rag_options` object.

```json
{
  "model": "pocket-advisor+gpt-4o",
  "messages": [
    {
      "role": "user",
      "content": "How do we handle production deployment rollbacks?"
    }
  ],
  "stream": true,
  "temperature": 0.2,
  "rag_options": {
    "top_k": 5,
    "score_threshold": 0.7,
    "bypass_rag": false
  }
}

```

#### Response Stream (SSE Format)

Standard `text/event-stream` format compatible with all OpenAI client libraries.

```http
HTTP/1.1 200 OK
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1721472005,"model":"pocket-advisor+gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1721472005,"model":"pocket-advisor+gpt-4o","choices":[{"index":0,"delta":{"content":"Based"},"finish_reason":null}]}

...

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1721472006,"model":"pocket-advisor+gpt-4o","choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"}],"citations":[{"id":"doc_102","title":"Deployment Strategy","source_url":"s3://notes/deploy.md","score":0.89}]}

data: [DONE]

```

---

## 5. Key System Components

### 5.1. Prompt Injector Strategy

The gateway supports two prompt injection modes based on configuration:

* **System Prompt Injection (Default):** Injects retrieved context into a prepended `system` message.
```text
You are Pocket Advisor, a helpful assistant. Use ONLY the following context to answer the user's request:
--- CONTEXT START ---
[Doc 1] Title: Deployments
...chunk text...
--- CONTEXT END ---

```


* **User Message Wrap (Fallback):** For models that ignore or poorly enforce system instructions, the context is wrapped directly into the latest `user` message turn.

### 5.2. BYO-LLM & Credentials Forwarding

To allow clients to use their own LLMs, the gateway supports key pass-through headers:

* `Authorization: Bearer <GATEWAY_TOKEN_OR_PROVIDER_KEY>`
* `X-OpenAI-Api-Key`: Explicit key forwarded to OpenAI backends.
* `X-Anthropic-Api-Key`: Explicit key forwarded to Anthropic backends.

If no provider key is supplied by the client, the gateway defaults to server-side configured environment keys (`OPENAI_API_KEY`, `OLLAMA_BASE_URL`).

---

## 6. Non-Functional Requirements & Guardrails

| Requirement | Target | Strategy |
| --- | --- | --- |
| **Retrieval Overhead** | `< 150ms` | Vector index caching, optimized HNSW index parameters, parallel chunk retrieval. |
| **Token Streaming TTFT** | `< 400ms` | Stream retrieved context directly into generator pipeline without blocking operations. |
| **Context Overflow** | Automatic Clipping | Dynamic context budgeting based on target model window (e.g., max 4k tokens allocated to RAG context for 8k models). |
| **Observability** | OpenTelemetry | Standard OpenInference spans wrapped around `/v1/retrieval` and LLM completion execution. |

---

## 7. Next Steps & Implementation Roadmap

1. [ ] Implement base FastAPI application structure with `/v1/models` and `/v1/chat/completions` route handlers.
2. [ ] Refactor existing `pocket-advisor.py` CLI module into a cleanly imported Python package (`pocket_advisor.core`).
3. [ ] Integrate `LiteLLM` router for unified upstream LLM streaming dispatch.
4. [ ] Build SSE streaming response generator with citation appending.
5. [ ] Containerize application via Docker and expose standard health endpoints (`/healthz`).
