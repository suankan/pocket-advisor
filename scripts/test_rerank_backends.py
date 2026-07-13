"""Self-test for rerank_backends.py dispatch logic. No real model load
— mirrors test_config.py's style, exercises only get_backend()'s
validation and class selection.

    venv/bin/python scripts/test_rerank_backends.py
"""
import sys

import config
import rerank_backends

FAILURES = []


def check(name, cond, detail=""):
    status = "ok" if cond else "FAIL"
    print(f"  [{status}] {name}" + (f" — {detail}" if detail and not cond else ""))
    if not cond:
        FAILURES.append(name)


def test_dispatch_selects_correct_class():
    print("get_backend dispatch:")
    before = config.RERANK_BACKEND
    try:
        config.RERANK_BACKEND = "llama_cpp"
        cls = {"llama_cpp": rerank_backends.LlamaCppRerankBackend,
               "jina_mlx": rerank_backends.JinaMlxRerankBackend}[config.RERANK_BACKEND]
        check("llama_cpp maps to LlamaCppRerankBackend",
              cls is rerank_backends.LlamaCppRerankBackend)

        config.RERANK_BACKEND = "jina_mlx"
        cls = {"llama_cpp": rerank_backends.LlamaCppRerankBackend,
               "jina_mlx": rerank_backends.JinaMlxRerankBackend}[config.RERANK_BACKEND]
        check("jina_mlx maps to JinaMlxRerankBackend",
              cls is rerank_backends.JinaMlxRerankBackend)
    finally:
        config.RERANK_BACKEND = before


def test_invalid_backend_raises():
    print("invalid RERANK_BACKEND raises:")
    before = config.RERANK_BACKEND
    try:
        config.RERANK_BACKEND = "not-a-real-backend"
        try:
            rerank_backends.get_backend()
            check("raises SystemExit", False)
        except SystemExit as e:
            check("raises SystemExit", "not-a-real-backend" in str(e))
    finally:
        config.RERANK_BACKEND = before


def test_backend_names_match_valid_tuple():
    print("backend .name attributes match _VALID:")
    check("llama_cpp name", rerank_backends.LlamaCppRerankBackend.name == "llama_cpp")
    check("jina_mlx name", rerank_backends.JinaMlxRerankBackend.name == "jina_mlx")
    check("_VALID covers both",
          set(rerank_backends._VALID) == {"llama_cpp", "jina_mlx"})


def main():
    test_dispatch_selects_correct_class()
    test_invalid_backend_raises()
    test_backend_names_match_valid_tuple()

    if FAILURES:
        print(f"\n{len(FAILURES)} FAILURE(S): {FAILURES}")
        return 1
    print("\nAll rerank_backends.py self-tests passed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
