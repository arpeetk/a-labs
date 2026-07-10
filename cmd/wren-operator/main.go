// Command wren-operator is the Kubernetes controller that reconciles Wren
// AgentRun and AgentPool resources into hardened agent pods.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
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
		images                 controller.Images
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "metrics endpoint address")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health probe address")
	flag.BoolVar(&leaderElect, "leader-elect", false, "enable leader election for HA")
	flag.StringVar(&images.Runtime, "runtime-image", "wren/runtime:dev", "wren-runtime image for in-pod sidecar/init roles")
	var githubTokenSecret string
	flag.StringVar(&githubTokenSecret, "github-token-secret", "wren-github-token", "Secret (key \"token\") injected as GITHUB_TOKEN into the runner; empty to disable")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

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

	if err := (&controller.AgentRunReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Images:            images,
		GitHubTokenSecret: githubTokenSecret,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up AgentRun controller")
		os.Exit(1)
	}
	if err := (&controller.AgentPoolReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up AgentPool controller")
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
