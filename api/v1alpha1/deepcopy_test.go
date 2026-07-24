package v1alpha1

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAgentRunDeepCopy(t *testing.T) {
	orig := &AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "r-1", Namespace: "ns"},
		Spec: AgentRunSpec{
			Project: "p",
			Harness: HarnessSpec{Kind: HarnessClaudeCode, Image: "img"},
			Task:    TaskSpec{Prompt: "do it", BaseRef: "main"},
			Sandbox: SandboxSpec{RuntimeClass: RuntimeRunc, Resources: ResourceSpec{
				CPU: resource.MustParse("1"), Memory: resource.MustParse("2Gi"), EphemeralDisk: resource.MustParse("5Gi"),
			}},
			Workspace: WorkspaceSpec{
				PVC:        PVCSpec{StorageClass: "regional-pd", Size: resource.MustParse("20Gi")},
				Checkpoint: CheckpointSpec{IntervalSeconds: 120, Bucket: "gs://b"},
			},
			MCP:    MCPSpec{ConfigRef: "mcp"},
			Egress: EgressSpec{Allowlist: []string{"github.com"}},
			Retry:  RetrySpec{MaxRestarts: 3, Backoff: "exponential"},
		},
		Status: AgentRunStatus{
			Phase:          PhaseRunning,
			RestartCount:   2,
			LastCheckpoint: &CheckpointRef{URI: "gs://b/ck", At: metav1.Now(), Commit: "abc"},
			SessionID:      "sess",
			PR:             PRStatus{URL: "http://pr", Branch: "wren/x"},
			Usage:          UsageStatus{InputTokens: 10, OutputTokens: 20, CostUSD: "0.05"},
			Conditions:     []metav1.Condition{{Type: "Ready", LastTransitionTime: metav1.Now()}},
		},
	}

	clone := orig.DeepCopy()
	if clone == orig {
		t.Fatal("DeepCopy returned same pointer")
	}
	if clone.Spec.Project != "p" || clone.Status.Phase != PhaseRunning {
		t.Fatal("DeepCopy lost data")
	}
	// Mutating the clone must not affect the original (deep, not shallow).
	clone.Spec.Egress.Allowlist[0] = "evil.com"
	if orig.Spec.Egress.Allowlist[0] != "github.com" {
		t.Fatal("DeepCopy shared slice backing array")
	}
	clone.Status.Conditions[0].Type = "Changed"
	if orig.Status.Conditions[0].Type != "Ready" {
		t.Fatal("DeepCopy shared conditions slice")
	}

	// DeepCopyObject returns a runtime.Object of the same kind.
	if _, ok := orig.DeepCopyObject().(*AgentRun); !ok {
		t.Fatal("DeepCopyObject wrong type")
	}

	list := &AgentRunList{Items: []AgentRun{*orig}}
	if got := list.DeepCopy(); len(got.Items) != 1 {
		t.Fatal("AgentRunList DeepCopy lost items")
	}
	if _, ok := list.DeepCopyObject().(*AgentRunList); !ok {
		t.Fatal("AgentRunList DeepCopyObject wrong type")
	}
}

// TestLeafDeepCopy exercises the generated DeepCopy() wrappers on the nested
// spec/status types (invoked directly rather than via the parent's DeepCopyInto)
// so the generated code is fully covered.
func TestLeafDeepCopy(t *testing.T) {
	q := resource.MustParse("1")
	_ = (&HarnessSpec{Kind: HarnessCodex, Image: "i"}).DeepCopy()
	_ = (&TaskSpec{Prompt: "p"}).DeepCopy()
	_ = (&ResourceSpec{CPU: q, Memory: q, EphemeralDisk: q}).DeepCopy()
	_ = (&SandboxSpec{Resources: ResourceSpec{CPU: q, Memory: q}}).DeepCopy()
	_ = (&PVCSpec{Size: q, StorageClass: "sc"}).DeepCopy()
	_ = (&CheckpointSpec{Bucket: "b"}).DeepCopy()
	_ = (&WorkspaceSpec{PVC: PVCSpec{Size: q}, Checkpoint: CheckpointSpec{Bucket: "b"}}).DeepCopy()
	_ = (&MCPSpec{ConfigRef: "m"}).DeepCopy()
	_ = (&EgressSpec{Allowlist: []string{"a"}}).DeepCopy()
	_ = (&RetrySpec{MaxRestarts: 1}).DeepCopy()
	_ = (&CheckpointRef{URI: "u"}).DeepCopy()
	_ = (&PRStatus{URL: "u"}).DeepCopy()
	_ = (&UsageStatus{InputTokens: 1}).DeepCopy()
	_ = (&AgentRunSpec{Egress: EgressSpec{Allowlist: []string{"a"}}}).DeepCopy()
	_ = (&AgentRunStatus{LastCheckpoint: &CheckpointRef{}, Conditions: []metav1.Condition{{}}}).DeepCopy()
	// Nil-receiver DeepCopy must return nil, not panic.
	var nilRun *AgentRun
	if nilRun.DeepCopy() != nil {
		t.Error("nil DeepCopy should be nil")
	}
}
