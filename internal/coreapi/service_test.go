package coreapi

import (
	"context"
	"errors"
	"testing"
	"time"

	wrenv1 "github.com/summiteight/wren/api/v1alpha1"
	"github.com/summiteight/wren/internal/launcher"
	"github.com/summiteight/wren/internal/store"
)

func newService(t *testing.T) (*Service, *store.Memory, *launcher.Fake) {
	t.Helper()
	st := store.NewMemory()
	fl := launcher.NewFake()
	svc := New(st, fl, DefaultDefaults())
	svc.idgen = func() string { return "r-fixed" }
	svc.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	return svc, st, fl
}

func seedProject(t *testing.T, svc *Service, p *store.Project) {
	t.Helper()
	if _, err := svc.CreateProject(context.Background(), p); err != nil {
		t.Fatalf("seed project: %v", err)
	}
}

func TestCreateProjectValidation(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	// name is required.
	if _, err := svc.CreateProject(ctx, &store.Project{Repo: "x/y"}); !errors.Is(err, ErrValidation) {
		t.Errorf("missing name = %v", err)
	}
	// repo is OPTIONAL: a keyless (repo-less) project creates successfully.
	keyless, err := svc.CreateProject(ctx, &store.Project{Name: "keyless", DefaultHarness: "mock"})
	if err != nil {
		t.Fatalf("empty-repo project should be valid: %v", err)
	}
	if keyless.Repo != "" {
		t.Errorf("keyless project repo = %q, want empty", keyless.Repo)
	}
	if keyless.CreatedAt.IsZero() {
		t.Error("CreatedAt not set (empty-repo)")
	}
	// A project with a repo still creates.
	p, err := svc.CreateProject(ctx, &store.Project{Name: "p", Repo: "x/y"})
	if err != nil {
		t.Fatal(err)
	}
	if p.CreatedAt.IsZero() {
		t.Error("CreatedAt not set")
	}
}

func TestCreateRunValidation(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	cases := []CreateRunRequest{
		{User: "u", Prompt: "p"},    // no project
		{Project: "p", User: "u"},   // no prompt
		{Project: "p", Prompt: "p"}, // no user
	}
	for i, req := range cases {
		if _, err := svc.CreateRun(ctx, req); !errors.Is(err, ErrValidation) {
			t.Errorf("case %d: err = %v, want validation", i, err)
		}
	}
}

