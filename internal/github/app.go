package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// AppCredentials are the GitHub App identity used to mint per-run installation
// tokens scoped to a single repo (spec §5.7). The private key stays in the
// control plane / Secret Manager and never enters the agent runner.
type AppCredentials struct {
	AppID          int64
	InstallationID int64
	PrivateKeyPEM  []byte
}

// InstallationToken mints a short-lived installation access token. baseURL
// defaults to the public GitHub API; httpClient may be nil.
func (a AppCredentials) InstallationToken(ctx context.Context, baseURL string, httpClient *http.Client) (string, time.Time, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM(a.PrivateKeyPEM)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("parse app private key: %w", err)
	}
	now := time.Now()
	appJWT, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now.Add(-30 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
		Issuer:    fmt.Sprintf("%d", a.AppID),
	}).SignedString(key)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign app jwt: %w", err)
	}

	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", strings.TrimRight(baseURL, "/"), a.InstallationID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+appJWT)
	httpReq.Header.Set("Accept", "application/vnd.github+json")

	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", time.Time{}, fmt.Errorf("mint installation token: unexpected status %s", resp.Status)
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", time.Time{}, err
	}
	if out.Token == "" {
		return "", time.Time{}, fmt.Errorf("mint installation token: empty token in response")
	}
	return out.Token, out.ExpiresAt, nil
}
