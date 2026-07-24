package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

var (
	// SchemeBuilder registers the Wren types with a runtime.Scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the Wren types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// addKnownTypes registers the Wren types with a scheme. Written against plain
// apimachinery (not controller-runtime's deprecated scheme.Builder) so this
// api package keeps the minimal dependency footprint the type meta wants.
func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&AgentRun{}, &AgentRunList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
