#!/bin/sh
# The default suite inside the pinned container.
set -eu
. "$(dirname "$0")/lib.sh"
docker_run sh -c 'go vet ./... && go test ./...'
echo "container-test: PASS"
