package install

import (
	"context"
	"strings"
	"testing"
)

// createClusterOpts is a valid --create-cluster invocation (registry + project),
// defaults filled by Options.defaults() at Install time.
func createClusterOpts() Options {
	return Options{
		CreateCluster:   true,
		Registry:        "us-central1-docker.pkg.dev/wren-proj/wren",
		GCPProject:      "wren-proj",
		SkipCredentials: true,
	}
}

// primeGCP adds the canned Outputs the create-cluster path reads: the preflight
// auth probe and the project-number lookup. The cluster-describe existence
// check is deliberately left unset so it errors -> "not present" -> create runs
// (tests that want the reuse path set it explicitly).
func primeGCP(r *FakeRunner) {
	r.Outputs["gcloud auth list --filter=status:ACTIVE --format=value(account)"] = "me@example.com\n"
	r.Outputs["gcloud projects describe wren-proj --format=value(projectNumber)"] = "123456789\n"
}

func TestInstallCreateClusterRequiresProject(t *testing.T) {
	in, _, _, _ := fixture(t)
	opts := createClusterOpts()
	opts.GCPProject = ""
	err := in.Install(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "--gcp-project") {
		t.Fatalf("expected --gcp-project guidance, got %v", err)
	}
}

func TestInstallCreateClusterRequiresRegistry(t *testing.T) {
	in, _, _, _ := fixture(t)
	opts := createClusterOpts()
	opts.Registry = ""
	err := in.Install(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "--registry") {
		t.Fatalf("expected --registry guidance, got %v", err)
	}
}

func TestInstallCreateClusterRejectsKind(t *testing.T) {
	in, _, _, _ := fixture(t)
	opts := createClusterOpts()
	opts.KindCluster = "eval"
	err := in.Install(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutual-exclusion error, got %v", err)
	}
}

func TestInstallCreateClusterPreflightMissingGcloud(t *testing.T) {
	in, _, r, _ := fixture(t)
	// gcloud absent; the other tools present so we reach the gcloud check.
	r.Tools = map[string]bool{"kubectl": true, "docker": true}
	err := in.Install(context.Background(), createClusterOpts())
	if err == nil || !strings.Contains(err.Error(), "gcloud not found") || !strings.Contains(err.Error(), "remedy:") {
		t.Fatalf("expected gcloud-missing remediation, got %v", err)
	}
}

func TestInstallCreateClusterPreflightUnauthenticated(t *testing.T) {
	in, _, r, _ := fixture(t)
	// gcloud present but the active-account probe yields empty -> not logged in.
	r.Outputs["gcloud auth list --filter=status:ACTIVE --format=value(account)"] = "\n"
	err := in.Install(context.Background(), createClusterOpts())
	if err == nil || !strings.Contains(err.Error(), "no active authenticated account") || !strings.Contains(err.Error(), "gcloud auth login") {
		t.Fatalf("expected unauthenticated remediation, got %v", err)
	}
}

