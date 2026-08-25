# Releasing

provenance is a Go library. A release is a `vX.Y.Z` git tag plus a GitHub
Release whose notes are drawn straight from `CHANGELOG.md` — there is no
build/asset pipeline, because module consumers fetch the source at that tag
through the Go module proxy (`go get github.com/dayvidpham/provenance@vX.Y.Z`).

The tag is created **automatically on merge** by `.github/workflows/release.yml`
(tag-on-merge), modeled on pasture's release workflow but adapted for a
library: no build matrix, and the Release body is required to come from a
dated `CHANGELOG.md` section rather than an auto-generated diff.

## The flow (tag-on-merge)

```bash
# 1. Branch off main.
git checkout -b chore/release-vX.Y.Z main

# 2. Bump flake.nix's `version = "X.Y.Z";` and rename CHANGELOG.md's
#    "## Unreleased" heading to "## vX.Y.Z - YYYY-MM-DD", moving that
#    release's entries under it. Do this by hand — provenance has no
#    pasture-release-equivalent bump tool. Commit both files together.
git commit -am "release: prepare vX.Y.Z"

# 3. Push, open a PR, merge to main.
git push -u origin chore/release-vX.Y.Z
gh pr create --base main --fill
#   → merge the PR

# 4. On merge, release.yml fires (it triggers on a push to main that changes
#    flake.nix): it tags vX.Y.Z on the merged commit, extracts the
#    "## vX.Y.Z - ..." section of CHANGELOG.md, and publishes a GitHub
#    Release with that section as the body.
```

## Why the CHANGELOG section is mandatory

The release workflow refuses to publish a Release for a version whose
`CHANGELOG.md` has no matching `## vX.Y.Z` heading — it fails the run with an
actionable error rather than falling back to an empty or auto-generated body.
This mirrors the CHANGELOG discipline already enforced elsewhere in this
repo (see `CHANGELOG.md` and `CONTRIBUTING.md`): every tagged release must
carry real, dated notes, not a diff summary.

## Duplicate-tag refusal

Unlike pasture's release workflow — which silently skips a re-run when
`vX.Y.Z` is already tagged — provenance's workflow **refuses** (fails the
run) if the target tag already exists. A pre-existing tag for the version
about to be released means one of:

- `flake.nix`'s version was bumped to a number that was already released, or
- an earlier run tagged `vX.Y.Z` but failed before publishing the Release
  (e.g. the CHANGELOG section was missing).

Either way this needs a human decision, not a quiet no-op:

- If the earlier run only tagged and never published, delete the errant tag
  (`git push origin :refs/tags/vX.Y.Z`) and re-run via
  `gh workflow run release.yml --ref main`.
- Otherwise, bump `flake.nix`'s version to a number that has not been tagged.

## One-time setup

The tag-on-merge workflow pushes the tag using a **GitHub App token**, so the
repo needs two secrets:

- `RELEASE_APP_ID` — the release App's ID
- `RELEASE_APP_PRIVATE_KEY` — the App's private key (PEM)

The App (`dayvidpham-release-bot`) is already installed on this repo with
**`Contents: write`**. An App token is used instead of the default
`GITHUB_TOKEN` so the tag is created under a real bot identity and survives
future tag-ref protection.

### Optional hardening: a branch-name ruleset

aura-plugins' `pasture` repo additionally runs a GitHub ruleset that blocks
creating branches matching `release/**`, so a release-prep branch can't be
confused with (or collide with) the tag-on-merge flow's own `vX.Y.Z` tags —
use a `chore/*` name for the prep branch instead, as in the flow above. This
repo does not currently define that ruleset; it's optional hardening to
consider if `release/*` branch names start showing up by convention or by
habit.

## Re-running / recovering a release

`workflow_dispatch` lets you fire the workflow manually against `main`:

- If `flake.nix` is already at the new version on `main` but the tag is
  missing (the first run failed before tagging), re-run with
  `gh workflow run release.yml --ref main` — on manual dispatch the workflow
  proceeds whenever the tag is absent.
- If `vX.Y.Z` is already tagged, see **Duplicate-tag refusal** above — the
  workflow will fail with the exact recovery steps rather than skip quietly.

## Troubleshooting

- **Tag push fails `403 ... denied to github-actions[bot]`** — the checkout
  persisted the default `GITHUB_TOKEN` as a git `http.extraheader`, which
  overrides the App token in the push URL. `release.yml`'s `detect` job sets
  `persist-credentials: false` on checkout for exactly this reason; if it's
  been removed, restore it. (If it instead 403s as the *App* identity, the
  App is missing `Contents: write`.)
- **No release fired after merge** — the trigger is a change to `flake.nix`
  on `main`. A merge that didn't change that file (e.g. a workflow-only fix)
  won't trigger; use `workflow_dispatch`.
- **Release workflow fails at "Extract CHANGELOG section"** — `CHANGELOG.md`
  has no `## vX.Y.Z` heading yet, or it's misspelled. Rename the
  `## Unreleased` heading on `main` and re-run via `workflow_dispatch`; the
  tag from the failed run is not pushed, so there's nothing to clean up.
