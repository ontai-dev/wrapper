// Package crd exposes the wrapper operator's controller-gen CRD YAML files
// as an embedded filesystem. Imported by the Compiler binary to build the
// CRD manifest bundle for compiler launch. conductor-schema.md §9.
package crd

import "embed"

// FS contains all CRD YAML files from this directory.
//
//go:embed *.yaml
var FS embed.FS