func TestInstallCreateClusterHappyPath(t *testing.T) {
	in, k, r, out := fixture(t)
	primeGCP(r)
	if err := in.Install(context.Background(), createClusterOpts()); err != nil {
		t.Fatal(err)
	}
	// Provisioning sequence, in the exact command shapes the brief specifies.
	for _, want := range []string{
		"gcloud services enable container.googleapis.com artifactregistry.googleapis.com --project wren-proj",
		"gcloud container clusters create wren --zone us-central1-a --project wren-proj --num-nodes 1 --machine-type e2-standard-4",
		"gcloud container clusters get-credentials wren --zone us-central1-a --project wren-proj",
		"gcloud auth configure-docker us-central1-docker.pkg.dev --quiet",
		"gcloud projects add-iam-policy-binding wren-proj --member=serviceAccount:123456789-compute@developer.gserviceaccount.com --role=roles/artifactregistry.reader",
	} {
		if !r.Ran(want) {
			t.Errorf("expected run %q, runs: %v", want, r.Runs)
		}
	}
	// Standard, never Autopilot.
	if r.Ran("gcloud container clusters create-auto") {
		t.Errorf("--create-cluster must use GKE Standard, not Autopilot, runs: %v", r.Runs)
	}
	// After provisioning, the normal registry install runs against the created
	// cluster: images pushed and the control-plane refs overridden.
	if !r.Ran("docker push us-central1-docker.pkg.dev/wren-proj/wren/runtime:abc1234") {
		t.Errorf("expected registry push after provisioning, runs: %v", r.SortedRuns())
	}
	if !k.HasCall("OverrideImages:us-central1-docker.pkg.dev/wren-proj/wren:abc1234") {
		t.Errorf("expected OverrideImages after provisioning, calls: %v", k.Calls)
	}
	if !k.HasCall("WaitDeployments:wren-operator,wren-apiserver") {
		t.Errorf("expected control plane wait, calls: %v", k.Calls)
	}
	// Cost transparency: an honest, unmissable notice before doing anything.
	if !strings.Contains(out.String(), "billable") {
		t.Errorf("expected a billable-cost notice, out:\n%s", out.String())
	}
}

func TestInstallCreateClusterIdempotentReuse(t *testing.T) {
	in, _, r, out := fixture(t)
	primeGCP(r)
	// An existing cluster of this name: describe returns it -> reuse, no create.
	r.Outputs["gcloud container clusters describe wren --zone us-central1-a --project wren-proj --format=value(name)"] = "wren\n"
	if err := in.Install(context.Background(), createClusterOpts()); err != nil {
		t.Fatal(err)
	}
	if r.Ran("gcloud container clusters create wren") {
		t.Errorf("existing cluster must be reused, not recreated, runs: %v", r.Runs)
	}
	if !strings.Contains(out.String(), "reusing existing GKE cluster") {
		t.Errorf("expected reuse notice, out:\n%s", out.String())
	}
	// Credentials are still fetched (idempotent) so the context is present.
	if !r.Ran("gcloud container clusters get-credentials wren") {
		t.Errorf("expected get-credentials even on reuse, runs: %v", r.Runs)
	}
}

func TestInstallCreateClusterCustomSizing(t *testing.T) {
	in, _, r, _ := fixture(t)
	primeGCP(r)
	opts := createClusterOpts()
	opts.GCPZone = "europe-west1-b"
	opts.GCPClusterName = "wren-eu"
	opts.GCPMachineType = "e2-standard-4"
	opts.GCPNumNodes = 3
	opts.Registry = "europe-west1-docker.pkg.dev/wren-proj/wren"
	r.Outputs["gcloud projects describe wren-proj --format=value(projectNumber)"] = "123456789\n"
	if err := in.Install(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if !r.Ran("gcloud container clusters create wren-eu --zone europe-west1-b --project wren-proj --num-nodes 3 --machine-type e2-standard-4") {
		t.Errorf("expected custom-sized create, runs: %v", r.Runs)
	}
	// Region prefix derived from the zone drives the docker-auth host.
	if !r.Ran("gcloud auth configure-docker europe-west1-docker.pkg.dev --quiet") {
		t.Errorf("expected region-derived docker auth host, runs: %v", r.Runs)
	}
}

func TestGKEContextName(t *testing.T) {
	if got := GKEContextName("proj", "us-central1-a", "wren"); got != "gke_proj_us-central1-a_wren" {
		t.Errorf("GKEContextName = %q", got)
	}
}

func TestRegionFromZone(t *testing.T) {
	// Input is always a GKE zone (region + "-" + letter).
	cases := map[string]string{
		"us-central1-a":  "us-central1",
		"europe-west1-b": "europe-west1",
		"asia-south1-c":  "asia-south1",
	}
	for in, want := range cases {
		if got := regionFromZone(in); got != want {
			t.Errorf("regionFromZone(%q) = %q, want %q", in, got, want)
		}
	}
}
