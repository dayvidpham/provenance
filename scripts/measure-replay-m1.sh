#!/usr/bin/env bash
set -euo pipefail

copy=/tmp/opencode/m1-measure.db
live=/home/minttea/.local/share/pasture/pasture.db
available_kb=$(df -Pk "$(dirname "$copy")" | awk 'NR==2 {print $4}')
if (( available_kb < 25 * 1024 * 1024 )); then
  echo "M1 measurement needs 25 GiB free at $(dirname "$copy"); available: ${available_kb} KiB" >&2
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
  for file in "$copy" "$copy-wal" "$copy-shm" "$copy-journal"; do
    if [[ -e "$file" ]]; then
      echo "M1 cleanup failed: $file remains" >&2
      return 1
    fi
  done
  echo "M1 cleanup: copy and sidecars absent"
}
trap cleanup EXIT
copy_start=$(date +%s%N)
sqlite3 -readonly "$live" "VACUUM INTO '$copy'"
copy_end=$(date +%s%N)
awk -v ns="$((copy_end - copy_start))" 'BEGIN {printf "M1 copy_creation_seconds=%.3f\n", ns / 1000000000}'
nix develop --command env CGO_ENABLED=1 PROVENANCE_M1_COPY="$copy" \
  go test -race -shuffle=on -fullpath -timeout=120m -run '^TestReplayM1Performance$' -v ./internal/sqlite
