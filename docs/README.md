# OrFS-GO documentation

Normative specifications and operator guidance. Where a document states a
capability, it is backed by code and, where a behaviour is claimed, by a court in
`internal/*_test.go` or `scripts/`.

## Start here

| Document | What it is |
| --- | --- |
| [`../README.md`](../README.md) | Overview, status, quick start, container workflow |
| [`ORFS-SPEC.md`](ORFS-SPEC.md) | The two laws, the planes, and the port's normative requirements |
| [`PROGRESS.md`](PROGRESS.md) | The ordered phases and their honest state |

## Format

| Document | What it is |
| --- | --- |
| [`ON_DISK.md`](ON_DISK.md) | The byte-exact on-disk format: envelope, registry, payloads, `volume.meta`, genesis, watermark |
| [`MIGRATION.md`](MIGRATION.md) | `or.log` v0 → v1, the compatibility matrix, and the frozen fixtures |

## Operation

| Document | What it is |
| --- | --- |
| [`OPERATIONS.md`](OPERATIONS.md) | Growth monitoring (logical vs physical), the recovery runbook, mount latency, capacity transitions |
| [`MANIFESTATION.md`](MANIFESTATION.md) | The concealed-expansion read path: identity, certification, fallback, measured results |
| [`LIMITATIONS.md`](LIMITATIONS.md) | Every standing non-claim, so absence is explicit |
| [`SECURITY.md`](SECURITY.md) | Threat model; integrity is not authentication |
| [`REJECTED.md`](REJECTED.md) | Designs decided against, with reasons |

## Fixtures and evidence

| Path | What it is |
| --- | --- |
| [`../tests/fixtures/`](../tests/fixtures/README.md) | Frozen `legacy_v0`/`current_v1` compatibility volumes |
| `../evidence/` | Machine-readable output written by the container courts |

## The one rule

**Authority establishes truth; a concealed structure only accelerates observation
of it.** Every document in this directory is written so that no claim outruns the
evidence in the repository.
