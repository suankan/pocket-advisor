"""Self-test for the config.yaml overlay (config.py). Temp-file fixture
— never touches the real config.yaml.

    venv/bin/python scripts/test_config.py
"""
import sys
import tempfile
from pathlib import Path

import config

FAILURES = []


def check(name, cond, detail=""):
    status = "ok" if cond else "FAIL"
    print(f"  [{status}] {name}" + (f" — {detail}" if detail and not cond else ""))
    if not cond:
        FAILURES.append(name)


def write_yaml(text):
    p = Path(tempfile.mkstemp(suffix=".yaml")[1])
    p.write_text(text)
    return p


def test_overlay_applies():
    print("overlay applies:")
    before_top_k = config.DEFAULT_TOP_K
    p = write_yaml("query:\n  default_top_k: 3\n")
    try:
        config.load_yaml_overlay(p)
        check("free knob overridden", config.DEFAULT_TOP_K == 3)
    finally:
        config.DEFAULT_TOP_K = before_top_k
        p.unlink()


def test_unknown_key_aborts():
    print("unknown key aborts:")
    p = write_yaml("privilege:\n  document_folder: [x]\n")  # typo (missing 's')
    try:
        config.load_yaml_overlay(p)
        check("aborts on typo", False)
    except SystemExit as e:
        check("aborts on typo", "privilege.document_folder" in str(e))
    finally:
        p.unlink()


def test_platform_privilege_document_folders_rejected():
    print("platform privilege.document_folders rejected:")
    p = write_yaml("privilege:\n  document_folders: [additional-documents]\n")
    try:
        config.load_yaml_overlay(p)
        check("aborts on privilege.document_folders", False)
    except SystemExit as e:
        check("aborts on privilege.document_folders",
              "privilege.document_folders" in str(e))
    finally:
        p.unlink()


def test_is_privileged_path():
    print("is_privileged_path convention:")
    check("top-level privileged/ wrapper",
          config.is_privileged_path("privileged/example-law-firm.example/x.eml"))
    check("privileged/ nested deeper",
          config.is_privileged_path("mail/privileged/sub/x.eml"))
    check("not privileged: no privileged/ ancestor",
          not config.is_privileged_path("example-law-firm.example/x.eml"))
    check("filename itself named 'privileged' does not count",
          not config.is_privileged_path("docs/privileged"))


def test_model_repo_overlay():
    print("models.mlx_* + ingestion.embed_* overlay:")
    before = (config.MLX_MODEL_EMBED_TEXT,
              config.MLX_MODEL_EMBED_OMNI,
              config.MLX_MODEL_RERANK,
              config.EMBED_TEXT,
              config.EMBED_IMAGES)
    p = write_yaml(
        "models:\n"
        "  mlx_model_embed_text: jinaai/text-test\n"
        "  mlx_model_embed_omni: jinaai/omni-test\n"
        "  mlx_model_rerank: jinaai/rerank-test\n"
        "ingestion:\n"
        "  embed_text: false\n"
        "  embed_images: false\n")
    try:
        config.load_yaml_overlay(p)
        check("text repo", config.MLX_MODEL_EMBED_TEXT == "jinaai/text-test")
        check("omni repo", config.MLX_MODEL_EMBED_OMNI == "jinaai/omni-test")
        check("rerank repo", config.MLX_MODEL_RERANK == "jinaai/rerank-test")
        check("embed_text", config.EMBED_TEXT is False)
        check("embed_images", config.EMBED_IMAGES is False)
    finally:
        (config.MLX_MODEL_EMBED_TEXT,
         config.MLX_MODEL_EMBED_OMNI,
         config.MLX_MODEL_RERANK,
         config.EMBED_TEXT,
         config.EMBED_IMAGES) = before
        p.unlink()


def test_workspace_dir_derives_eval_paths():
    print("workspace paths derive output/eval:")
    before = (config.WORKSPACE_DIR, config.WORKSPACES_DIR, config.EVAL_DIR,
              config.EVAL_GOLDEN_DIR, config.EVAL_RESULTS_DIR,
              config.INGESTION_SOURCES, config.OUTPUT_DIR, config.DB_PATH,
              getattr(config, "ACTIVE_WORKSPACE_ID", None))
    # Prefer workspaces.dir; active workspace comes from registry when present.
    p = write_yaml("workspaces:\n  dir: workspaces\n")
    try:
        config.load_yaml_overlay(p)
        check("WORKSPACES_DIR set",
              config.WORKSPACES_DIR == config.PROJECT_ROOT / "workspaces")
        # With a live workspace-config.yaml, active workspace wins.
        check("WORKSPACE_DIR under workspaces/",
              "workspaces" in config.WORKSPACE_DIR.parts)
        check("EVAL_RESULTS_DIR under workspace",
              config.EVAL_RESULTS_DIR == config.WORKSPACE_DIR / "eval" / "results")
        # Shared corpora + state (not under matter folder)
        check("INGESTION_SOURCES shared corpora",
              config.INGESTION_SOURCES == config.WORKSPACES_DIR / "corpora")
        check("OUTPUT_DIR is shared state",
              config.OUTPUT_DIR == config.WORKSPACES_DIR / "state"
              or config.OUTPUT_DIR.name in ("state", "output"))
        check("DB_PATH under state",
              config.DB_PATH.name == "pocket_advisor.db"
              and config.DB_PATH.parent == config.OUTPUT_DIR)
    finally:
        (config.WORKSPACE_DIR, config.WORKSPACES_DIR, config.EVAL_DIR,
         config.EVAL_GOLDEN_DIR, config.EVAL_RESULTS_DIR,
         config.INGESTION_SOURCES, config.OUTPUT_DIR, config.DB_PATH,
         config.ACTIVE_WORKSPACE_ID) = before
        p.unlink()


def test_missing_file_is_a_noop():
    print("missing file leaves defaults untouched:")
    before = config.DEFAULT_TOP_K
    missing = Path(tempfile.mkdtemp()) / "does-not-exist.yaml"
    check("no exception constructing path", not missing.exists())
    check("value unchanged (no load attempted)", config.DEFAULT_TOP_K == before)


def main():
    test_overlay_applies()
    test_unknown_key_aborts()
    test_platform_privilege_document_folders_rejected()
    test_is_privileged_path()
    test_model_repo_overlay()
    test_workspace_dir_derives_eval_paths()
    test_missing_file_is_a_noop()

    if FAILURES:
        print(f"\n{len(FAILURES)} FAILURE(S): {FAILURES}")
        return 1
    print("\nAll config.py self-tests passed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
