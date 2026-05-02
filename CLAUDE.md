## wrapper: Operational Constraints
> Read ~/ontai/CLAUDE.md first. The constraints below extend the root constitutional document.

### Schema authority
Primary: docs/wrapper-schema.md
Supporting: ~/ontai/conductor/docs/conductor-schema.md (RunnerConfig contract)
Supporting: ~/ontai/guardian/docs/guardian-schema.md (execution gatekeeper conditions)

### Invariants
CI-INV-001 -- Runtime delivers only pre-rendered Kubernetes manifests. No Helm or Kustomize at runtime. (root INV-014)
CI-INV-002 -- ClusterPack, once registered, is never modified. Changes require a new PackBuild. Immutability is absolute.
CI-INV-003 -- PackExecution is not submitted until all three execution gatekeeper conditions pass: PermissionSnapshot current, RBACProfile provisioned, ClusterPack not revoked.
CI-INV-004 -- Leader election required.
CI-INV-005 -- The agent bootstrap exception is the only context where pack application bypasses PackExecution. It is documented and finite.

### Session protocol additions
Step 4a -- Read wrapper-design.md in this repository.
Step 4b -- Verify the pack-compile or pack-deploy capability is declared in RunnerConfig status before implementing any Job submission.
