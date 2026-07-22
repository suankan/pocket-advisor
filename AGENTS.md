# Pocket Advisor — Agent Instructions

Pocket Advisor is a local RAG engine over personal content.

## Read first

For every platform task, load these files in order:

1. this file;
2. `docs/design.md` — holistic solution architecture, pipeline, artifacts,
   retrieval, transactions, and system invariants;
   - load the relevant feature design from the per-concern folders under
     `docs/` (`ingestion/`, `retrieval/`, `generation/`, `inference/`,
     `storage/`, `benchmarks/`, `platform/`) as needed by the task;
   - read the code as needed;
3. `docs/work-in-progress.md` — check for unfinished work; cross-reference
   with `git status`;
4. `docs/roadmap.md` — ordered future work only;
5. `docs/changelog.md` — shipped history, newest first (optional context).

For case work, additionally load:

1. `workspaces/workspace-config.yaml`;
2. the selected workspace's `WORKSPACE.md`;
3. its applicable domain playbook(s).

Do not answer case questions from platform instructions alone.

## Documentation lifecycle

The three planning records are:

- `docs/roadmap.md` — ordered, unshipped work.
- `docs/work-in-progress.md` — scratch pad for the feature currently being
  implemented. Near-empty when idle.
- `docs/changelog.md` — durable, reverse-chronological history of shipped
  items.

The implementation workflow is:

1. **Design and commit.** Plan the feature, lock down an initial design in a
   doc under the relevant `docs/` concern folder, and commit.
2. **Pick up.** Move the roadmap item into `docs/work-in-progress.md` with any
   active context needed to resume.
3. **Implement.** Build and verify.
4. **Lock down and commit.** Update the feature design doc to reflect the final
   implemented state and commit.
5. **Changelog.** Add a condensed entry to `docs/changelog.md`
   (date, title, commit, summary, verification).
6. **Clean up.** Remove the item from `docs/work-in-progress.md`; remove the
   completed item from `docs/roadmap.md`, renumber remaining items, and repair
   cross-references.
7. **Commit.** One atomic commit with changelog entry, roadmap and
   work-in-progress records cleanup.

## Hard rules

1. **Source of truth corpora collections is read-only.** Never write, rename, or delete anything under a
   collection root (`workspaces/corpora/...` or a registry path). Durable
   identity is `(collection_id, sha256)`, never a path. Only engine-derived
   state under `workspaces/.state/` is regenerable; preserved
   `search-accuracy-tests/` directories are human-authored workspace test data.

## Design references

All design and architecture detail lives in `docs/`. When working on a
concern, load the relevant feature doc from the index in `docs/design.md`. Or create the new one if needed.

## Verification

Use temp fixtures for any integrity/drift test; never modify real corpus files.
Before handing off a change:

```bash
for test_file in modules/tests/test_*.py; do
  uv run python "$test_file"
done
uv run pocket-advisor.py test
git diff --check
git status --short
```

The query-daemon socket test may require permission to bind a temporary local
Unix socket in restricted environments; that is an environment constraint,
not grounds to weaken or skip the test.
