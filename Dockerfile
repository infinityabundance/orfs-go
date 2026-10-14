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

# The court image: Go toolchain + fio + FUSE userspace, the filesystems the
# cross-FS determinism court formats and mounts, and the upstream pjdfstest
# POSIX suite, for the mounted courts.
FROM deps AS court
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      fio fuse3 psmisc util-linux coreutils \
      e2fsprogs xfsprogs btrfs-progs \
      perl libtest-harness-perl \
      git autoconf automake make \
 && rm -rf /var/lib/apt/lists/*
COPY . .
RUN CGO_ENABLED=0 go build -o /out/orfs ./cmd/orfs \
 && CGO_ENABLED=0 go build -o /out/fiosummary ./cmd/fiosummary \
 && CGO_ENABLED=0 go build -o /out/orfsstress ./cmd/orfsstress \
 && CGO_ENABLED=0 go build -o /out/transportsummary ./cmd/transportsummary \
 && CGO_ENABLED=0 go build -o /out/orfsworm ./cmd/orfsworm \
 && CGO_ENABLED=0 go build -o /out/fuseuringprobe ./cmd/fuseuringprobe

# Build the upstream pjdfstest suite into the image so the pjdfstest court can
# run it without cloning at court time. gcc ships in the Go base image; the
# build produces /opt/pjdfstest/pjdfstest (the helper the tests invoke) and
# /opt/pjdfstest/tests.
ARG PJD_URL=https://github.com/pjd/pjdfstest
RUN git clone --depth 1 "${PJD_URL}" /opt/pjdfstest \
 && cd /opt/pjdfstest \
 && autoreconf -ifs \
 && ./configure \
 && make pjdfstest

FROM alpine:3.20 AS runtime
RUN adduser -D -u 65532 orfs
COPY --from=build /out/orfs /usr/local/bin/orfs
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/orfs"]
