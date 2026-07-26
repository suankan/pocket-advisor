"""HTTP client for the external Inference Server.

Embedding, reranking, and generation are HTTP services behind separate
configurable endpoints (``models.embedding_endpoint``,
``models.reranker_endpoint``, ``models.summarisation_endpoint``). This
module is the engine's single inference surface — a synchronous facade;
the only concurrency lives in the embedding dispatcher's bounded pool
(``modules/embedding/dispatch.py``).

Defaults point to a local oMLX instance; users may override any endpoint
to use a remote or paid API. The engine never loads a model and never
auto-spawns a server: if an endpoint is down, the caller gets one clear
actionable error.
"""
import math
import threading

import numpy as np

from modules.logs import get_log

INFERENCE_MAX_IN_FLIGHT = 8
CONNECT_TIMEOUT_SEC = 5.0
EMBED_TIMEOUT_SEC = 300.0
RERANK_TIMEOUT_SEC = 300.0
GENERATE_TIMEOUT_SEC = 1800.0
CHARS_PER_TOKEN_ESTIMATE = 3


class InferenceError(RuntimeError):
    """The inference server failed or rejected one request."""


class InferenceUnavailable(InferenceError):
    """The inference endpoint cannot be reached."""


def estimate_tokens(text: str) -> int:
    """Conservative (over-)estimate of model tokens for budgeting."""
    return max(1, math.ceil(len(text) / CHARS_PER_TOKEN_ESTIMATE))


def truncate_by_estimate(text: str, max_tokens: int) -> str:
    """Deterministic character truncation honoring the token estimate."""
    if max_tokens <= 0:
        return ""
    limit = max_tokens * CHARS_PER_TOKEN_ESTIMATE
    if len(text) <= limit:
        return text
    cut = text.rfind(" ", int(limit * 0.8), limit)
    return text[:cut if cut > 0 else limit]


class InferenceClient:
    """One synchronous client over the three inference services.

    Thread-safe: the underlying httpx client supports concurrent requests
    and the readiness cache is lock-guarded.
    """

    def __init__(self, config):
        self.embed_endpoint = config.embedding_endpoint.rstrip("/")
        self.rerank_endpoint = config.reranker_endpoint.rstrip("/")
        self.generate_endpoint = config.summarisation_endpoint.rstrip("/")
        self.embed_dim: int = getattr(config, "embed_dim", 0) or 0
        self.last_prompt_tokens = 0
        self._ready: bool = False
        self._lock = threading.Lock()
        import httpx
        self._http = httpx.Client(
            timeout=httpx.Timeout(EMBED_TIMEOUT_SEC,
                                  connect=CONNECT_TIMEOUT_SEC))

    # -- readiness ---------------------------------------------------------

    def _probe(self, url: str) -> str | None:
        """None when reachable, else an actionable error message."""
        try:
            self._http.get(url, timeout=CONNECT_TIMEOUT_SEC)
            return None
        except Exception as exc:
            return (f"inference endpoint unreachable at {url} —"
                    f" ({type(exc).__name__}: {exc})")

    def check_ready(self) -> None:
        """Hard fail-fast readiness gate (design decision 6)."""
        with self._lock:
            if self._ready:
                return
        for url in (self.embed_endpoint, self.rerank_endpoint,
                    self.generate_endpoint):
            error = self._probe(url)
            if error is not None:
                raise SystemExit(f"inference: {error}")
        with self._lock:
            self._ready = True

    # -- services ----------------------------------------------------------

    def embed(self, texts: list[str]) -> tuple[list[np.ndarray], int]:
        """Embed passages/queries. Returns (vectors, usage prompt
        tokens)."""
        if not texts:
            return [], 0
        data = self._post(
            self.embed_endpoint,
            {"model": "embedding", "input": texts}, EMBED_TIMEOUT_SEC)
        rows = sorted(data.get("data", []),
                      key=lambda item: int(item.get("index", 0)))
        if len(rows) != len(texts):
            raise InferenceError(
                f"embeddings returned {len(rows)} vectors for"
                f" {len(texts)} inputs")
        vectors = []
        for row in rows:
            vector = np.asarray(row["embedding"], dtype=np.float32)
            if self.embed_dim and vector.shape != (self.embed_dim,):
                raise InferenceError(
                    f"embedding dim {vector.shape} != expected"
                    f" ({self.embed_dim},) — model may have changed")
            if not self.embed_dim:
                self.embed_dim = vector.shape[0]
            vectors.append(vector)
        usage = data.get("usage") or {}
        return vectors, int(usage.get("prompt_tokens", 0))

    def embed_one(self, text: str, is_query: bool = False) -> np.ndarray:
        _ = is_query
        return self.embed([text])[0][0]

    def rerank(self, question: str, text_by_id: dict) -> list:
        """Listwise rerank: ids of ``text_by_id`` ordered most relevant
        first. Ids missing from the response keep their input order."""
        ids = list(text_by_id.keys())
        if not ids:
            return []
        data = self._post(
            self.rerank_endpoint,
            {"model": "reranker",
             "query": question,
             "documents": [text_by_id[key] for key in ids]},
            RERANK_TIMEOUT_SEC)
        results = sorted(
            data.get("results", []),
            key=lambda item: -float(item.get("relevance_score", 0.0)))
        ranked = [ids[int(item["index"])] for item in results
                  if 0 <= int(item.get("index", -1)) < len(ids)]
        return ranked + [key for key in ids if key not in set(ranked)]

    def generate(self, system: str, user: str, *, max_tokens: int) -> str:
        """One greedy, bounded chat completion."""
        data = self._post(
            self.generate_endpoint,
            {
                "model": "summariser",
                "messages": [
                    {"role": "system", "content": system},
                    {"role": "user", "content": user},
                ],
                "max_tokens": max_tokens,
                "temperature": 0.0,
            },
            GENERATE_TIMEOUT_SEC)
        choices = data.get("choices") or []
        if not choices:
            raise InferenceError("chat completion returned no choices")
        usage = data.get("usage") or {}
        self.last_prompt_tokens = int(usage.get("prompt_tokens", 0))
        return str(choices[0].get("message", {}).get("content") or "")

    # -- transport ---------------------------------------------------------

    def _post(self, url: str, payload: dict, timeout: float) -> dict:
        import httpx
        try:
            response = self._http.post(url, json=payload, timeout=timeout)
        except (httpx.TransportError, OSError) as exc:
            # The failure that motivated structured logging: previously one
            # summary string reached the operator with no request context,
            # no connection detail, and no stack.
            get_log().debug(
                "inference request failed", endpoint=url,
                failure=f"{type(exc).__name__}: {exc}")
            raise InferenceUnavailable(
                f"inference endpoint unreachable at {url} —"
                f" ({type(exc).__name__}: {exc})") from exc
        if response.status_code >= 400:
            raise InferenceError(
                f"{url}: HTTP {response.status_code}:"
                f" {response.text[:300]}")
        return response.json()
