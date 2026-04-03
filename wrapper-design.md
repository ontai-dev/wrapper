# Development Standard

> No code. Architecture, patterns, and constraints only.
> Amended: 2026-03-30 — PackBuild controller removed. Compile is workstation/CI.
>   ClusterPack signing lifecycle added. SignaturePending gate documented.

---

## 1. What Changed and Why

Previous versions of this document described wrapper as having a PackBuild
controller that submitted pack-compile Kueue Jobs. This was wrong.

Pack compilation runs on the human's workstation or in a CI/CD pipeline as a
Docker container running conductor compile mode. The management cluster never
runs compile mode. The management cluster only ever receives the output: a
ClusterPack CR applied via GitOps, and a ClusterPack OCI artifact in the registry.

This means:
- No PackBuild controller exists in wrapper.
- No pack-compile Kueue Jobs exist on the management cluster.
- pack-deploy Jobs use conductor (distroless) exclusively — no conductor debian
  image needed at runtime.
- The execution seam problem (sidecar/init-container) is resolved by architectural
  clarity: compile never runs on a cluster.

---

## 2. Two Controllers, One Pattern

**ClusterPackReconciler** — watches ClusterPack CRs that arrive via GitOps apply.
Verifies the ClusterPack CR is structurally valid. Checks that the OCI artifact
exists at the declared registry reference. Sets Status.Signed=false on creation.
Waits for the conductor signing loop to set Status.Signed=true before the
ClusterPack transitions to Available. Does not trigger any Job.

**PackExecutionReconciler** — watches PackExecution CRs. Verifies all execution
gatekeeper conditions. If all gates pass: submits pack-deploy Job. Reads
OperationResult. Updates PackInstance status.

There is no PackBuildReconciler. PackBuild is a local spec file, not a cluster CRD.

---

## 3. ClusterPackReconciler Behavior

On ClusterPack creation (GitOps apply):
1. Validate that spec.version is unique — no existing ClusterPack in infra-system
   has this version. If duplicate: set VersionConflict status. Do not proceed.
2. Validate that spec.registryRef is reachable and the digest resolves. If not:
   set RegistryUnreachable status. Requeue with backoff.
3. Set Status.Signed=false, Status condition SignaturePending.
4. Do nothing further — the conductor signing loop takes over.

On ClusterPack update (signing loop writes signature annotation):
1. Detect ontai.dev/pack-signature annotation present.
2. Verify the signature annotation format is valid base64.
3. Transition Status.Signed=true, Status condition Available.
4. Any PackExecution CRs blocked on PackSignaturePending will be requeued
   automatically by the PackExecutionReconciler's backoff loop.

ClusterPackReconciler never modifies ClusterPack spec. It only reads spec and
writes to status subresource.

---

## 4. Execution Gatekeeper Check (PackExecutionReconciler)

Before submitting any pack-deploy Job the controller performs four checks in order:

**Check 1 — Signature gate:**
Read ClusterPack for the referenced version. If Status.Signed=false: set
PackSignaturePending on PackExecution. Requeue with 15-second backoff. Do not
proceed to further checks.

**Check 2 — Revocation gate:**
If ClusterPack Status.condition=Revoked: set PackRevoked on PackExecution. Do not
requeue. A revoked pack requires human intervention — new PackExecution with a
valid version.

**Check 3 — PermissionSnapshot gate:**
Read the target cluster's PermissionSnapshot delivery status from guardian.
If lastAckedVersion does not match expectedVersion: set PermissionSnapshotOutOfSync
on PackExecution. Requeue with 30-second backoff.

**Check 4 — RBACProfile gate:**
Read RBACProfile for the requesting principal. If provisioned != true: set
RBACProfileNotProvisioned. Requeue with 30-second backoff.

All four must pass before Job submission proceeds. The order matters — signature
check runs first because a tampered or unsigned pack should never reach the
security gating layer.

---

## 5. ClusterPack Immutability Enforcement

The ClusterPackReconciler must reject any attempt to update an existing ClusterPack
spec after creation. ClusterPack spec is immutable. The only mutable surface is
status. If a spec update is detected (by comparing resourceVersion and spec hash):
set ImmutabilityViolation status condition and log the attempt as a security event.
This should never happen via normal GitOps operation — it indicates either a bug
in the CI pipeline or a malicious injection attempt.

---

## 6. Job Spec for pack-deploy

