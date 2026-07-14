"""Self-test for rerank_backends.py (MLX-only). No real model load.

    venv/bin/python scripts/test_rerank_backends.py
"""
import sys

import rerank_backends

FAILURES = []


def check(name, cond, detail=""):
    status = "ok" if cond else "FAIL"
    print(f"  [{status}] {name}" + (f" — {detail}" if detail and not cond else ""))
    if not cond:
        FAILURES.append(name)


def test_backend_surface():
    print("backend surface:")
    check("MlxRerankBackend present",
          hasattr(rerank_backends, "MlxRerankBackend"))
    check("name is mlx", rerank_backends.MlxRerankBackend.name == "mlx")
    check("get_backend is callable", callable(rerank_backends.get_backend))


def main():
    test_backend_surface()
    if FAILURES:
        print(f"\n{len(FAILURES)} FAILURE(S): {FAILURES}")
        return 1
    print("\nAll rerank_backends.py self-tests passed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
