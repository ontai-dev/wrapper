# Wrapper-schema.md
> API Group: infra.ontai.dev
> Operator: Wrapper
> Absorb before any design or implementation work touching pack compile or delivery.
> Amended: 2026-03-30 — compile mode is a workstation/CI operation, never a cluster Job.
>   ClusterPack signing lifecycle added. PackBuild is a local spec file, not a cluster trigger.

---

## 1. Domain Boundary

Wrapper owns the delivery of immutable ClusterPack artifacts to target clusters
and the tracking of their deployed state. It does not own compilation. Compilation
is a workstation or CI/CD pipeline operation performed by the human or an automated
pipeline using conductor in compile mode.

Wrapper is a thin reconciler. Its pattern is identical to all ONT operators:
watch CR → read RunnerConfig → confirm named capability → build Job spec → submit
to Kueue → read OperationResult → update CR status.

Runtime delivers only pre-rendered Kubernetes manifests contained in an OCI
artifact. The management cluster never runs Helm or Kustomize. No rendering
happens on any cluster. This is absolute. INV-014.

---

## 2. The Compile / Runtime Boundary

### 2.1 Compile Time — Workstation or CI/CD Pipeline

Compilation is not a cluster operation. It is a human or pipeline operation using
conductor in compile mode as a Docker container. No management cluster connection
is required.

**Invocation pattern:**
The human or CI/CD pipeline runs Compiler with the PackBuild spec
as input. The runner renders Helm charts via its embedded helm goclient, resolves
Kustomize overlays via its embedded kustomize goclient, normalizes all inputs to
flat Kubernetes manifests, validates schemas, computes execution order, pins image
digests, generates the content-addressed checksum, pushes the ClusterPack OCI
artifact to the registry, and emits a ClusterPack CR YAML file as output.

The ClusterPack OCI artifact contains only raw, fully rendered Kubernetes manifests.
No templates. No variable references. No Helm or Kustomize dependencies at runtime.

The emitted ClusterPack CR YAML is committed to git. GitOps apply delivers it to
the management cluster. This is the only way a ClusterPack CR appears on the
management cluster.

**PackBuild is a local spec file and provenance record.** It is the human's
authoritative declaration of source inputs — Helm chart coordinates, values,
overlay paths. It may optionally be committed to git alongside the ClusterPack
output for audit trail. It is never applied to the management cluster as a Job
trigger. No PackBuild controller exists on the management cluster.

### 2.2 Runtime — Management Cluster (conductor Jobs via Kueue)

When a PackExecution CR is created, the Wrapper controller verifies execution
gatekeeper conditions and submits a pack-deploy Job. The Job runs conductor, which:
- Fetches the ClusterPack OCI artifact from the registry
- Verifies the checksum and the platform signature
- Applies manifests in declared execution order using server-side apply
- Monitors readiness per stage
- Writes OperationResult to ConfigMap
- Exits

The pack-deploy Job uses conductor exclusively. conductor is the distroless
runtime binary. It needs only kube goclient to apply manifests. It never invokes
helm goclient or kustomize goclient. The execution seam between compile mode
and runtime that would require a sidecar or full debian image does not exist
because compile mode never runs on any cluster.

---

## 3. ClusterPack Signing Lifecycle

ClusterPacks require a platform signature before any PackExecution may proceed.
The signing follows the same pattern as PermissionSnapshot signing.

**Signing flow:**

Step 1 — GitOps applies ClusterPack CR to the management cluster.
Wrapper controller detects the new ClusterPack CR with Status.Signed=false
(absent signature annotation).

Step 2 — Management cluster conductor signing loop detects the unsigned ClusterPack
CR. It fetches the OCI artifact, verifies the content checksum matches the CR spec,
signs the artifact digest with the platform private key, and writes the signature
annotation: ontai.dev/pack-signature={base64-encoded-signature}.
Status.Signed transitions to true.

