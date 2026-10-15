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
# zfsutils-linux lives in Debian's `contrib` component, so enable it before the
# install. The OpenZFS retention court (scripts/container-retention-zfs.sh) ships
# the `zpool`/`zfs` userspace tools (and kmod, so `modprobe -n` can be queried) so
# it can probe for a reachable OpenZFS. The ZFS kernel module itself is a host
# concern: it is never installed or loaded here.
RUN if [ -f /etc/apt/sources.list.d/debian.sources ]; then \
      sed -i 's/^Components: main.*/Components: main contrib/' /etc/apt/sources.list.d/debian.sources; \
    fi \
 && apt-get update \
 && apt-get install -y --no-install-recommends \
      fio fuse3 psmisc util-linux coreutils \
      e2fsprogs xfsprogs btrfs-progs \
      zfsutils-linux kmod \
      perl libtest-harness-perl \
      git autoconf automake make \
 && rm -rf /var/lib/apt/lists/*
COPY . .
RUN CGO_ENABLED=0 go build -o /out/orfs ./cmd/orfs \
 && CGO_ENABLED=0 go build -o /out/fiosummary ./cmd/fiosummary \
 && CGO_ENABLED=0 go build -o /out/orfsstress ./cmd/orfsstress \
 && CGO_ENABLED=0 go build -o /out/orfsrw ./cmd/orfsrw \
 && CGO_ENABLED=0 go build -o /out/gcsummary ./cmd/gcsummary \
 && CGO_ENABLED=0 go build -o /out/transportsummary ./cmd/transportsummary \
 && CGO_ENABLED=0 go build -o /out/orfsworm ./cmd/orfsworm \
 && CGO_ENABLED=0 go build -o /out/orfsaudit ./cmd/orfsaudit \
 && CGO_ENABLED=0 go build -o /out/fuseuringprobe ./cmd/fuseuringprobe \
 && CGO_ENABLED=0 go build -o /out/orfsxlang ./cmd/orfsxlang \
 && CGO_ENABLED=0 go build -o /out/orfsoptprobe ./cmd/orfsoptprobe \
 && CGO_ENABLED=0 go build -o /out/orfscompare ./cmd/orfscompare \
 && CGO_ENABLED=0 go build -o /out/osscomparesummary ./cmd/osscomparesummary \
 && CGO_ENABLED=0 go build -o /out/s3load ./courts/s3bench/s3load \
 && CGO_ENABLED=0 go build -o /out/orfs-s3 ./cmd/orfs-s3 \
 && CGO_ENABLED=0 go build -o /out/retentionbench ./cmd/retentionbench \
 && CGO_ENABLED=0 go build -o /out/retentionsummary ./cmd/retentionsummary

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

# The OpenZFS retention court's KVM micro-VM stage: the court image plus a KVM
# hypervisor (qemu), cloud-init tooling, a static busybox (the HTTP server that
# hands the driver to the guest), and curl (to fetch the pinned guest image).
# OpenZFS itself is never installed here: the *guest* kernel provides it, and
# only inside the VM. The host kernel is never touched.
FROM court AS zfs-kvm
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      qemu-system-x86 cloud-image-utils busybox-static curl \
 && rm -rf /var/lib/apt/lists/*

FROM alpine:3.20 AS runtime
RUN adduser -D -u 65532 orfs
COPY --from=build /out/orfs /usr/local/bin/orfs
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/orfs"]
