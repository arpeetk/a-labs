package install

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// GKEContextName returns the fixed kubeconfig context name GKE's
// get-credentials writes: gke_<project>_<zone>_<name>. Both the installer
// (Options.contextName) and the CLI (installKubeContext) derive the context
// this way, so the rest of Install() targets the freshly-created cluster with
// no special-casing — exactly as the --kind path does (kind-<name>).
func GKEContextName(project, zone, name string) string {
	return fmt.Sprintf("gke_%s_%s_%s", project, zone, name)
}

// regionFromZone strips a GKE zone's trailing letter to get its region
// (us-central1-a -> us-central1) — the form Artifact Registry is hosted at
// and `gcloud auth configure-docker` expects.
func regionFromZone(zone string) string {
	if i := strings.LastIndex(zone, "-"); i > 0 {
		return zone[:i]
	}
	return zone
}

// provisionGKE creates (or reuses) a GKE Standard cluster and wires everything
// the rest of Install() needs to reach and pull into it: enable the container
// + Artifact Registry APIs, create the cluster, fetch credentials (which writes
// the gke_<project>_<zone>_<name> context the rest of Install targets),
// configure docker->Artifact Registry auth for the image push that follows, and
// grant the node service account registry-pull access — closing the exact gap
// diagnosePullFailure/imagePullRemedy (commit 84a4337) exist to catch on
// bring-your-own clusters. GKE Standard only (never `clusters create-auto`):
// Autopilot forbids the WS-1 egress-lockdown privileged init container. No-op
// unless --create-cluster. Runs before checkServer so the cluster is reachable
// by the time Install() talks to it. gcloud is driven through the same Runner
// seam every other external tool uses — no GCP Go SDK.
func (s *steps) provisionGKE(ctx context.Context) error {
	if !s.opts.CreateCluster {
		return nil
	}
	r := s.in.Runner
	proj := s.opts.GCPProject
	zone := s.opts.GCPZone
	name := s.opts.GCPClusterName

	// Cost transparency, not a confirmation gate: --create-cluster is itself the
	// unambiguous signal of intent (creating infrastructure is reversible), but
	// be honest up front that this bills real money before doing anything.
	s.logf("--create-cluster: provisioning GKE Standard cluster %q in project %s zone %s (%d node(s), %s) — this creates real, billable Google Cloud infrastructure",
		name, proj, zone, s.opts.GCPNumNodes, s.opts.GCPMachineType)

	// A genuinely brand-new project has neither API on; enabling is idempotent.
	s.logf("enabling container.googleapis.com + artifactregistry.googleapis.com on %s", proj)
	if err := r.Run(ctx, "gcloud", "services", "enable",
		"container.googleapis.com", "artifactregistry.googleapis.com",
		"--project", proj); err != nil {
		return fmt.Errorf("enable required GCP APIs on %s: %w", proj, err)
	}

	// Create the cluster — GKE Standard (the default subcommand shape), never
	// create-auto. Idempotent: an existing cluster of this name is reused, so a
	// re-install converges rather than erroring "already exists".
	exists, err := s.gkeClusterExists(ctx)
	if err != nil {
		return err
	}
	if exists {
		s.logf("reusing existing GKE cluster %q in %s/%s", name, proj, zone)
	} else {
		s.logf("creating GKE Standard cluster %q (this typically takes several minutes)", name)
		if err := r.Run(ctx, "gcloud", "container", "clusters", "create", name,
			"--zone", zone, "--project", proj,
			"--num-nodes", strconv.Itoa(s.opts.GCPNumNodes),
			"--machine-type", s.opts.GCPMachineType); err != nil {
			return fmt.Errorf("create GKE cluster %q: %w", name, err)
		}
	}

	// Fetch credentials — this writes the gke_<project>_<zone>_<name> context
	// that installKubeContext/contextName resolve to, making the cluster
	// reachable for applyManifests/images/wait with no further changes.
	s.logf("fetching cluster credentials (kubeconfig context %s)", GKEContextName(proj, zone, name))
	if err := r.Run(ctx, "gcloud", "container", "clusters", "get-credentials", name,
		"--zone", zone, "--project", proj); err != nil {
		return fmt.Errorf("get credentials for GKE cluster %q: %w", name, err)
	}

	// Wire docker -> Artifact Registry auth before the push step (registryImages).
	registryHost := regionFromZone(zone) + "-docker.pkg.dev"
	s.logf("configuring docker auth for %s", registryHost)
	if err := r.Run(ctx, "gcloud", "auth", "configure-docker", registryHost, "--quiet"); err != nil {
		return fmt.Errorf("configure docker auth for %s: %w", registryHost, err)
	}

	// Grant the node service account registry-pull access — the exact grant
	// imagePullRemedy prints as the manual fix. With --create-cluster nobody
	// should ever actually see that diagnostic fire.
	return s.grantNodePull(ctx)
}

// gkeClusterExists reports whether a cluster of the configured name already
// lives in the target project/zone, so a re-install reuses it instead of
// erroring on create. Any describe error is treated as "not present yet": the
// common case is a genuine not-found, and a real problem (bad project, no auth)
// surfaces with a clear error from the create call that follows.
func (s *steps) gkeClusterExists(ctx context.Context) (bool, error) {
	_, err := s.in.Runner.Output(ctx, "gcloud", "container", "clusters", "describe",
		s.opts.GCPClusterName, "--zone", s.opts.GCPZone, "--project", s.opts.GCPProject,
		"--format=value(name)")
	return err == nil, nil
}

// grantNodePull resolves the project number and grants the default compute node
// service account roles/artifactregistry.reader — the sequence imagePullRemedy
// documents. add-iam-policy-binding is idempotent (re-granting is a no-op).
func (s *steps) grantNodePull(ctx context.Context) error {
	proj := s.opts.GCPProject
	num, err := s.in.Runner.Output(ctx, "gcloud", "projects", "describe", proj,
		"--format=value(projectNumber)")
	if err != nil {
		return fmt.Errorf("resolve project number for %s: %w", proj, err)
	}
	member := fmt.Sprintf("serviceAccount:%s-compute@developer.gserviceaccount.com", strings.TrimSpace(num))
	s.logf("granting node service account %s roles/artifactregistry.reader", member)
	if err := s.in.Runner.Run(ctx, "gcloud", "projects", "add-iam-policy-binding", proj,
		"--member="+member, "--role=roles/artifactregistry.reader"); err != nil {
		return fmt.Errorf("grant node service account registry-pull access: %w", err)
	}
	return nil
}
