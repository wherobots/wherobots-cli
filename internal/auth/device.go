// Package auth implements browser-based sign-in for the CLI using the OAuth
// 2.0 Device Authorization Grant (RFC 8628) against WorkOS AuthKit, plus the
// on-disk session store and the request-time credential resolver.
//
// The resulting access token is the same WorkOS bearer the Wherobots API
// accepts on every REST route, so no token-exchange step is needed. This
// mirrors the VS Code extension's OAuth implementation (src/auth/ there);
// semantics kept in sync: scopes, refresh-token rotation, invalid_grant
// handling, and the 2-minute expiry skew.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"wherobots/cli/internal/httpclient"
)

// Scopes requested at sign-in; offline_access yields the refresh token.
const oauthScopes = "openid profile email offline_access"

const deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

// Sentinel errors mapped from RFC 8628 / OAuth error codes.
var (
	ErrAccessDenied = errors.New("sign-in was declined in the browser")
	ErrExpiredToken = errors.New("the confirmation code expired before sign-in was approved")
	ErrInvalidGrant = errors.New("the stored session is no longer valid")
)

// Client drives the device-authorization flow against one AuthKit tenant.
type Client struct {
	Domain   string // e.g. https://login.cloud.wherobots.com
	ClientID string
	HTTP     *http.Client

	// sleep is a test seam; nil means a real context-aware sleep.
	sleep func(ctx context.Context, d time.Duration) error

	endpoints *Endpoints
}

// Endpoints are the two AuthKit URLs the flow needs.
type Endpoints struct {
	DeviceAuthorization string `json:"device_authorization_endpoint"`
	Token               string `json:"token_endpoint"`
}

// DeviceAuthorization is the RFC 8628 device-authorization response.
type DeviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// TokenSet is the outcome of a successful token or refresh grant.
type TokenSet struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// Endpoints resolves the tenant's endpoints via OIDC discovery, falling back
// to the conventional AuthKit paths when discovery is unavailable. The result
// is cached on the client.
func (c *Client) Endpoints(ctx context.Context) (Endpoints, error) {
	if c.endpoints != nil {
		return *c.endpoints, nil
	}

	fallback := Endpoints{
		DeviceAuthorization: c.Domain + "/oauth2/device_authorization",
		Token:               c.Domain + "/oauth2/token",
	}

	discovered, err := c.discover(ctx)
	if err != nil || discovered.DeviceAuthorization == "" || discovered.Token == "" {
		c.endpoints = &fallback
		return fallback, nil
	}
	c.endpoints = &discovered
	return discovered, nil
}

func (c *Client) discover(ctx context.Context) (Endpoints, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Domain+"/.well-known/openid-configuration", nil)
	if err != nil {
		return Endpoints{}, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Endpoints{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Endpoints{}, fmt.Errorf("discovery returned HTTP %d", resp.StatusCode)
	}

	var endpoints Endpoints
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&endpoints); err != nil {
		return Endpoints{}, err
	}
	return endpoints, nil
}

// StartDeviceAuthorization requests a device code and user confirmation code.
func (c *Client) StartDeviceAuthorization(ctx context.Context) (*DeviceAuthorization, error) {
	endpoints, err := c.Endpoints(ctx)
	if err != nil {
		return nil, err
	}

	form := url.Values{
		"client_id": {c.ClientID},
		"scope":     {oauthScopes},
	}
	body, err := c.postForm(ctx, endpoints.DeviceAuthorization, form)
	if err != nil {
		return nil, fmt.Errorf("start sign-in: %w", err)
	}

	var da DeviceAuthorization
	if err := json.Unmarshal(body, &da); err != nil {
		return nil, fmt.Errorf("start sign-in: parse response: %w", err)
	}
	if da.DeviceCode == "" || da.UserCode == "" {
		return nil, fmt.Errorf("start sign-in: response missing device_code/user_code")
	}
	return &da, nil
}

