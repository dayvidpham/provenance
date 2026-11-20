#!/usr/bin/env bash
set -euo pipefail

copy=/tmp/opencode/m1-measure.db
live=/home/minttea/.local/share/pasture/pasture.db
available_kb=$(df -Pk / | awk 'NR==2 {print $4}')
if (( available_kb < 25 * 1024 * 1024 )); then
  echo "M1 measurement needs 25 GiB free on /; available: ${available_kb} KiB" >&2
  exit 1
fi
for file in "$copy" "$copy-wal" "$copy-shm" "$copy-journal"; do
  if [[ -e "$file" ]]; then
    echo "Refusing to overwrite existing file: $file" >&2
    exit 1
  fi
done
cleanup() {
  rm -f -- "$copy" "$copy-wal" "$copy-shm" "$copy-journal"
}
trap cleanup EXIT
sqlite3 -readonly "$live" "VACUUM INTO '$copy'"
nix develop --command env CGO_ENABLED=1 PROVENANCE_M1_COPY="$copy" \
  go test -race -shuffle=on -fullpath -timeout=20m -run '^TestReplayM1Performance$' -v ./internal/sqlite