Step 3 — PackExecution gate includes a signature check. The pack-deploy Job will
not be submitted until ClusterPack Status.Signed=true. If the ClusterPack is not
yet signed, Wrapper sets PackSignaturePending on the PackExecution status and
requeues.

Step 4 — On the target cluster, the pack-deploy Job (running conductor) fetches
the ClusterPack artifact and verifies the signature against the platform public key
embedded in the conductor binary before applying any manifest. An artifact that
fails signature verification causes immediate ExecutionFailure with SignatureInvalid
category. Nothing is applied.

**Why this matters:** Unsigned or tampered ClusterPacks cannot be deployed to any
cluster. An artifact produced outside the official compile flow and injected into
the registry without a valid signature is rejected at the Job execution layer.

---

## 4. CRDs — Management Cluster

### PackBuild (Local Spec and Provenance Record — Not a Cluster CRD)

PackBuild is not a Kubernetes CRD applied to the management cluster. It is the
human's local specification file used as input to conductor compile mode. It may
be committed to git alongside the ClusterPack CR output for provenance and audit
trail. The management cluster has no PackBuild controller.

Key fields in the PackBuild spec file (consumed by conductor compile mode):

source.helm.repository: Helm chart repository URL.
source.helm.chart: chart name.
source.helm.version: the admin's version record. The single field to change when
  upgrading a component.
source.helm.values: inline structured data equivalent to the admin's values.yaml.
  Canonical record of all non-sensitive configuration values.
source.helm.valuesSecretRef: optional path to a local file or environment variable
  containing sensitive values — credentials, tokens, API keys. These are merged
  at compile time. They never appear in the ClusterPack CR or OCI artifact metadata.
source.kustomize.path: Kustomize overlay directory path.
source.kustomize.secretRef: optional path to sensitive kustomize patch file.
source.raw: list of raw manifest file paths.
targetVersion: the version string to assign to the produced ClusterPack. Must be
  unique. The runner fails compilation with VersionConflict if a ClusterPack with
  this version already exists in the registry.
outputDir: filesystem path where the runner writes the ClusterPack CR YAML output
  for git commit.

---

### ClusterPack

Scope: Namespaced — seam-tenant-{cluster-name}
Short name: cp
Lives in: OCI registry (artifact), git (CR YAML), management cluster (applied via GitOps).

The management cluster's record of an immutable deployed artifact. Applied to
the management cluster via GitOps from the output of conductor compile mode.
Never created by an operator or controller on the management cluster.

Key spec fields:
- version: declared version string. Immutable after creation.
- registryRef: OCI registry URL and content digest (sha256).
- checksum: content-addressed checksum of the full artifact manifest set.
- sourceBuildRef: optional reference to the PackBuild spec file path in git
  (provenance only — not a cluster object reference).
- executionOrder: stage ordering derived from the compiled execution graph.
  Stages in order: rbac, storage, stateful, stateless.
- lifecyclePolicies: resource retention rules for upgrade and delete operations.
- provenance: build identity, timestamp, Helm chart digest, Kustomize overlay
  digest, compiler version, compilation timestamp.
- targetClusters: list of cluster names (strings) to which this ClusterPack
  should be delivered. The ClusterPackReconciler creates one RunnerConfig per
  entry in seam-tenant-{clusterName}. ClusterAssignment is removed — pack-to-cluster
  binding is declared directly here.

ClusterPack spec never contains: Helm templates, Kustomize overlays, variable
references, runtime decision logic, or values from valuesSecretRef. Their presence
is an invariant violation.

Status fields:
- Signed: bool. Set to true by the management cluster conductor signing loop after
  signature verification and annotation. False on creation.
- packSignature: the base64-encoded platform signature written as annotation
  ontai.dev/pack-signature by the signing loop.

Status conditions: Available (signed and registry-verified), Revoked, SignaturePending.

