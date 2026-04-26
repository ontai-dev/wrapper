// Package crd previously embedded wrapper's own CRD YAML files.
// After T-2B-9 migration, all wrapper CRDs (InfrastructureClusterPack,
// InfrastructurePackExecution, InfrastructurePackInstance) are declared in
// seam-core (infrastructure.ontai.dev). The compiler bundles them from
// seam-core/config/crd directly.
// This package is retained for structural consistency only.
package crd

import "embed"

// FS is an empty embedded filesystem. Wrapper's CRDs are now in seam-core.
var FS embed.FS
