package egress

import (
	"encoding/base64"
	"net/http"
)

// Authorizer injects a credential into an outbound request. Implementations are
// applied by the proxy on the way to an upstream, so the credential never lives
// in the untrusted runner (spec §5.6/§5.7).
type Authorizer interface {
	Apply(*http.Request)
}

// NoAuth injects nothing.
type NoAuth struct{}

func (NoAuth) Apply(*http.Request) {}

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
