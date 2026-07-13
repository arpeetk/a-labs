package v1alpha1

import "sigs.k8s.io/controller-runtime/pkg/scheme"

var (
	// SchemeBuilder registers the Wren types with a runtime.Scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the Wren types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(
		&AgentRun{}, &AgentRunList{},
		&AgentPool{}, &AgentPoolList{},
	)
}
