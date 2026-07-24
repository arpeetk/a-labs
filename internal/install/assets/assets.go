// Package assets embeds the rendered control-plane deployment so `wren install`
// works with no repo checkout and no kustomize binary. config/default stays the
// source of truth — regenerate with `make assets`; CI guards drift via
// `make check-assets`.
package assets

import _ "embed"

// Manifests is `kubectl kustomize config/default`, committed.
//
//go:embed manifests.yaml
var Manifests []byte