// PollForToken polls the token endpoint until the user approves the sign-in,
// declines it, or the device code expires. It honors the server-provided
// interval, adds 5s on slow_down per RFC 8628 §3.5, and returns early on
// context cancellation.
func (c *Client) PollForToken(ctx context.Context, da *DeviceAuthorization) (*TokenSet, error) {
	endpoints, err := c.Endpoints(ctx)
	if err != nil {
		return nil, err
	}

	interval := da.Interval
	if interval <= 0 {
		interval = 5
	}
	deadline := time.Now().Add(time.Duration(da.ExpiresIn) * time.Second)

	form := url.Values{
		"grant_type":  {deviceGrantType},
		"device_code": {da.DeviceCode},
		"client_id":   {c.ClientID},
	}

	// A transient network hiccup mid-poll shouldn't abort the sign-in and
	// force the user to start over with a new code; tolerate a few
	// consecutive failures before giving up.
	const maxConsecutivePollFailures = 3
	failures := 0

	for {
		if err := c.sleepCtx(ctx, time.Duration(interval)*time.Second); err != nil {
			return nil, err
		}

		tokens, oauthErr, err := c.tokenGrant(ctx, endpoints.Token, form)
		if err != nil {
			failures++
			var refused *httpclient.RefusedRedirectError
			if ctx.Err() != nil || failures >= maxConsecutivePollFailures || errors.As(err, &refused) {
				return nil, fmt.Errorf("poll for sign-in: %w", err)
			}
		} else if oauthErr == nil {
			return tokens, nil
		} else {
			failures = 0
			switch oauthErr.Error {
			case "authorization_pending":
				// keep polling
			case "slow_down":
				interval += 5
			case "access_denied":
				return nil, ErrAccessDenied
			case "expired_token":
				return nil, ErrExpiredToken
			default:
				return nil, oauthErrorf("poll for sign-in", oauthErr)
			}
		}

		// The server's expired_token response is the primary expiry signal;
		// this client-side deadline is a guard for a slow or unresponsive
		// server. It is checked after each poll, so one extra poll may occur
		// once the deadline passes mid-sleep — harmless.
		if da.ExpiresIn > 0 && time.Now().After(deadline) {
			return nil, ErrExpiredToken
		}
	}
}

// Refresh redeems a refresh token. AuthKit rotates refresh tokens: callers
// must persist the returned RefreshToken. clientID is the client the token
// was issued to (from the stored session, so a change to the baked-in
// default can't orphan old sessions); tokenEndpoint and clientID may be
// empty (older stored sessions) and fall back to the client's defaults.
func (c *Client) Refresh(ctx context.Context, tokenEndpoint, clientID, refreshToken string) (*TokenSet, error) {
	if tokenEndpoint == "" {
		tokenEndpoint = c.Domain + "/oauth2/token"
	}
	if clientID == "" {
		clientID = c.ClientID
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
	}
	tokens, oauthErr, err := c.tokenGrant(ctx, tokenEndpoint, form)
	if err != nil {
		return nil, fmt.Errorf("refresh session: %w", err)
	}
	if oauthErr != nil {
		if oauthErr.Error == "invalid_grant" {
			return nil, ErrInvalidGrant
		}
		return nil, oauthErrorf("refresh session", oauthErr)
	}
	return tokens, nil
}

// postForm posts a form and returns the raw body of a 2xx response, or an
// error carrying the OAuth error code for non-2xx.
func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var parsed tokenResponse
		if json.Unmarshal(body, &parsed) == nil && parsed.Error != "" {
			return nil, oauthErrorf(fmt.Sprintf("HTTP %d", resp.StatusCode), &oauthError{Error: parsed.Error, Desc: parsed.ErrorDesc})
		}
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return body, nil
}

type oauthError struct {
	Error string
	Desc  string
}

// tokenGrant posts to the token endpoint. It returns exactly one of: a token
// set, a structured OAuth error (4xx with an error code), or a transport /
// server error.
func (c *Client) tokenGrant(ctx context.Context, endpoint string, form url.Values) (*TokenSet, *oauthError, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, err
	}

	var parsed tokenResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, nil, fmt.Errorf("HTTP %d: unparseable token response", resp.StatusCode)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 && parsed.AccessToken != "" {
		return c.toTokenSet(parsed), nil, nil
	}
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && parsed.Error != "" {
		return nil, &oauthError{Error: parsed.Error, Desc: parsed.ErrorDesc}, nil
	}
	return nil, nil, fmt.Errorf("token endpoint returned HTTP %d", resp.StatusCode)
}

// toTokenSet computes expiry from expires_in, falling back to the JWT exp
// claim, then a conservative 5 minutes.
func (c *Client) toTokenSet(parsed tokenResponse) *TokenSet {
	expiresAt := time.Now().Add(5 * time.Minute)
	if parsed.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(parsed.ExpiresIn) * time.Second)
	} else if claims := DecodeClaims(parsed.AccessToken); claims.Exp > 0 {
		expiresAt = time.Unix(claims.Exp, 0)
	}
	return &TokenSet{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		ExpiresAt:    expiresAt,
	}
}

func (c *Client) sleepCtx(ctx context.Context, d time.Duration) error {
	if c.sleep != nil {
		return c.sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func oauthErrorf(action string, oauthErr *oauthError) error {
	if oauthErr.Desc != "" {
		return fmt.Errorf("%s: %s: %s", action, oauthErr.Error, oauthErr.Desc)
	}
	return fmt.Errorf("%s: %s", action, oauthErr.Error)
}
