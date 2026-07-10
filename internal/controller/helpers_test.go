package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	wrenv1 "github.com/summiteight/wren/api/v1alpha1"
)

func TestClassifyTermination(t *testing.T) {
	cases := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{
			name: "oom",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name:  ContainerHarness,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}},
			}}}},
			want: "OOMKilled",
		},
		{
			name: "nonzero exit",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name:  ContainerHarness,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2}},
			}}}},
			want: "harness exit 2",
		},
		{
			name: "init container failure",
			pod: &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{
				Name:  InitHydrate,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
			}}}},
			want: "hydrate exit 1",
		},
		{
			name: "evicted",
			pod:  &corev1.Pod{Status: corev1.PodStatus{Reason: "Evicted"}},
			want: "Evicted",
		},
		{
			name: "unknown",
			pod:  &corev1.Pod{Status: corev1.PodStatus{}},
			want: "unknown failure",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyTermination(tc.pod); got != tc.want {
				t.Errorf("classifyTermination = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIsTerminal(t *testing.T) {
	terminal := []wrenv1.RunPhase{wrenv1.PhaseSucceeded, wrenv1.PhaseFailed, wrenv1.PhaseCanceled}
	for _, p := range terminal {
		if !isTerminal(p) {
			t.Errorf("%q should be terminal", p)
		}
	}
	for _, p := range []wrenv1.RunPhase{wrenv1.PhaseRunning, wrenv1.PhasePending, wrenv1.PhaseInterrupted, ""} {
		if isTerminal(p) {
			t.Errorf("%q should not be terminal", p)
		}
	}
}

func TestReadyStatus(t *testing.T) {
	if readyStatus(wrenv1.PhaseRunning) != metav1.ConditionTrue {
		t.Error("Running should be ready-true")
	}
	if readyStatus(wrenv1.PhasePending) != metav1.ConditionFalse {
		t.Error("Pending should be ready-false")
	}
}

func TestCheckpointIntervalDefault(t *testing.T) {
	run := testRun()
	run.Spec.Workspace.Checkpoint.IntervalSeconds = 0
	if got := checkpointInterval(run); got != defaultCheckpointInterval {
		t.Errorf("default interval = %d, want %d", got, defaultCheckpointInterval)
	}
	run.Spec.Workspace.Checkpoint.IntervalSeconds = 45
	if got := checkpointInterval(run); got != 45 {
		t.Errorf("interval = %d, want 45", got)
	}
}

func TestJoinAllowlist(t *testing.T) {
	if got := joinAllowlist(nil); got != "" {
		t.Errorf("empty = %q", got)
	}
	if got := joinAllowlist([]string{"a.com"}); got != "a.com" {
		t.Errorf("single = %q", got)
	}
	if got := joinAllowlist([]string{"a.com", "b.com"}); got != "a.com,b.com" {
		t.Errorf("multi = %q", got)
	}
}

func TestBuildRunSpecMCPPath(t *testing.T) {
	r := &AgentRunReconciler{}
	run := testRun()
	rs := r.buildRunSpec(run)
	if rs.MCPConfigPath != "" {
		t.Errorf("no MCP configured, path should be empty, got %q", rs.MCPConfigPath)
	}
	run.Spec.MCP.ConfigRef = "mcp-secret"
	rs = r.buildRunSpec(run)
	if rs.MCPConfigPath == "" {
		t.Error("MCP configured, path should be set")
	}
	if rs.Mode != "start" || rs.RunID != "r-abc" {
		t.Errorf("unexpected runspec: %+v", rs)
	}
}

func TestGitHubTokenInjection(t *testing.T) {
	run := testRun()
	pod := buildAgentPod(run, testImages, "wren-github-token")

	hasTokenEnv := func(c *corev1.Container) bool {
		for _, e := range c.Env {
			if e.Name == "GITHUB_TOKEN" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil &&
				e.ValueFrom.SecretKeyRef.Name == "wren-github-token" && e.ValueFrom.SecretKeyRef.Key == "token" {
				return true
			}
		}
		return false
	}
	if !hasTokenEnv(&pod.Spec.Containers[0]) {
		t.Error("harness missing GITHUB_TOKEN secret env")
	}
	if !hasTokenEnv(containerByName(pod.Spec.InitContainers, InitHydrate)) {
		t.Error("hydrate missing GITHUB_TOKEN secret env")
	}

	// Empty secret name → no injection.
	plain := buildAgentPod(run, testImages, "")
	if hasTokenEnv(&plain.Spec.Containers[0]) {
		t.Error("token env injected despite empty secret name")
	}
}

func TestSanitizeRef(t *testing.T) {
	cases := map[string]string{
		"arpeet@corp.com": "arpeet-corp.com",
		"arpeet":          "arpeet",
		"":                "user",
		"@@@":             "user",
	}
	for in, want := range cases {
		if got := sanitizeRef(in); got != want {
			t.Errorf("sanitizeRef(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildAgentPodMCPVolume(t *testing.T) {
	run := testRun()
	run.Spec.MCP.ConfigRef = "mcp-secret"
	pod := buildAgentPod(run, testImages, "")
	found := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == VolumeMCP && v.Secret != nil && v.Secret.SecretName == "mcp-secret" {
			found = true
		}
	}
	if !found {
		t.Error("expected MCP secret volume when configRef set")
	}
}
