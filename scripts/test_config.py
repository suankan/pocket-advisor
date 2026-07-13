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


def test_list_to_set():
    print("list -> set conversion:")
    before = config.DOCUMENT_FOLDERS
    p = write_yaml("privilege:\n  document_folders: [a, b]\n")
    try:
        config.load_yaml_overlay(p)
        check("converted to set", config.DOCUMENT_FOLDERS == {"a", "b"})
    finally:
        config.DOCUMENT_FOLDERS = before
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


def test_derived_path_recomputed():
    print("derived path recomputed on file override:")
    before_file, before_path = config.EMBED_MODEL_FILE, config.EMBED_MODEL_PATH
    p = write_yaml("models:\n  embed_model_file: other-model.gguf\n")
    try:
        config.load_yaml_overlay(p)
        check("EMBED_MODEL_PATH updated",
              config.EMBED_MODEL_PATH == config.MODELS_DIR / "other-model.gguf")
    finally:
        config.EMBED_MODEL_FILE, config.EMBED_MODEL_PATH = before_file, before_path
        p.unlink()


def test_workspace_dir_derives_eval_paths():
    print("workspace.dir derives EVAL_* paths:")
    before = (config.WORKSPACE_DIR, config.EVAL_DIR,
              config.EVAL_GOLDEN_DIR, config.EVAL_RESULTS_DIR)
    p = write_yaml("workspace:\n  dir: workspaces/test-ws\n")
    try:
        config.load_yaml_overlay(p)
        check("WORKSPACE_DIR overridden",
              config.WORKSPACE_DIR == config.PROJECT_ROOT / "workspaces/test-ws")
        check("EVAL_RESULTS_DIR derived",
              config.EVAL_RESULTS_DIR == config.WORKSPACE_DIR / "eval" / "results")
        check("INGESTION_SOURCES under workspace corpora",
              config.INGESTION_SOURCES == config.WORKSPACE_DIR / "corpora")
        check("OUTPUT_DIR under workspace",
              config.OUTPUT_DIR == config.WORKSPACE_DIR / "output")
        check("DB_PATH under workspace output",
              config.DB_PATH == config.WORKSPACE_DIR / "output" / "pocket_advisor.db")
    finally:
        (config.WORKSPACE_DIR, config.EVAL_DIR,
         config.EVAL_GOLDEN_DIR, config.EVAL_RESULTS_DIR) = before
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
    test_list_to_set()
    test_is_privileged_path()
    test_derived_path_recomputed()
    test_workspace_dir_derives_eval_paths()
    test_missing_file_is_a_noop()

    if FAILURES:
        print(f"\n{len(FAILURES)} FAILURE(S): {FAILURES}")
        return 1
    print("\nAll config.py self-tests passed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
