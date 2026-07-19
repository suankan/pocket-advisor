"""HTTP client for the external oMLX Inference Server.

Per `docs/features/embedding-design-v2.md`, embedding, summarization, and
reranking are HTTP services behind one localhost OpenAI-compatible endpoint
(`models.inference_endpoint`). This module is the engine's single inference
surface — a synchronous facade; the only concurrency lives in the embedding
dispatcher's bounded pool (`modules/embedding/dispatch.py`).

Corpus text goes only to the loopback endpoint; non-local endpoints are
refused outright (`docs/design.md` privacy rule). The engine never loads a
model and never auto-spawns the server: if it is down, the caller gets one
clear actionable error.
"""
import ipaddress
import math
import threading
from urllib.parse import urlparse

import numpy as np

# oMLX's continuous-batching default (max_concurrent_requests); the
# dispatcher never keeps more requests in flight than the server batches.
INFERENCE_MAX_IN_FLIGHT = 8
CONNECT_TIMEOUT_SEC = 5.0
EMBED_TIMEOUT_SEC = 300.0
RERANK_TIMEOUT_SEC = 300.0
GENERATE_TIMEOUT_SEC = 1800.0
# Deterministic conservative pre-call token estimate (design decision 12):
# no local tokenizer exists, so budgeting overestimates tokens and only ever
# segments earlier than strictly necessary. Exact counts for telemetry come
# from the service's usage fields after each call.
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


def require_loopback(endpoint: str) -> None:
    """Refuse any non-loopback inference endpoint: case text never leaves
    the machine, and this is the mechanical enforcement of that rule."""
    host = urlparse(endpoint).hostname
    if host == "localhost":
        return
    try:
        address = ipaddress.ip_address(host or "")
    except ValueError:
        raise SystemExit(
            f"models.inference_endpoint must be loopback-only, got"
            f" {endpoint!r} — case text never leaves the machine")
    if not address.is_loopback:
        raise SystemExit(
            f"models.inference_endpoint must be loopback-only, got"
            f" {endpoint!r} — case text never leaves the machine")


class InferenceClient:
    """One synchronous client over the three oMLX services.

    Thread-safe: the underlying httpx client supports concurrent requests
    and the readiness cache is lock-guarded.
    """

    def __init__(self, config):
        require_loopback(config.inference_endpoint)
        self.endpoint = config.inference_endpoint.rstrip("/")
        self.embed_model = config.model_embed_text
        self.rerank_model = config.model_rerank
        self.generate_model = config.model_thread_summary
        self.embed_dim = config.embed_dim
        self.last_prompt_tokens = 0
        self._ready_models: set[str] = set()
        self._lock = threading.Lock()
        import httpx
        self._http = httpx.Client(
            base_url=self.endpoint,
            timeout=httpx.Timeout(EMBED_TIMEOUT_SEC,
                                  connect=CONNECT_TIMEOUT_SEC))

    # -- readiness ---------------------------------------------------------

    def ready_error(self, *model_ids: str) -> str | None:
        """None when the endpoint serves every given model id, else one
        actionable error message. Never raises for connectivity."""
        needed = set(model_ids) - self._ready_models
        if not needed:
            return None
        try:
            served = self._served_models()
        except InferenceUnavailable as exc:
            return str(exc)
        missing = needed - served
        if missing:
            return (f"model(s) not served at {self.endpoint}:"
                    f" {', '.join(sorted(missing))} — load them in oMLX"
                    f" (served: {', '.join(sorted(served)) or 'none'})")
        with self._lock:
            self._ready_models |= needed
        return None

    def check_ready(self, *model_ids: str) -> None:
        """Hard fail-fast readiness gate (design decision 6)."""
        error = self.ready_error(*model_ids)
        if error is not None:
            raise SystemExit(f"inference: {error}")

    def _served_models(self) -> set[str]:
        data = self._request("GET", "/models", None, CONNECT_TIMEOUT_SEC)
        return {str(item.get("id")) for item in data.get("data", [])}

    # -- services ----------------------------------------------------------

    def embed(self, texts: list[str]) -> tuple[list[np.ndarray], int]:
        """Embed passages/queries (symmetric — oMLX forwards no
        instruction). Returns (vectors, usage prompt tokens)."""
        if not texts:
            return [], 0
        data = self._request(
            "POST", "/embeddings",
            {"model": self.embed_model, "input": texts},
            EMBED_TIMEOUT_SEC)
        rows = sorted(data.get("data", []),
                      key=lambda item: int(item.get("index", 0)))
        if len(rows) != len(texts):
            raise InferenceError(
                f"embeddings returned {len(rows)} vectors for"
                f" {len(texts)} inputs")
        vectors = []
        for row in rows:
            vector = np.asarray(row["embedding"], dtype=np.float32)
            if vector.shape != (self.embed_dim,):
                raise InferenceError(
                    f"embedding dim {vector.shape} != configured"
                    f" models.embed_dim ({self.embed_dim},) — check"
                    " model_embed_text/embed_dim")
            vectors.append(vector)
        usage = data.get("usage") or {}
        return vectors, int(usage.get("prompt_tokens", 0))

    def embed_one(self, text: str, is_query: bool = False) -> np.ndarray:
        _ = is_query  # symmetric encoder: query == passage scheme
        return self.embed([text])[0][0]

    def rerank(self, question: str, text_by_id: dict) -> list:
        """Listwise rerank: ids of ``text_by_id`` ordered most relevant
        first. Ids missing from the response keep their input order."""
        ids = list(text_by_id.keys())
        if not ids:
            return []
        data = self._request(
            "POST", "/rerank",
            {"model": self.rerank_model, "query": question,
             "documents": [text_by_id[key] for key in ids]},
            RERANK_TIMEOUT_SEC)
        results = sorted(
            data.get("results", []),
            key=lambda item: -float(item.get("relevance_score", 0.0)))
        ranked = [ids[int(item["index"])] for item in results
                  if 0 <= int(item.get("index", -1)) < len(ids)]
        return ranked + [key for key in ids if key not in set(ranked)]

    def generate(self, system: str, user: str, *, max_tokens: int) -> str:
        """One greedy, bounded chat completion with Qwen thinking output
        disabled (verified against oMLX's chat_template_kwargs)."""
        data = self._request(
            "POST", "/chat/completions",
            {
                "model": self.generate_model,
                "messages": [
                    {"role": "system", "content": system},
                    {"role": "user", "content": user},
                ],
                "max_tokens": max_tokens,
                "temperature": 0.0,
                "chat_template_kwargs": {"enable_thinking": False},
            },
            GENERATE_TIMEOUT_SEC)
        choices = data.get("choices") or []
        if not choices:
            raise InferenceError("chat completion returned no choices")
        usage = data.get("usage") or {}
        self.last_prompt_tokens = int(usage.get("prompt_tokens", 0))
        return str(choices[0].get("message", {}).get("content") or "")

    # -- transport ---------------------------------------------------------

    def _request(self, method: str, path: str, payload: dict | None,
                 timeout: float) -> dict:
        import httpx
        try:
            response = self._http.request(
                method, path, json=payload, timeout=timeout)
        except (httpx.TransportError, OSError) as exc:
            raise InferenceUnavailable(
                f"inference endpoint unreachable at {self.endpoint} —"
                f" is oMLX running? ({type(exc).__name__}: {exc})") from exc
        if response.status_code >= 400:
            raise InferenceError(
                f"{path}: HTTP {response.status_code}:"
                f" {response.text[:300]}")
        return response.json()
