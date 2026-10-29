package main

// Client-credentials token for the observer's MCP endpoint.
//
// WHY THIS EXISTS. The observer's /mcp is behind a JWT: its middleware chain is
// auth401 -> jwt, and the live observer-auth-config recognises a service
// account by the client_id claim. Reaching it without a token returns
// 401 MISSING_TOKEN, which is exactly where this service sat once the gateway
// route and the network policy in front of it were fixed.
//
// The MCP federation trait cannot supply this. Its target configuration carries
// only name/host/port/path/protocol with no credential field, so the token has
// to come from the client.
//
// Mirrors observer-kiosk-proxy's KioskToken deliberately: same grant, same
// refresh-before-expiry shape. That proxy is the one component already trusted
// to hold this credential, so following it rather than inventing a second
// pattern keeps one thing to audit instead of two. Stdlib only, like the rest
// of this service.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type clientCredsToken struct {
	tokenURL, clientID, clientSecret string
	http                             *http.Client

	mu    sync.Mutex
	value string
	exp   time.Time
}

func newClientCredsToken(tokenURL, clientID, clientSecret string) *clientCredsToken {
	return &clientCredsToken{
		tokenURL:     tokenURL,
		clientID:     clientID,
		clientSecret: clientSecret,
		http:         &http.Client{Timeout: 20 * time.Second},
	}
}

// get returns a cached token, refreshing a minute before expiry. The lock spans
// the fetch, not just the expiry check: several questions can be answered
// concurrently, and an unlocked check would mint one token per caller.
func (t *clientCredsToken) get(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.value != "" && time.Now().Before(t.exp.Add(-time.Minute)) {
		return t.value, nil
	}

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {t.clientID},
		"client_secret": {t.clientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := t.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	// The status alone, never the body: a token endpoint's error body can echo
	// the request, and this error is surfaced through the API and the logs.
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint: http %d", resp.StatusCode)
	}

	var payload struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if payload.AccessToken == "" {
		return "", fmt.Errorf("token endpoint returned no access_token")
	}

	t.value = payload.AccessToken
	secs := payload.ExpiresIn
	if secs <= 0 {
		secs = 3600
	}
	t.exp = time.Now().Add(time.Duration(secs) * time.Second)
	return t.value, nil
}
