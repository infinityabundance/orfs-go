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

# The court image: Go toolchain + fio + FUSE userspace, for the mounted courts.
FROM deps AS court
RUN apt-get update \
 && apt-get install -y --no-install-recommends fio fuse3 psmisc util-linux coreutils \
 && rm -rf /var/lib/apt/lists/*
COPY . .
RUN CGO_ENABLED=0 go build -o /out/orfs ./cmd/orfs \
 && CGO_ENABLED=0 go build -o /out/fiosummary ./cmd/fiosummary \
 && CGO_ENABLED=0 go build -o /out/orfsstress ./cmd/orfsstress

FROM alpine:3.20 AS runtime
RUN adduser -D -u 65532 orfs
COPY --from=build /out/orfs /usr/local/bin/orfs
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/orfs"]
