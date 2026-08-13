package egress

import (
	"encoding/base64"
	"net/http"
)

// Authorizer injects a credential into an outbound request. Implementations are
// applied by the proxy on the way to an upstream, so the credential never lives
// in the untrusted runner (spec: egress and finalization).
type Authorizer interface {
	Apply(*http.Request)
}

// BasicAuth injects an HTTP Basic Authorization header (used for git over HTTPS:
// GitHub accepts the token as the password with username "x-access-token").
type BasicAuth struct {
	Username string
	Password string
}

func (b BasicAuth) Apply(r *http.Request) {
	if b.Password == "" {
		return
	}
	cred := base64.StdEncoding.EncodeToString([]byte(b.Username + ":" + b.Password))
	r.Header.Set("Authorization", "Basic "+cred)
}

// HeaderAuth injects a fixed header (e.g. Anthropic's x-api-key).
type HeaderAuth struct {
	Key   string
	Value string
}

func (h HeaderAuth) Apply(r *http.Request) {
	if h.Value != "" {
		r.Header.Set(h.Key, h.Value)
	}
}

// HeaderAuths applies several fixed headers. It is used by the trusted proxy's
// control-plane route to bind both the cluster credential and the immutable run
// identity; the gateway itself receives neither secret nor authority to choose
// another run ID.
type HeaderAuths []HeaderAuth

func (hs HeaderAuths) Apply(r *http.Request) {
	for _, h := range hs {
		h.Apply(r)
	}
}