A ClusterPack in SignaturePending state blocks all PackExecution creation for that
version. The signing loop is the only process that can advance this state.

---

### PackExecution

Scope: Namespaced — seam-tenant-{cluster-name}
Short name: pe
Named capability: pack-deploy

Runtime request to apply a specific ClusterPack version to a specific target
cluster. Contains no execution logic. A pure reference that the PackExecution
controller acts on by submitting a pack-deploy Job to Kueue.

Gates verified before Job submission:
- ClusterPack Status.Signed=true (signature present and valid in annotation).
- Target cluster PermissionSnapshot is current and acknowledged.
- Requesting principal's RBACProfile is provisioned and permits this operation.
- ClusterPack version is Available and not Revoked.

If any gate fails: set specific blocking condition on PackExecution status, requeue
with backoff. Do not submit Job.

If ClusterPack is SignaturePending: set PackSignaturePending. Requeue with 15-second
backoff. Do not submit Job.

Key spec fields: clusterPackRef (name and version), targetClusterRef,
admissionProfileRef.
Status conditions: Pending, PackSignaturePending, Running, Succeeded, Failed.

---

### PackInstance

Scope: Namespaced — seam-tenant-{cluster-name}
Short name: pi

Tracks currently deployed state of a specific pack on a specific target cluster.
One PackInstance per pack per target cluster. Continuously compares expected state
from ClusterPack with PackReceipt drift status from the target cluster conductor.

Key spec fields:
- clusterPackRef: currently active ClusterPack name and version.
- targetClusterRef: target cluster this instance tracks.
- dependsOn: list of PackInstance names that must be Ready before this is
  deployable. Dependencies must exist on the same target cluster.
- dependencyPolicy.onDrift: Block (default for infra packs), Warn (default for
  application packs), Ignore.

Status conditions: Ready, Progressing, Drifted, DependencyBlocked.

---

## 5. CRDs — Target Cluster (Agent-Managed)

### PackReceipt

Scope: Namespaced — ont-system on target cluster.
Short name: pr

Local record of deployed ClusterPack versions and drift status. Created and
maintained exclusively by the conductor Deployment in ont-system on the target
cluster. Never authored by humans or other controllers.

One PackReceipt per deployed pack per target cluster. The management cluster's
PackInstance trusts PackReceipt as the ground truth for delivery confirmation.

Key fields (agent-managed): packRef, appliedAt, checksum, signatureVerified (bool —
set true only after the pack-deploy Job verifies the platform signature before
applying), driftStatus (Clean or Drifted), driftDetails, lastCheckedAt.

The signatureVerified field is critical. A PackReceipt where signatureVerified=false
is treated as a security incident. The PackInstance on the management cluster raises
a SecurityViolation condition and blocks further pack operations on that cluster.

---

## 6. Upgrade and Rollback

### 6.1 Upgrade Flow

A new ClusterPack version is compiled locally or via CI pipeline. The ClusterPack
CR YAML output is committed to git. GitOps applies it to the management cluster.
The conductor signing loop signs it. Once Available, a new PackExecution references
the new version.

Before execution, the runner diff engine in the pack-deploy Job computes the
resource delta between the current PackInstance's active ClusterPack version and
the target version by comparing their respective OCI artifact manifests.

| Resource condition          | Action                              |
|-----------------------------|-------------------------------------|
| Exists in both versions     | Patch via server-side apply         |
| Only in current version     | Apply lifecycle policy              |
| Only in target version      | Create                              |

Lifecycle policies: retain, delete, orphan, replace.

Stateful defaults (require explicit human approval to override):
- PersistentVolumeClaims: retain.
- StatefulSets: retain with protected status.
- Breaking schema changes to stateful resources: mandatory human approval gate.

### 6.2 Rollback

PackExecution referencing a previous ClusterPack version. The previous version
must still be Available and not Revoked. Signing was already performed when the
version was first applied. Same diff engine and execution order apply. No special
reverse logic.

