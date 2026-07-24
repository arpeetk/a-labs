package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/summiteight/wren/internal/install"
)

// newInstallCmd wires `wren install`: stand the control plane up on an existing
// cluster (GKE via --registry, local eval via --kind). The flags only collect
// decisions; the flow lives in internal/install (WS-13).
func newInstallCmd() *cobra.Command {
	var opts install.Options
	var kubeContext string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the Wren control plane onto a Kubernetes cluster",
		Long: "Install CRDs, RBAC, and the operator + apiserver Deployments onto a cluster,\n" +
			"build and deliver the images, store the agent credentials as proxy Secrets,\n" +
			"and wait for the control plane to become Ready.\n\n" +
			"  GKE (real cluster):  wren install --registry us-central1-docker.pkg.dev/PROJ/wren\n" +
			"  kind (local eval):   wren install --kind wren-eval",
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.KubeContext = kubeContext
			// Env is the non-interactive credential path; GITHUB_TOKEN falls
			// back to `gh auth token` inside the installer.
			if opts.GitHubToken == "" {
				opts.GitHubToken = os.Getenv("GITHUB_TOKEN")
			}
			if opts.AnthropicKey == "" {
				opts.AnthropicKey = os.Getenv("ANTHROPIC_API_KEY")
			}
			in, err := install.New(installKubeContext(opts), cmd.OutOrStdout())
			if err != nil {
				return err
			}
			// Only prompt on a real terminal; scripts must not hang (and the
			// prompt never echoes — term.ReadPassword).
			if term.IsTerminal(int(os.Stdin.Fd())) {
				in.PromptSecret = promptSecret
			}
			return in.Install(context.Background(), opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&kubeContext, "kube-context", "", "kubectl context to install into (default: current; kind installs default to kind-<name>)")
	f.StringVar(&opts.KindCluster, "kind", "", "local eval: create/reuse this kind cluster and load images into it")
	f.StringVar(&opts.Registry, "registry", "", "image prefix to build (linux/amd64), push, and point the control plane at (e.g. an Artifact Registry path)")
	f.StringVar(&opts.ImageTag, "tag", "", "image tag for --registry pushes (default: source tree's short git SHA, else \"dev\")")
	f.StringVar(&opts.SrcDir, "src", ".", "repo checkout to build images from")
	f.StringVar(&opts.Expose, "expose", "", "expose the apiserver Service as this type (LoadBalancer) for team setups; default stays port-forward-only")
	f.StringVar(&opts.RunNamespace, "run-namespace", "wren-runs", "namespace for the proxy credential Secrets; credentialed projects point their namespace here")
	f.BoolVar(&opts.SkipCredentials, "skip-credentials", false, "do not collect GITHUB_TOKEN/ANTHROPIC_API_KEY (keyless eval)")
	return cmd
}

// newUninstallCmd wires `wren uninstall`: remove the install (namespaces +
// CRDs). Destructive and gated behind --confirm — deleting the CRDs deletes
// every AgentRun cluster-wide.
func newUninstallCmd() *cobra.Command {
	var opts install.UninstallOptions
	var kubeContext string
	var confirm bool
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the Wren control plane (namespaces + CRDs) from the cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !confirm {
				return fmt.Errorf("uninstall removes namespaces %s, %s, all Wren CRDs (every AgentRun goes with them) and the cluster RBAC — re-run with --confirm to proceed",
					"wren-system", opts.RunNamespace)
			}
			opts.KubeContext = kubeContext
			in, err := install.New(kubeContext, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			return in.Uninstall(context.Background(), opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&kubeContext, "kube-context", "", "kubectl context (default: current)")
	f.StringVar(&opts.RunNamespace, "run-namespace", "wren-runs", "run namespace to remove (must match install's)")
	f.BoolVar(&confirm, "confirm", false, "confirm the destructive uninstall")
	return cmd
}

// installKubeContext resolves the context install.New should load: explicit
// --kube-context wins; a kind install defaults to kind-<name> (which the
// installer may have just created).
func installKubeContext(opts install.Options) string {
	if opts.KubeContext != "" {
		return opts.KubeContext
	}
	if opts.KindCluster != "" {
		return "kind-" + opts.KindCluster
	}
	return ""
}

// promptSecret reads a credential from the terminal with echo off. The value
// exists only in the returned string, which flows straight into a Secret write.
func promptSecret(label string) (string, error) {
	fmt.Fprintf(os.Stderr, "%s: ", label)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
