// Package v1alpha1 contains the Wren API types for the group wren.dev.
//
// These are the CRD Go types the operator reconciles. DeepCopy methods
// (zz_generated.deepcopy.go) and CRD YAML manifests are generated with
// controller-gen (see `make generate` / `make manifests`).
//
// +kubebuilder:object:generate=true
// +groupName=wren.dev
package v1alpha1

import "k8s.io/apimachinery/pkg/runtime/schema"

// GroupVersion is the API group and version for all Wren resources.
var GroupVersion = schema.GroupVersion{Group: "wren.dev", Version: "v1alpha1"}
