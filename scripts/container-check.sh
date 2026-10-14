#!/bin/sh
# Formatting and static checks inside the pinned container.
set -eu
. "$(dirname "$0")/lib.sh"
docker_run sh -c '
  unformatted=$(gofmt -l . | grep -v "^.cache/" || true)
  if [ -n "$unformatted" ]; then
    echo "gofmt found unformatted files:" >&2
    echo "$unformatted" >&2
    exit 1
  fi
  go vet ./...
'
echo "container-check: PASS"
