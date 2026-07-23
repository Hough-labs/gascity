# Fork patch management (the `integration` branch)

This fork carries a small set of fork-local changes on top of a **pinned
upstream gascity release** — never on top of `main`. The mechanism is a
`git format-patch` / `git am` patch stack, mirrored from the same system used
in `gastown` and `beads`.

## The model

- **`integration`** — the branch that carries the fork. It is
  `BASELINE` + the commits in `patches/`, in order.
- **`BASELINE`** — a pinned upstream commit (a release SHA), defined in the
  `Makefile`. The fork's patches are always replayed onto exactly this commit,
  so builds are reproducible. It is deliberately an upstream **release tag**
  (currently `v1.4.0`), not `main` — and it is pinned by SHA rather than by tag
  name, so a moved or re-cut tag cannot silently change what the fork builds on.
- **`patches/`** — a **derived artifact**: the `git format-patch BASELINE..HEAD`
  export of the fork's divergence, excluding `patches/` itself. It is committed
  so the divergence is reviewable and replayable, but it is regenerated, never
  hand-edited.
- **`BASELINE_VERSION`** — the version string a fork build reports. On an exact
  release tag the tag wins; otherwise `gc version` prints `BASELINE_VERSION`
  (e.g. `1.4.0-integration`) so a fork binary is never confused with `dev`/main.

## Everyday workflow — adding or changing a fork patch

```bash
git switch integration
# ... make your change ...
git commit -m "fix(scope): what and why"

make patches                       # regenerate patches/ from the divergence
git add patches/
git commit --amend --no-edit       # fold the export into the same commit
```

`make check-patches` (run automatically by the pre-push hook on this branch)
fails the push if `patches/` does not match the current `BASELINE..HEAD`
divergence, so the export can never silently drift from the commits.

## Upgrading the baseline (moving to a newer upstream release)

1. Pick the new upstream release commit (`git fetch upstream --tags`, then
   resolve the tag to a SHA with `git rev-parse v<X.Y.Z>^{commit}`) and update
   **both** `BASELINE` and `BASELINE_VERSION` in the `Makefile`, plus the
   standalone default in `scripts/upgrade-integration.sh` — all three must
   agree.
2. **Fold that bump into the patch that introduces it** — the patch-management
   patch (`0001`) is what creates the `Makefile` and the upgrade script, so a
   bump committed on top would just be a patch rewriting a line an earlier
   patch had written, and the stack would grow by one such patch per upgrade
   forever. Amend it into `0001` instead, so the stack stays at the fork's real
   changes:

   ```bash
   git commit --fixup <sha-of-patch-0001-commit>
   GIT_SEQUENCE_EDITOR=true git rebase --autosquash --onto <old-baseline> <old-baseline>
   make patches && git add patches/ && git commit --amend --no-edit
   ```

   This is the one exception to the everyday "commit it like any other fork
   patch" flow.
3. Run the guided upgrade:

   ```bash
   make upgrade
   ```

   It fetches `upstream`, copies `patches/` to a tmpdir (the reset would wipe
   it), resets `integration` to `BASELINE`, replays the patches with
   `git am --3way`, regenerates `patches/`, and offers to build + install.
4. On a conflict, `git am` stops with instructions: resolve, `git add`,
   `git am --continue` — or `git am --abort` to bail.

## Building from source

```bash
make build            # -> bin/gc, version stamped from BASELINE_VERSION
gc version            # confirm: version = 1.4.0-integration, commit = <sha>
make install          # install to $(go env GOPATH)/bin + ~/.local/bin symlink
```

## Why patches/ is excluded from its own export

`git format-patch BASELINE..HEAD -- . ':!patches/'` excludes the `patches/`
pathspec. Without the exclusion the export would recursively include previous
exports, ballooning every patch. A commit that touches only `patches/` is
therefore omitted from the export entirely (history simplification treats it as
empty), which is why the everyday flow folds `patches/` back into the source
commit with `--amend`.
