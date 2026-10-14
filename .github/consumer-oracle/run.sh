#!/usr/bin/env bash
#
# Cross-repo consumer oracle: run PASTURE's built-store reader suite against THIS
# Provenance checkout.
#
# WHY THIS EXISTS
#
# Provenance v0.6.1 shipped a store-layer safety hole: the schema-currency fast
# path (internal/sqlite/activation_currency.go) decided "current" from structural
# facts alone and therefore stopped running the convergence checks activation
# itself runs (verifyIntegrity, then replayProjections). A structurally-current
# store carrying projection damage was then OPENED SUCCESSFULLY instead of being
# refused: an out-of-range phase or an owner that disagreed with the folded
# journal was served, not rejected. Provenance's own CI was green throughout,
# because the regression test that proves the refusal lived in the CONSUMER.
#
# Pasture's cmd/pasture/hook_lifecycle_reader_production_test.go owns that test:
#
#   TestReaderIntegrityRowsAreHandlerOnlyBecauseTheStoreRefusesTheDamage
#
# Its contract, in its own words: "the pinned Provenance open-time replay
# convergence check refuses the damage (an out-of-range phase, an owner
# projection that disagrees with the folded journal) before any gate read runs".
# It seeds a converged store through the production writer, damages one
# projection out of band, reopens through the production borrowed open
# (provenance.OpenBorrowedSQLiteContext, reached via Pasture's
# tasks.OpenTaskTracker), and requires the reopen to FAIL. It fails on v0.6.1
# with "An error is expected but got nil" and passes on v0.6.2.
#
# This script closes that verification gap by running that exact consumer test,
# unchanged, against the Provenance revision under test. It is a real
# cross-repo check, not a re-implementation: the fixture recipe, the assertions,
# and the production open path are all Pasture's.
#
# THE ONE ADAPTATION, AND WHY IT IS HERE
#
# Pasture's pinned revision cannot compile against Provenance >= v0.6.0 without
# one change: provenance v0.6.0 retired the two-dash identity separator
# (<namespace>--<uuid>) for <namespace>/<uuid>, and Pasture's
# internal/acceptance package constructs a retired-format actor id at package
# init, which panics the whole cmd/pasture test binary before any test runs.
# Pasture's own pending identity-format cutover branch
# (pasture-42--feat--identity-literal-sweep, local commit a1cc599) makes exactly
# this one-line change, among many others.
#
# So this script rewrites that single fixture literal after checkout -- and
# asserts that it did, so the adaptation can never silently stop applying. When
# the pin is moved to a cutover revision the rewrite becomes a no-op (the
# already-adapted branch below), and the step can then be deleted.
#
# WHAT THIS GATE CANNOT CATCH
#
#   - It runs one consumer test, not the consumer's hook suite, and no harness
#     transport (Claude Code / Codex / OpenCode). Transport, exit-code, and
#     fault-record behavior stay Pasture's own CI's responsibility.
#   - It pins one Pasture revision, so it does not track consumer drift; it
#     catches PROVENANCE store-layer regressions, not consumer regressions.
#   - It exercises the borrowed open path only (that is the path
#     tasks.OpenTaskTracker uses). The file-backed Open path's read-only
#     preflight is covered by Provenance's own suite.
#   - It does not run under -race; Pasture's own CI owns this suite's race pass.
#
# USAGE
#
#   bash .github/consumer-oracle/run.sh
#
# Overridable for local reproduction of the v0.6.1 / v0.6.2 evidence:
#   PROVENANCE_DIR   Provenance checkout to test (default: this script's repo).
#   PASTURE_SHA      Consumer revision to pin (default below).
#   WORK_DIR         Reuse a directory instead of a fresh temp dir (not removed).
#
set -euo pipefail

PASTURE_REPO="${PASTURE_REPO:-https://github.com/dayvidpham/pasture.git}"
# The consumer revision whose go.mod pins provenance v0.5.2 and which carries
# TestReaderIntegrityRowsAreHandlerOnlyBecauseTheStoreRefusesTheDamage.
# Bump deliberately; this pin is part of the gate, not a floating dependency.
PASTURE_SHA="${PASTURE_SHA:-cecdb598200099e48cee2b3dca6c2e212f31f254}"
ORACLE_TEST="${ORACLE_TEST:-TestReaderIntegrityRowsAreHandlerOnlyBecauseTheStoreRefusesTheDamage}"
PROVENANCE_DIR="${PROVENANCE_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"

