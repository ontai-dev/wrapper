# Contributing to wrapper

Thank you for your interest in contributing to the Seam platform.

---

## Before you begin

Read the Seam Platform Constitution (`CLAUDE.md` in the ontai root repository)
and the wrapper component constitution (`CLAUDE.md` in this repository) before
opening a Pull Request. All contributions must respect the platform invariants
defined in those documents.

Key invariants for this repository:

- Wrapper is a thin reconciler. No execution logic lives in this operator. All
  pack compilation runs as conductor executor Jobs.
- `PackInstance` signing is performed exclusively by the management cluster
  Conductor. Wrapper confirms registration only.
- Kueue `ClusterQueue` provisioning is performed by guardian. Wrapper does not
  provision queues.
- No Jobs on the delete path. Pack deletion emits events only.

---

## Development setup

```sh
git clone https://github.com/ontai-dev/wrapper
cd wrapper
go build ./...
go test ./test/unit/...
```

Integration tests require a running management cluster with Kueue installed and
a guardian-provisioned `ClusterQueue` in the tenant namespace. Unit tests run
without a cluster.

---

## Schema changes

All changes to CRD types in `api/v1alpha1/` must be accompanied by a
`docs/wrapper-schema.md` update in the same Pull Request. Schema amendments
require Platform Governor approval.

---

## Pull Request checklist

- [ ] `go build ./...` passes with no errors
- [ ] `go test ./test/unit/...` passes
- [ ] No em dashes in any new documentation
- [ ] No shell scripts added (Go only, per INV-001)
- [ ] Distroless image constraint respected
- [ ] `docs/wrapper-schema.md` updated if CRD types changed

---

## Reporting issues

Open an issue at: https://github.com/ontai-dev/wrapper/issues

For security vulnerabilities, contact the maintainers directly rather than
opening a public issue.

---

## License

By contributing, you agree that your contributions will be licensed under the
Apache License, Version 2.0. See `LICENSE` for the full text.

---

*wrapper - Seam Pack Delivery Operator*
