package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	wrenv1 "github.com/summiteight/wren/api/v1alpha1"
)

func termPod(name string, code int32, reason string) *corev1.Pod {
	return &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		Name:  name,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Reason: reason}},
	}}}}
}

func TestClassifyTermination(t *testing.T) {
	cases := []struct {
		name          string
		pod           *corev1.Pod
		wantReason    string
		wantRetryable bool
	}{
		{"oom", termPod(ContainerHarness, 137, "OOMKilled"), "OOMKilled", true},
		{"harness clean error is fatal", termPod(ContainerHarness, 2, ""), "harness exit 2", false},
		{"retryable exit code", termPod(ContainerHarness, 75, ""), "harness requested retry", true},
		{
			name: "init container failure is fatal",
			pod: &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{
				Name:  InitHydrate,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
			}}}},
			wantReason: "hydrate exit 1", wantRetryable: false,
		},
		{"evicted is retryable", &corev1.Pod{Status: corev1.PodStatus{Reason: "Evicted"}}, "Evicted", true},
		{"unknown is retryable", &corev1.Pod{Status: corev1.PodStatus{}}, "unknown failure", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyTermination(tc.pod)
			if got.reason != tc.wantReason || got.retryable != tc.wantRetryable {
				t.Errorf("classifyTermination = %+v, want {%q %v}", got, tc.wantReason, tc.wantRetryable)
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

func envValue(c *corev1.Container, name string) (corev1.EnvVar, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e, true
		}
	}
	return corev1.EnvVar{}, false
}

// The credential belongs on the egress-proxy, never the runner (spec §5.6): the
// harness/hydrate get only the proxy URL, and the token env lives on the proxy.
func TestCredentialsGoToEgressProxyNotRunner(t *testing.T) {
	run := testRun()
	pod := buildAgentPod(run, PodConfig{
		Images:             testImages,
		GitHubTokenSecret:  "wren-github-token",
		AnthropicKeySecret: "wren-anthropic-key",
		OpenAIKeySecret:    "wren-openai-key",
	})

	harness := &pod.Spec.Containers[0]
	hydrate := containerByName(pod.Spec.InitContainers, InitHydrate)
	proxy := containerByName(pod.Spec.InitContainers, ContainerEgressProxy)

	// Runner containers must NOT carry any credential.
	for _, c := range []*corev1.Container{harness, hydrate} {
		if _, ok := envValue(c, "GITHUB_TOKEN"); ok {
			t.Errorf("%s must not receive GITHUB_TOKEN", c.Name)
		}
		if _, ok := envValue(c, "ANTHROPIC_API_KEY"); ok {
			t.Errorf("%s must not receive ANTHROPIC_API_KEY", c.Name)
		}
		if _, ok := envValue(c, "OPENAI_API_KEY"); ok {
			t.Errorf("%s must not receive OPENAI_API_KEY", c.Name)
		}
		// ...but they are pointed at the proxy.
		if e, ok := envValue(c, "WREN_EGRESS_PROXY"); !ok || e.Value != "http://127.0.0.1:8099" {
			t.Errorf("%s WREN_EGRESS_PROXY = %q (ok=%v)", c.Name, e.Value, ok)
		}
	}
	if e, ok := envValue(harness, "ANTHROPIC_BASE_URL"); !ok || e.Value != "http://127.0.0.1:8099/anthropic" {
		t.Errorf("harness ANTHROPIC_BASE_URL = %q (ok=%v)", e.Value, ok)
	}
	// The codex adapter's provider route is wired the same way (WS-12).
	if e, ok := envValue(harness, "OPENAI_BASE_URL"); !ok || e.Value != "http://127.0.0.1:8099/openai" {
		t.Errorf("harness OPENAI_BASE_URL = %q (ok=%v)", e.Value, ok)
	}

	// The proxy carries the credentials via Secret refs.
	gh, ok := envValue(proxy, "GITHUB_TOKEN")
	if !ok || gh.ValueFrom == nil || gh.ValueFrom.SecretKeyRef == nil ||
		gh.ValueFrom.SecretKeyRef.Name != "wren-github-token" || gh.ValueFrom.SecretKeyRef.Key != "token" {
		t.Errorf("egress-proxy GITHUB_TOKEN secret ref wrong: %+v", gh)
	}
	ak, ok := envValue(proxy, "ANTHROPIC_API_KEY")
	if !ok || ak.ValueFrom == nil || ak.ValueFrom.SecretKeyRef == nil ||
		ak.ValueFrom.SecretKeyRef.Name != "wren-anthropic-key" || ak.ValueFrom.SecretKeyRef.Key != "key" {
		t.Errorf("egress-proxy ANTHROPIC_API_KEY secret ref wrong: %+v", ak)
	}
	oa, ok := envValue(proxy, "OPENAI_API_KEY")
	if !ok || oa.ValueFrom == nil || oa.ValueFrom.SecretKeyRef == nil ||
		oa.ValueFrom.SecretKeyRef.Name != "wren-openai-key" || oa.ValueFrom.SecretKeyRef.Key != "key" {
		t.Errorf("egress-proxy OPENAI_API_KEY secret ref wrong: %+v", oa)
	}

	// No secrets configured → proxy has no credential envs.
	plain := buildAgentPod(run, PodConfig{Images: testImages})
	plainProxy := containerByName(plain.Spec.InitContainers, ContainerEgressProxy)
	if _, ok := envValue(plainProxy, "GITHUB_TOKEN"); ok {
		t.Error("GITHUB_TOKEN injected despite empty secret name")
	}
	if _, ok := envValue(plainProxy, "OPENAI_API_KEY"); ok {
		t.Error("OPENAI_API_KEY injected despite empty secret name")
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
	pod := buildAgentPod(run, PodConfig{Images: testImages})
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
