#!/usr/bin/env bash
set -euo pipefail

status=0
while IFS= read -r -d '' file; do
  if git check-ignore -q -- "$file"; then
    printf 'error: source file is ignored by Git: %s\n' "$file" >&2
    status=1
  fi
done < <(find cmd internal -type f -name '*.go' -print0 2>/dev/null)

exit "$status"
