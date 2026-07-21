# uv Migration

Replace `venv` + `requirements.txt` + `pip` with `uv` for all Python runtime
concerns: dependency resolution, lockfile, virtual environment, and invocation.

## Current state

| Concern | Mechanism | Location |
|---|---|---|
| Dependency spec | `requirements.txt` (8 runtime deps, no pins) | `requirements.txt` |
| Env creation | `python3.14 -m venv venv` | manual, documented in `docs/rag-dev-howto.md` |
| Install | `venv/bin/pip install -r requirements.txt` | manual |
| Invocation | `./pocket-advisor.py` (auto-re-execs into `venv/bin/python`) | `pocket-advisor.py:12-20` |
| Test runner | `venv/bin/python "$test_file"` | `AGENTS.md`, `docs/rag-dev-howto.md`, feature docs |
| Lockfile | none | — |
| Python version | declared only in docs and `venv/pyvenv.cfg` | `docs/design.md:239` |
| .gitignore | `venv/` | `.gitignore:10` |

The live venv contains ~160 stale packages from pre-MLX iterations. No
`pyproject.toml`, `setup.py`, `Pipfile`, or lockfile exists.

## Target state

| Concern | Mechanism | Location |
|---|---|---|
| Dependency spec | `pyproject.toml` `[project.dependencies]` | `pyproject.toml` |
| Env + install | `uv sync` (creates `.venv`, installs from lockfile) | automatic |
| Lockfile | `uv.lock` (committed, reproducible) | `uv.lock` |
| Invocation | `uv run ./pocket-advisor.py` | shell |
| Test runner | `uv run python "$test_file"` | docs, agents |
| Python version | `requires-python = ">=3.14"` in `pyproject.toml` | `pyproject.toml` |
| .gitignore | `.venv/` (replaces `venv/`) | `.gitignore` |

`uv run` transparently ensures the environment is synced before executing.
No `os.execv` re-exec is needed.

## Design decisions

### D1. pyproject.toml, not requirements.txt

`pyproject.toml` is the single source of truth for project metadata, Python
version constraint, and runtime dependencies. `requirements.txt` is deleted.
`uv.lock` is committed for reproducible installs across machines.

### D2. `uv run` invocation, no auto-re-exec

Remove the `os.execv` venv-detection logic from `pocket-advisor.py`. The
script becomes a thin entrypoint that imports and calls `modules.cli.main`.
Users invoke via `uv run ./pocket-advisor.py [args]`.

Rationale: `uv run` already ensures the correct Python and dependencies are
available. Duplicating that logic inside the script adds complexity with no
benefit.

### D3. `.venv` (uv default), not `venv`

uv creates `.venv` by default. Adopt the default rather than overriding with
`--python-version` or `UV_PROJECT_ENVIRONMENT`. The `.gitignore` entry
changes from `venv/` to `.venv/`.

### D4. Delete the old `venv/` directory

The old venv is stale (~160 packages from prior iterations). It is not
migrated; the operator deletes it manually after the uv environment is
verified working.

### D5. No `[project.scripts]` entry point

Pocket Advisor is not a distributable package. `pocket-advisor.py` remains
the sole executable. No `pip install -e .` or entry-point machinery.

## File changes

### `pyproject.toml` (new)

```toml
[project]
name = "pocket-advisor"
version = "0.1.0"
requires-python = ">=3.14"
dependencies = [
    "beautifulsoup4>=4.12",
    "lxml>=5.0",
    "numpy>=1.26",
    "python-dateutil>=2.9",
    "pyyaml>=6.0",
    "unidecode>=1.3",
    "httpx>=0.27",
]
```

### `requirements.txt` (delete)

Superseded by `pyproject.toml`.

### `pocket-advisor.py` (simplify)

Remove lines 7-20 (`os`, `Path` imports, `ROOT`/`VENV`/`VENV_PYTHON`
constants, `os.execv` block). Keep the shebang, `main()`, and
`if __name__` block.

### `.gitignore`

Replace `venv/` with `.venv/`. Add `uv.lock` if the team decides to keep
the lockfile committed (it should be committed).

### `AGENTS.md`

Replace verification block:

```bash
for test_file in modules/tests/test_*.py; do
  uv run python "$test_file"
done
uv run ./pocket-advisor.py test
```

### `docs/rag-dev-howto.md`

- **Setup section**: replace `python3.14 -m venv venv` + `pip install` with:
  ```bash
  uv sync
  ```
- **All command examples**: prefix with `uv run`:
  ```bash
  uv run ./pocket-advisor.py --workspace <id> <command>
  ```
- **Verification section**: replace `venv/bin/python` with `uv run python`.

### `docs/design.md`

No change to "Runtime is Python 3.14." — that remains true. The
`requires-python` constraint in `pyproject.toml` is now the enforced
declaration.

### Feature docs referencing `venv/bin/python`

- `docs/features/embedding-design-v2.md:322` — replace `venv/bin/python` with
  `uv run python`.
- `docs/features/summary-generation-concurrency.md:151-152` — same.

### `docs/changelog.md`

No changes (historical references to `venv` and `pip` are accurate for their
time).

## Implementation order

1. Create `pyproject.toml` with the dependency list above.
2. Run `uv lock` to generate `uv.lock`.
3. Run `uv sync` to create `.venv` and install.
4. Run the full verification suite to confirm nothing broke.
5. Simplify `pocket-advisor.py` (remove re-exec logic).
6. Re-run verification.
7. Delete `requirements.txt`.
8. Update `AGENTS.md` verification commands.
9. Update `docs/rag-dev-howto.md` setup and command examples.
10. Update feature docs (`embedding-design-v2.md`,
    `summary-generation-concurrency.md`).
11. Update `.gitignore` (`venv/` → `.venv/`).
12. Add feature doc entry to `docs/design.md` feature index.
13. Operator manually deletes old `venv/` directory.
14. Final verification: full suite + `./pocket-advisor.py test`.

## Verification

After implementation, confirm:

```bash
uv sync
for test_file in modules/tests/test_*.py; do
  uv run python "$test_file"
done
uv run ./pocket-advisor.py test
git diff --check
git status --short
```

All 14 native tests and the CLI `test` command must pass. No `venv/`
directory should remain. `uv.lock` should be committed.