func TestCreateRunProjectNotFound(t *testing.T) {
	svc, _, _ := newService(t)
	_, err := svc.CreateRun(context.Background(), CreateRunRequest{Project: "ghost", User: "u@x", Prompt: "hi"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestCreateRunResolvesConfigAndCreatesCR(t *testing.T) {
	svc, st, fl := newService(t)
	ctx := context.Background()
	seedProject(t, svc, &store.Project{
		Name: "payments-api", Repo: "corp/payments-api",
		DefaultHarness: "codex", HarnessImage: "reg/codex:1", DefaultModel: "m1",
		RuntimeClass: "runc", CPU: "1", Memory: "2Gi", Disk: "5Gi",
		CheckpointBucket: "gs://proj-ckpt", EgressAllowlist: []string{"github.com"},
	})

	// Request overrides harness + cpu; everything else from project/defaults.
	run, err := svc.CreateRun(ctx, CreateRunRequest{
		Project: "payments-api", User: "Arpeet@Corp.com", Prompt: "do it",
		Harness: "claude-code", CPU: "4", BaseRef: "main", Interactive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.ID != "r-fixed" || run.Phase != string(wrenv1.PhasePending) {
		t.Fatalf("run = %+v", run)
	}
	// Namespace derived + sanitized from the user identity.
	if run.Namespace != "user-arpeet-corp-com" {
		t.Errorf("namespace = %q", run.Namespace)
	}

	// Namespace ensured and CR created in the (fake) cluster.
	if !fl.Namespaces[run.Namespace] {
		t.Error("namespace not ensured")
	}
	cr, err := fl.GetRun(ctx, run.Namespace, "r-fixed")
	if err != nil {
		t.Fatalf("CR not created: %v", err)
	}
	// Effective config: override wins over project over defaults.
	if cr.Spec.Harness.Kind != "claude-code" {
		t.Errorf("harness = %q, want override claude-code", cr.Spec.Harness.Kind)
	}
	if cr.Spec.Harness.Image != "reg/codex:1" {
		t.Errorf("image = %q, want project image", cr.Spec.Harness.Image)
	}
	if cr.Spec.Harness.Model != "m1" {
		t.Errorf("model = %q, want project default", cr.Spec.Harness.Model)
	}
	if got := cr.Spec.Sandbox.Resources.CPU.String(); got != "4" {
		t.Errorf("cpu = %q, want override 4", got)
	}
	if got := cr.Spec.Sandbox.Resources.Memory.String(); got != "2Gi" {
		t.Errorf("memory = %q, want project 2Gi", got)
	}
	if cr.Spec.Workspace.Checkpoint.Bucket != "gs://proj-ckpt" {
		t.Errorf("bucket = %q", cr.Spec.Workspace.Checkpoint.Bucket)
	}
	if len(cr.Spec.Egress.Allowlist) != 1 || cr.Spec.Egress.Allowlist[0] != "github.com" {
		t.Errorf("allowlist = %v", cr.Spec.Egress.Allowlist)
	}
	if !cr.Spec.Interactive {
		t.Error("interactive not propagated")
	}

	// Store record persisted.
	if _, err := st.GetRun(ctx, "r-fixed"); err != nil {
		t.Fatalf("store record missing: %v", err)
	}
}

// TestCreateRunFallsBackToDefaultHarnessImage guards against a repeat of the
// WS-14 gap: a project registered with no --harness-image must resolve to a
// real, buildable image (`wren install`'s kind zero-config default), never
// the old dead "wren/claude-code-runner:latest" placeholder.
func TestCreateRunFallsBackToDefaultHarnessImage(t *testing.T) {
	svc, _, fl := newService(t)
	ctx := context.Background()
	seedProject(t, svc, &store.Project{Name: "demo", Repo: "corp/demo"})

	run, err := svc.CreateRun(ctx, CreateRunRequest{Project: "demo", User: "u@corp.com", Prompt: "do it"})
	if err != nil {
		t.Fatal(err)
	}
	cr, err := fl.GetRun(ctx, run.Namespace, run.ID)
	if err != nil {
		t.Fatalf("CR not created: %v", err)
	}
	want := DefaultDefaults().HarnessImage
	if cr.Spec.Harness.Image != want {
		t.Errorf("harness image = %q, want the service default %q", cr.Spec.Harness.Image, want)
	}
	if cr.Spec.Harness.Image == "wren/claude-code-runner:latest" {
		t.Error("fell back to the dead placeholder image — this repo builds no such image")
	}
}

func TestCreateRunInvalidResourceOverride(t *testing.T) {
	svc, _, _ := newService(t)
	seedProject(t, svc, &store.Project{Name: "p", Repo: "x/y"})
	_, err := svc.CreateRun(context.Background(), CreateRunRequest{
		Project: "p", User: "u@x", Prompt: "hi", CPU: "not-a-number",
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want validation", err)
	}
}

func TestGetRunMirrorsCRStatus(t *testing.T) {
	svc, st, fl := newService(t)
	ctx := context.Background()
	seedProject(t, svc, &store.Project{Name: "p", Repo: "x/y"})
	run, err := svc.CreateRun(ctx, CreateRunRequest{Project: "p", User: "u@x", Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}

	// Operator writes back status.
	fl.SetStatus(run.Namespace, run.ID, wrenv1.AgentRunStatus{
		Phase:        wrenv1.PhaseRunning,
		RestartCount: 1,
		PR:           wrenv1.PRStatus{URL: "https://pr/1"},
	})

	got, err := svc.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != "Running" || got.RestartCount != 1 || got.PRURL != "https://pr/1" {
		t.Fatalf("mirrored run = %+v", got)
	}
	// Mirror is persisted to the store.
	stored, _ := st.GetRun(ctx, run.ID)
	if stored.Phase != "Running" {
		t.Errorf("store not updated: %+v", stored)
	}
}

func TestGetRunCRGoneReturnsStore(t *testing.T) {
	svc, _, fl := newService(t)
	ctx := context.Background()
	seedProject(t, svc, &store.Project{Name: "p", Repo: "x/y"})
	run, _ := svc.CreateRun(ctx, CreateRunRequest{Project: "p", User: "u@x", Prompt: "hi"})
	_ = fl.DeleteRun(ctx, run.Namespace, run.ID) // CR gone

	got, err := svc.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun after CR delete = %v", err)
	}
	if got.Phase != string(wrenv1.PhasePending) {
		t.Errorf("expected last-known Pending, got %q", got.Phase)
	}
}

func TestGetRunNotFound(t *testing.T) {
	svc, _, _ := newService(t)
	if _, err := svc.GetRun(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestListRunsScope(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()
	seedProject(t, svc, &store.Project{Name: "p", Repo: "x/y"})
	svc.idgen = func() string { return "r-a" }
	_, _ = svc.CreateRun(ctx, CreateRunRequest{Project: "p", User: "alice@x", Prompt: "1"})
	svc.idgen = func() string { return "r-b" }
	_, _ = svc.CreateRun(ctx, CreateRunRequest{Project: "p", User: "bob@x", Prompt: "2"})

	mine, _ := svc.ListRuns(ctx, "mine", "alice@x")
	if len(mine) != 1 || mine[0].User != "alice@x" {
		t.Fatalf("scope mine = %+v", mine)
	}
	all, _ := svc.ListRuns(ctx, "all", "alice@x")
	if len(all) != 2 {
		t.Fatalf("scope all = %d, want 2", len(all))
	}
}

// TestReconcileFromCluster simulates a restarted apiserver with an empty store
// (the durability hole WS-3 closes): the AgentRun CR still exists in the
// cluster, so reconcile-on-boot must re-learn the run with the CR's phase.
func TestReconcileFromCluster(t *testing.T) {
	svc, _, fl := newService(t)
	ctx := context.Background()
	seedProject(t, svc, &store.Project{Name: "p", Repo: "x/y"})
	run, err := svc.CreateRun(ctx, CreateRunRequest{Project: "p", User: "u@x", Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	// Operator advanced the run; then the store is wiped (fresh backing store on
	// restart / mid-migration). The CR remains in the cluster.
	fl.SetStatus(run.Namespace, run.ID, wrenv1.AgentRunStatus{Phase: wrenv1.PhaseRunning, RestartCount: 2})
	// Wipe the store by constructing a new one and re-pointing the service at
	// it (a fresh backing store on restart / mid-migration). The CR remains.
	fresh := store.NewMemory()
	svc.store = fresh

	if _, err := fresh.GetRun(ctx, run.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("precondition: fresh store should not know the run: %v", err)
	}

	n, err := svc.ReconcileFromCluster(ctx)
	if err != nil {
		t.Fatalf("ReconcileFromCluster: %v", err)
	}
	if n != 1 {
		t.Fatalf("reconciled %d runs, want 1", n)
	}
	got, err := fresh.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("run not re-learned: %v", err)
	}
	if got.Phase != string(wrenv1.PhaseRunning) || got.RestartCount != 2 {
		t.Errorf("re-learned run = %+v, want phase Running restart 2", got)
	}
	if got.Project != "p" || got.User != "u@x" || got.Prompt != "hi" {
		t.Errorf("re-learned run lost spec fields: %+v", got)
	}
	// Idempotent: a second boot must not error or duplicate.
	if _, err := svc.ReconcileFromCluster(ctx); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	all, _ := fresh.ListRuns(ctx, store.RunFilter{})
	if len(all) != 1 {
		t.Errorf("reconcile not idempotent: %d runs", len(all))
	}
}

// TestReconcilePreservesPhaseWhenCRHasNoStatus covers the restart-with-no-status
// case (the manual durability check): an unstarted run's CR carries empty
// status, so reconcile must keep the store's last-known phase rather than blank
// it. This mirrors GetRun's "only overwrite when the CR carries a value" rule.
func TestReconcilePreservesPhaseWhenCRHasNoStatus(t *testing.T) {
	svc, st, _ := newService(t)
	ctx := context.Background()
	seedProject(t, svc, &store.Project{Name: "p", Repo: "x/y"})
	run, err := svc.CreateRun(ctx, CreateRunRequest{Project: "p", User: "u@x", Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	// The CR in the Fake has empty status (operator never ran). The store row is
	// the CreateRun-set "Pending". Reconcile must not blank it.
	if _, err := svc.ReconcileFromCluster(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != string(wrenv1.PhasePending) {
		t.Errorf("reconcile blanked a known phase: got %q, want Pending", got.Phase)
	}
}

func TestSanitizeLabel(t *testing.T) {
	cases := map[string]string{
		"Arpeet@Corp.com": "arpeet-corp-com",
		"":                "anon",
		"---":             "anon",
		"UPPER":           "upper",
	}
	for in, want := range cases {
		if got := sanitizeLabel(in); got != want {
			t.Errorf("sanitizeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGenRunIDFormat(t *testing.T) {
	id := genRunID()
	if len(id) != 10 || id[:2] != "r-" {
		t.Errorf("genRunID = %q", id)
	}
}
