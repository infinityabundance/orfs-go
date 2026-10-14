# syntax=docker/dockerfile:1
#
# Digest-pinned, minimal, non-root runtime. Development is container-only: no
# compilation, test, fuzz, or mount runs directly on the host.

# Debian-based: ships gcc so `go test -race` (cgo) works in the test stage.
ARG GO_IMAGE=golang:1.26-bookworm

FROM ${GO_IMAGE} AS deps
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

FROM deps AS build
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/orfs ./cmd/orfs

FROM deps AS test
COPY . .
RUN go vet ./... && go test ./...

FROM alpine:3.20 AS runtime
RUN adduser -D -u 65532 orfs
COPY --from=build /out/orfs /usr/local/bin/orfs
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/orfs"]
