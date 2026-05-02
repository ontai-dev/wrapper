# wrapper: Codebase Reference

## 1. Purpose

Wrapper is the pack delivery engine for the ONT platform. It manages the lifecycle of pre-compiled OCI artifact deliveries (`InfrastructureClusterPack`) to target clusters: enforcing 6 delivery gates (gates 0-5) before submitting a `pack-deploy` Kueue Job, tracking delivered state via `InfrastructurePackInstance`, and managing drift visibility via `InfrastructurePackReceipt`. Wrapper does NOT compile packs (conductor/compiler), sign packs (conductor agent on management cluster), own RBAC governance (guardian), or manage cluster lifecycle (platform). It does not apply Helm or Kustomize at runtime.

Wrapper has NO own CRD type definitions. `api/v1alpha1/` contains only `.gitkeep`. All types consumed by wrapper (InfrastructureClusterPack, InfrastructurePackExecution, InfrastructurePackInstance, InfrastructurePackReceipt, PackOperationResult, DriftSignal) are defined in seam-core (Decision G).

---

## 2. Key Files and Locations

### Controllers (`internal/controller/`)

#### `packexecution_reconciler.go`

`PackExecutionReconciler` (L74 comment block, `Reconcile()` L121). Manages the 6-gate delivery pipeline.

**Gate check flow** (all gates at L175-417):

| Gate | Line | Condition | Blocks on |
|------|------|-----------|-----------|
| 0 | L176 | ConductorReady | `isConductorReadyForCluster()` L799 -- checks RunnerConfig in `ont-system` has `status.capabilities` non-empty |
| 1 | L221 | Signature | `ClusterPack.status.Signed=true` |
| 2 | L289 | Revocation | ClusterPack conditions Revoked != True |
| 3 | L306 | PermissionSnapshot | `isPermissionSnapshotCurrent()` L716 -- reads PermissionSnapshot via unstructured (no cross-operator type import) |
| 4 | L343 | RBACProfile | `isRBACProfileProvisioned()` L755 -- checks `provisioned=true` on the pack's RBACProfile |
| 5 | L378 | WrapperRunnerRBAC | `isWrapperRunnerRBACReady()` L849 -- SubjectAccessReview verifies wrapper-runner SA has required permissions |

`gateRequeueInterval = 30 * time.Second` (L61). Failing a gate sets `ConditionTypePackExecutionPending=True` with `ReasonGatesClearing` and returns `RequeueAfter: gateRequeueInterval`.

`RBACReadyChecker` type at L101: `func(ctx, *InfrastructurePackExecution) (bool, string, error)`. Production uses `isWrapperRunnerRBACReady`; test stub set via `r.RBACChecker` field (L107).

`findLatestPOR()` at L1162: lists all PackOperationResult CRs in namespace labeled with `packExecutionRef`, returns the one with highest `Spec.Revision`. Called at L466 to check completion status.

#### `clusterpack_reconciler.go`

`ClusterPackReconciler.Reconcile()` L67. Called on ClusterPack create/update.

`handleClusterPackDeletion()` L393: three steps + step 2.5:
1. L396: List all PackInstances cluster-wide, delete those where `spec.clusterPackRef == cp.Name`.
2. L415: List all PackExecutions cluster-wide, delete those where `spec.clusterPackRef.name == cp.Name`.
3. Step 2.5 (L434): Delete DriftSignal named `drift-{cp.Name}` in `seam-tenant-{clusterName}` for each target cluster.
4. L449: Remove finalizer `clusterPackFinalizer` so API server can delete the ClusterPack object.

`handleRollback()` L306: SSA-patches ClusterPack spec back to a previous version. Normal reconcile then creates PackExecution for the rolled-back version.

PackExecution creation (L230): for each cluster in `spec.targetClusters`, creates one PackExecution in `seam-tenant-{cluster}`. Skips if PackInstance with current version already exists (L243). Skips if PackExecution already exists (L258).

---

## 3. Primary Data Flows

**Pack deploy path**: ClusterPack created --> `ClusterPackReconciler` creates PackExecution in `seam-tenant-{cluster}` --> `PackExecutionReconciler` runs 6-gate check --> all gates pass --> Kueue Job (`pack-deploy`, `conductor-execute:dev` image) submitted --> conductor execute-mode `executeSplitPath()` applies RBAC + cluster-scoped + workload OCI layers --> writes PackOperationResult --> `PackExecutionReconciler` reads POR via `findLatestPOR()` L1162 --> creates PackInstance on management cluster.

**ClusterPack deletion path**: Finalizer prevents deletion --> `handleClusterPackDeletion()` L393 runs 3 steps (PackInstances, PackExecutions, DriftSignals) --> removes finalizer --> API server deletes ClusterPack object. Conductor `teardownOrphanedReceipt()` then cleans up deployed resources on the tenant cluster.

**Pack rollback**: `spec.rollbackToRevision` set on ClusterPack --> `handleRollback()` L306 patches spec --> `clearRollbackField()` L378 clears the field --> normal reconcile creates new PackExecution for rolled-back version.

**Single-active-revision (POR)**: `conductor/internal/persistence/operationresult_writer.go` writes POR with `Revision` incremented. Predecessor labeled `ontai.dev/superseded=true`, retained max 10. `findLatestPOR()` L1162 selects highest revision.

---

## 4. PackExecution naming and supersession

PackExecution name: `{packName}-{targetCluster}`. PackInstance name: `{basePackName}-{targetCluster}`. Same base name enables supersession: when a newer ClusterPack version arrives, the existing PackInstance is replaced in-place (same name, new content) rather than creating a new object. This is the upgrade path.

---

## 5. Invariants

| ID | Rule | Location |
|----|------|----------|
| CP-INV-010 | Kueue is not used for any operation in platform. Pack-deploy Jobs are the only Kueue Jobs in wrapper. | `packexecution_reconciler.go` |
| Decision G | Wrapper has no own CRD type definitions | `api/v1alpha1/.gitkeep` |

---

## 6. Open Items

**PLATFORM-BL-WRAPPER-RUNNER-RBAC-LIFECYCLE (platform)**: `ensureWrapperRunnerResources()` in `platform/internal/controller/taloscluster_helpers.go` creates wrapper-runner SA/Role/RoleBinding/ClusterRoleBinding at tenant onboarding. `handleTalosClusterDeletion()` does NOT delete `ClusterRoleBinding wrapper-runner-{cluster}`. This is a platform open item, not a wrapper open item.

**CLUSTERPACK-BL-VERSION-CLEANUP (conductor)**: `DeployedResources` field exists in `InfrastructurePackReceiptSpec` at `seam-core/api/v1alpha1/packreceipt_types.go:74`. When PackInstance version N+1 replaces N, resources present in N's PackReceipt but absent from N+1's manifests are NOT cleaned up. Version-upgrade orphan diff is absent from `conductor/internal/agent/packinstance_pull_loop.go`. No schema addition needed; only implementation missing.

---

## 7. Test Contract

| Package | Coverage |
|---------|----------|
| `test/unit/controller` | PackExecutionReconciler (all 6 gates, POR revision selection), ClusterPackReconciler (deletion, rollback) |
| `test/e2e` | Stub files; all skip when `MGMT_KUBECONFIG` absent; skip reasons reference backlog item IDs |
