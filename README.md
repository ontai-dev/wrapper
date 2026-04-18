# wrapper

**Seam Pack Delivery operator**
**API Group:** `infra.ontai.dev`
**Image:** `registry.ontai.dev/ontai-dev/wrapper:<semver>`

---

## What this repository is

`wrapper` is the pack compile and delivery operator in the Seam platform. It owns
the full lifecycle of Seam packs from compilation intent through cluster delivery
and drift detection.

---

## CRDs

| Kind | API Group | Role |
|---|---|---|
| `ClusterPack` | `infra.ontai.dev` | Declared pack composition for a target cluster |
| `PackExecution` | `infra.ontai.dev` | Compile-and-deliver intent record for a single pack |
| `PackInstance` | `infra.ontai.dev` | Registered and signed pack artifact record |

---

## Architecture

Wrapper is a thin reconciler. Its reconcile loop is: watch `ClusterPack`, read
`RunnerConfig`, confirm the named `pack-compile` capability exists in the conductor
manifest, build a Kueue Job spec, submit to the tenant `ClusterQueue`, read
`OperationResult`, and update `PackExecution` status.

**Pack compilation** runs as a Kueue Job. The Kueue `ClusterQueue` for each tenant
is provisioned by guardian from `QueueProfile`. Wrapper does not provision queues.

**PackInstance signing** is performed exclusively by the management cluster Conductor
in agent mode after wrapper confirms `ClusterPack` registration. Wrapper does not sign.

**Cilium** is delivered as a Seam pack. The `CiliumPending` condition on `TalosCluster`
(owned by platform) is the expected state between CAPI cluster Running and Cilium
`PackInstance` reaching Ready. This window is not an error state.

---

## Building

```sh
go build ./cmd/wrapper
```

The binary is built into a distroless container image:

```sh
docker build -t registry.ontai.dev/ontai-dev/wrapper:<semver> .
```

---

## Testing

```sh
go test ./test/unit/...
```

---

## Schema and design reference

- `docs/wrapper-schema.md` - API contract, field definitions, status conditions
- `wrapper-design.md` - Implementation architecture and reconciler design

---

*wrapper - Seam Pack Delivery Operator*
*Apache License, Version 2.0*