The pack-deploy Job spec:
- Image: conductor image (distroless — kube goclient only, no helm, no kustomize)
- Env: CAPABILITY=pack-deploy, CLUSTER_REF={cluster-name},
  OPERATION_RESULT_CM={configmap-name}, PACK_REGISTRY_REF={oci-uri},
  PACK_CHECKSUM={sha256}, PACK_SIGNATURE={base64-signature}
- Volumes: kubeconfig secret from ont-system mounted read-only
- The OCI artifact digest and signature are passed via environment — the agent
  fetches and verifies before applying anything
- ServiceAccount: infra-system/wrapper-runner with minimum permissions
- TTL: 600 seconds post-completion
- Kueue labels: kueue.x-k8s.io/queue-name from RunnerConfig reference

The pack-deploy Job does NOT mount the conductor image. It uses conductor only.
This is possible because all rendering happened at compile time on the workstation.
The Job applies already-rendered manifests — no intelligence required at runtime.

---

## 7. Signing Loop (conductor Responsibility — Referenced Here for Clarity)

The signing loop runs in the management cluster conductor Deployment in ont-system.
It is NOT part of the wrapper controller. It is implemented in the conductor
shared library and executed by conductor.

The signing loop watches for ClusterPack CRs with Status.Signed=false:
1. Fetch the OCI artifact from the registry using the digest in spec.registryRef.
2. Verify the artifact checksum matches spec.checksum. If mismatch: raise
   ChecksumMismatch security event. Do not sign. Flag for human review.
3. Sign the artifact digest with the platform private key (RS256).
4. Write the base64-encoded signature as annotation ontai.dev/pack-signature.
5. The ClusterPackReconciler detects the annotation and transitions status.

The platform private key is mounted as a projected Secret into the management
cluster conductor Deployment. It is not accessible to any other process. The
corresponding public key is embedded in the conductor binary at build time.

wrapper owns the ClusterPack CR schema and the PackExecution flow. It does not
own the signing key, the signing logic, or the signing loop. Those belong to
conductor. The wrapper controller only reads the signed status.

---

## 8. Drift Detection Integration

The conductor on target clusters runs periodic server-side dry-run comparisons.
When PackReceipt.driftStatus changes to Drifted, the PackInstanceReconciler updates
the corresponding PackInstance status to Drifted. It does not auto-submit a
remediation Job. Drift surfaces to the human for decision.

If PackReceipt.signatureVerified=false is ever detected (indicating a pack was
somehow applied without signature verification), the PackInstanceReconciler raises
a SecurityViolation condition on PackInstance. This condition blocks all further
pack operations on the affected cluster until cleared by human investigation.

---

## 9. No PackBuild Controller — Developer Reminder

There is no PackBuildReconciler. There is no PackBuildController. There is no
pack-compile Kueue Job. Any implementation that creates any of these is incorrect.

If a developer sees PackBuild as a Kubernetes CRD applied to the management cluster,
that is a bug in the GitOps configuration — PackBuild is a local spec file only.

The two controllers in wrapper are ClusterPackReconciler and PackExecutionReconciler.
No others.

---

## 10. Testing Standard

Unit tests: ClusterPackReconciler signature gate logic, PackExecutionReconciler
four-gate check sequence, ClusterPack immutability rejection.

Integration tests: mock OCI registry with valid and invalid signatures. Verify
SignaturePending blocks PackExecution. Verify revoked ClusterPack blocks with no
requeue. Verify all four gates in correct order.

e2e tests: full pack compile (local), GitOps apply ClusterPack CR, conductor signs,
PackExecution deploys to ccs-test. Gate: PackReceipt.signatureVerified=true,
PackInstance Ready, no drift.

Security regression tests: tampered OCI artifact (checksum mismatch) must be
rejected at signing loop. Missing signature must block PackExecution. Invalid
signature on target cluster must cause SignatureInvalid ExecutionFailure.

---

*wrapper development standard*
*Amendments:*
*2026-03-30 — Complete redesign. PackBuild controller removed. PackBuildReconciler*
*  removed. Compile-mode Kueue Jobs removed. ClusterPackReconciler added with*
*  signing gate. SignaturePending gate added to execution gatekeeper as first check.*
*  Section 9 added as explicit no-PackBuild reminder for developers.*
*  Signing loop ownership clarified: conductor implements, wrapper controller reads.*