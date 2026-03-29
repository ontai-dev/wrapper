# Development Standard

> This document defines the development standard for ont-platform.
> Every agent reads this before beginning implementation work.

---

## 1. Two Controllers, One Pattern

**PackBuildReconciler** — watches PackBuild CRs. Generates RunnerConfig using
shared library GenerateFromPackBuild(spec). Submits pack-compile Job. Reads
OperationResult containing ClusterPack name and OCI digest. Creates ClusterPack
CR with Available status. Updates PackBuild status to Ready with ClusterPack ref.

**PackExecutionReconciler** — watches PackExecution CRs. Verifies execution
gatekeeper conditions by reading PermissionSnapshot delivery status and RBACProfile
provisioned status. If gates pass: submits pack-deploy Job. If gates fail: sets
Pending with specific blocking condition in status. Reads OperationResult.
Updates PackInstance status.

---

## 2. Execution Gatekeeper Check

Before submitting any pack-deploy Job the controller performs three checks:

Check 1 — Read the target cluster's PermissionSnapshot delivery status from
ont-security. If lastAckedVersion does not match expectedVersion: set
PermissionSnapshotOutOfSync on PackExecution and requeue with 30-second backoff.

Check 2 — Read RBACProfile for the requesting principal. If provisioned != true:
set RBACProfileNotProvisioned and requeue.

Check 3 — Read ClusterPack status. If Revoked: set PackRevoked and do not requeue.
A revoked pack requires human intervention — new PackExecution with a valid version.

All three must pass before Job submission proceeds.

---

## 3. ClusterPack Immutability Enforcement

The PackBuildReconciler uses server-side apply to create ClusterPack. It never
updates an existing ClusterPack version spec. If a PackBuild references a version
that already exists as a ClusterPack CR: fail the PackBuild with VersionConflict
status. The human must increment the targetVersion.

---

## 4. Drift Detection Integration

The PackInstanceReconciler watches PackReceipt updates from the runner agent on
target clusters. When PackReceipt.driftStatus changes to Drifted, the controller
updates the corresponding PackInstance status to Drifted. It does not auto-submit
a remediation Job. Drift surfaces to the operator for human decision.

---

## 5. Testing Standard

Unit tests: PackBuildReconciler and PackExecutionReconciler with envtest.
Integration tests: mock OCI registry, verify ClusterPack CR creation from
PackBuild. Verify execution gatekeeper blocks correctly.
e2e tests: full pack compile and deploy cycle on ccs-test cluster. Gate: pack
deployed, PackReceipt Clean, PackInstance Ready.

---

*ont-infra development standard*
*Amendments appended below with date and rationale.*