if [ -n "${WORK_DIR:-}" ]; then
  CREATED_WORK_DIR=0
else
  WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/provenance-consumer-oracle.XXXXXX")"
  CREATED_WORK_DIR=1
fi
cleanup() {
  if [ "$CREATED_WORK_DIR" = 1 ]; then
    rm -rf -- "$WORK_DIR"
  else
    echo >&2 "consumer-oracle: work dir kept at $WORK_DIR"
  fi
}
trap cleanup EXIT

PASTURE_DIR="$WORK_DIR/pasture"
PROVENANCE_REV="$(git -C "$PROVENANCE_DIR" rev-parse HEAD)"
echo "consumer-oracle: provenance $PROVENANCE_DIR @ $PROVENANCE_REV"
echo "consumer-oracle: pasture     $PASTURE_REPO @ $PASTURE_SHA"

git init -q "$PASTURE_DIR"
git -C "$PASTURE_DIR" remote add origin "$PASTURE_REPO"
# Fetch the exact revision; a branch clone would drift and silently retire the
# gate. Depth 1 keeps the fetch to a couple of seconds.
git -C "$PASTURE_DIR" fetch --depth 1 -q origin "$PASTURE_SHA"
git -C "$PASTURE_DIR" checkout -q FETCH_HEAD

ACTUAL_SHA="$(git -C "$PASTURE_DIR" rev-parse HEAD)"
if [ "$ACTUAL_SHA" != "$PASTURE_SHA" ]; then
  echo >&2 "consumer-oracle: fetched $ACTUAL_SHA but pinned $PASTURE_SHA."
  echo >&2 "What went wrong: the consumer revision the gate pins was not the one checked out."
  echo >&2 "Why: a moved, rewritten, or mistyped pin."
  echo >&2 "Impact: the gate would run a different consumer oracle than the one it documents."
  echo >&2 "Fix: re-pin PASTURE_SHA in $0 to a revision you have verified, and re-run."
  exit 1
fi

# The single identity-format adaptation described in the header. It is applied
# only when the retired literal is still present, and its presence is asserted
# either way, so this step cannot become a silent no-op.
MUTATION_FILE="$PASTURE_DIR/internal/acceptance/mutation.go"
RETIRED_LITERAL='acceptance-mutant--00000000-0000-7000-8000-000000000001'
SLASH_LITERAL='acceptance-mutant/00000000-0000-7000-8000-000000000001'

if grep -qF "$SLASH_LITERAL" "$MUTATION_FILE"; then
  echo "consumer-oracle: pinned consumer already uses the slash identity format; adaptation not needed."
elif grep -qF "$RETIRED_LITERAL" "$MUTATION_FILE"; then
  sed -i "s|$RETIRED_LITERAL|$SLASH_LITERAL|" "$MUTATION_FILE"
  echo "consumer-oracle: adapted the one retired-format fixture literal in internal/acceptance/mutation.go."
else
  echo >&2 "consumer-oracle: neither the retired nor the slash acceptance-actor literal is in"
  echo >&2 "                 $MUTATION_FILE."
  echo >&2 "What went wrong: the pinned consumer revision no longer contains the literal this"
  echo >&2 "                 script adapts, so the whole cmd/pasture test binary will panic at"
  echo >&2 "                 init against Provenance >= v0.6.0."
  echo >&2 "Why: the consumer's identity format changed under the pin."
  echo >&2 "Impact: this gate can no longer run the consumer oracle at all."
  echo >&2 "Fix: re-pin PASTURE_SHA to a consumer revision that is compatible with this"
  echo >&2 "     Provenance (one whose internal/acceptance uses '<namespace>/<uuid>'), then"
  echo >&2 "     delete this adaptation step; see the header for the unblock condition."
  exit 1
fi

cd "$PASTURE_DIR"
# Build the consumer against the revision under test. The consumer's own pinned
# provenance is replaced, never edited.
go mod edit -replace "github.com/dayvidpham/provenance=$PROVENANCE_DIR"

echo "consumer-oracle: go test ./cmd/pasture -run ^${ORACLE_TEST}\$"
go test ./cmd/pasture -run "^${ORACLE_TEST}$" -count=1 -v -timeout 10m
