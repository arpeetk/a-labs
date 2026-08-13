// Command wren-operator is the Kubernetes controller that reconciles Wren
// AgentRun resources into hardened agent pods.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	wrenv1 "github.com/summiteight/wren/api/v1alpha1"
	"github.com/summiteight/wren/internal/controller"
)

var scheme = runtime.NewScheme()

func init() {
	must(clientgoscheme.AddToScheme(scheme))
	must(wrenv1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr, probeAddr string
		leaderElect            bool
		podCfg                 controller.PodConfig
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "metrics endpoint address")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health probe address")
	flag.BoolVar(&leaderElect, "leader-elect", false, "enable leader election for HA")
	flag.StringVar(&podCfg.Images.Runtime, "runtime-image", "wren/runtime:dev", "wren-runtime image for in-pod sidecar/init roles")
	flag.StringVar(&podCfg.GitHubTokenSecret, "github-token-secret", "wren-github-token", "Secret (key \"token\") injected as GITHUB_TOKEN into the egress-proxy; empty to disable")
	flag.StringVar(&podCfg.AnthropicKeySecret, "anthropic-key-secret", "wren-anthropic-key", "Secret (key \"key\") injected as ANTHROPIC_API_KEY into the egress-proxy; empty to disable")
	flag.StringVar(&podCfg.OpenAIKeySecret, "openai-key-secret", "wren-openai-key", "Secret (key \"key\") injected as OPENAI_API_KEY into the egress-proxy; empty to disable")
	flag.StringVar(&podCfg.GatewayTokenSecret, "gateway-token-secret", "wren-gateway-token", "Secret (key \"token\") injected into the trusted egress-proxy for event delivery; empty to disable")
	flag.StringVar(&podCfg.ControlPlaneURL, "control-plane-url", "", "in-cluster apiserver origin for gateway events (default http://wren-apiserver.wren-system.svc:8090)")
	flag.StringVar(&podCfg.EgressPort, "egress-port", "", "egress-proxy localhost port (default 8099)")
	var egressEnforcement string
	flag.StringVar(&egressEnforcement, "egress-enforcement", string(controller.EgressEnforcementIptables),
		"egress bypass enforcement: iptables (privileged lockdown init container, default) | off (escape hatch for clusters that forbid privileged init containers, e.g. GKE Autopilot)")
	flag.BoolVar(&podCfg.CheckpointGCSMount, "checkpoint-gcs-mount", false,
		"mount checkpoint buckets into trusted checkpoint/restore containers via GKE Cloud Storage FUSE; requires the CSI addon and --checkpoint-ksa Workload Identity binding")
	flag.StringVar(&podCfg.CheckpointKSA, "checkpoint-ksa", controller.DefaultCheckpointKSA,
		"Kubernetes ServiceAccount (Workload-Identity-bound to a GCP SA with objectAdmin on the checkpoint bucket) applied to pods with --checkpoint-gcs-mount enabled")
	flag.StringVar(&podCfg.CheckpointLocalPath, "checkpoint-local-path", "",
		"development/test only: existing absolute node directory mounted into trusted checkpoint containers (single-node kind; not node-durable)")
	flag.StringVar(&podCfg.MockDelay, "mock-delay", "", "development/test only: delay deterministic mock harness completion (for example 2m)")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	if podCfg.CheckpointLocalPath != "" && (!filepath.IsAbs(podCfg.CheckpointLocalPath) || filepath.Clean(podCfg.CheckpointLocalPath) == string(filepath.Separator)) {
		fmt.Fprintf(os.Stderr, "wren-operator: invalid --checkpoint-local-path %q (want a non-root absolute path)\n", podCfg.CheckpointLocalPath)
		os.Exit(1)
	}
	if podCfg.MockDelay != "" {
		d, err := time.ParseDuration(podCfg.MockDelay)
		if err != nil || d <= 0 {
			fmt.Fprintf(os.Stderr, "wren-operator: invalid --mock-delay %q (want a positive duration)\n", podCfg.MockDelay)
			os.Exit(1)
		}
	}

	switch controller.EgressEnforcement(egressEnforcement) {
	case controller.EgressEnforcementIptables, controller.EgressEnforcementOff:
		podCfg.EgressEnforcement = controller.EgressEnforcement(egressEnforcement)
	default:
		// ctrl.Log has no sink until SetLogger runs below — a log line here is
		// silently discarded. Write to stderr so an invalid flag is not a
		// silent CrashLoopBackOff.
		fmt.Fprintf(os.Stderr, "wren-operator: invalid --egress-enforcement %q (want iptables|off)\n", egressEnforcement)
		os.Exit(1)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "wren-operator.wren.dev",
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// The controller-runtime client cannot read pod logs (a subresource), so a
	// typed clientset backs the terminal-event scrape into run status.
	cs, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to build clientset for log scraping")
		os.Exit(1)
	}

	if err := (&controller.AgentRunReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		PodConfig: podCfg,
		Logs:      controller.NewLogReader(cs),
		Executor:  controller.NewPodExecutor(mgr.GetConfig(), cs),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up AgentRun controller")
		os.Exit(1)
	}

	must(mgr.AddHealthzCheck("healthz", healthz.Ping))
	must(mgr.AddReadyzCheck("readyz", healthz.Ping))

	setupLog.Info("starting wren-operator")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited")
		os.Exit(1)
	}
}

func must(err error) {
	if err != nil {
		ctrl.Log.WithName("setup").Error(err, "startup error")
		os.Exit(1)
	}
}