---

## 7. Agent Bootstrap Exception

The agent ClusterPack is the first pack applied to any target cluster. During
bootstrap, conductor applies it directly via kube goclient without going through
the PackExecution flow. Signature verification still occurs — the bootstrap
bootstrap sequence includes signature verification using the embedded public key
before any manifest is applied. No PackReceipt tracking exists yet because no
infra agent is running. This is the single documented exception to the PackExecution
flow model.

After the agent pack is applied, the infra agent comes online, creates its own
PackReceipt for the agent pack with signatureVerified=true, and all subsequent
deliveries follow the normal PackExecution model.

---

## 8. Drift Detection

The conductor on target clusters runs periodic server-side dry-run comparisons
between the expected state from the current PackReceipt and actual live cluster
state. Updates PackReceipt driftStatus. PackInstance on the management cluster
reflects this via its Drifted condition. Remediation is a runner Job submitted
via a new PackExecution — the agent never auto-remediates.

---

## 9. Cross-Domain Rules

Reads: security.ontai.dev/PermissionSnapshot delivery status before admitting
PackExecution. Does not write to security.ontai.dev.
ClusterAssignment is removed. Pack-to-cluster binding is declared directly in
ClusterPack.spec.targetClusters. Wrapper does not read from platform.ontai.dev.
Reads: runner.ontai.dev/RunnerConfig status (capability confirmation).
Writes: runner.ontai.dev/RunnerConfig (generates from ClusterPack/PackExecution
  context via shared runner library — no PackBuild controller).
Writes: infra.ontai.dev resources on management cluster.
Writes: PackReceipt on target clusters via conductor.

Pack delivery ownership chain (locked):
- ClusterPack: human/GitOps authored. Immutable after creation.
- RunnerConfig: created by ClusterPackReconciler, one per targetClusters entry,
  in seam-tenant-{clusterName}, with labels platform.ontai.dev/cluster and
  infra.ontai.dev/pack.
- PackExecution: created by management cluster Conductor agent from RunnerConfig
  (not by Wrapper). Conductor watches RunnerConfigs labeled infra.ontai.dev/pack
  and creates one PackExecution per RunnerConfig that lacks one.
- PackInstance: created by Wrapper PackExecutionReconciler after observing that
  the pack-deploy Job succeeded (OperationResult ConfigMap present and Succeeded).
  Namespace: seam-tenant-{clusterRef}. Label: infra.ontai.dev/pack.
- PackReceipt: created by Conductor agent on the target cluster after verifying
  the PackInstance signature and applying the manifests.

Note: The signing loop is an conductor responsibility. The Wrapper controller
does not sign ClusterPacks. It reads the Signed status and blocks PackExecution
until signing is complete.

---

*infra.ontai.dev schema — Wrapper*
*Amendments:*
*2026-03-30 — Compile mode is a workstation/CI operation, never a cluster Job.*
*  PackBuild removed as a management cluster CRD. No PackBuild controller.*
*  ClusterPack signing lifecycle added (Section 3). Signing is conductor responsibility.*
*  pack-deploy Jobs use conductor (distroless). No execution seam with compile mode.*
*  PackReceipt.signatureVerified field added. SecurityViolation condition added.*
*  PackExecution.PackSignaturePending gate condition added.*
*2026-04-10 — Namespace model locked: ClusterPack, PackExecution, PackInstance all live*
*  in seam-tenant-{cluster-name}, not infra-system. ClusterAssignment removed.*
*  ClusterPack.spec.targetClusters added — pack-to-cluster binding declared directly.*
*  Pack delivery ownership chain locked (Section 9): ClusterPack (human/GitOps),*
*  RunnerConfig (ClusterPackReconciler), PackExecution (Conductor agent from RunnerConfig),*
*  PackInstance (Wrapper after Job success), PackReceipt (target Conductor).*