package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wherobots/cli/internal/config"
	"wherobots/cli/internal/httpclient"
)

// mockAuthKit serves OIDC discovery plus scripted device-authorization and
// token responses, mirroring the WorkOS AuthKit endpoints.
type mockAuthKit struct {
	srv            *httptest.Server
	deviceAuthForm url.Values
	tokenForms     []url.Values
	tokenResponses []mockResponse
	serveDiscovery bool
}

type mockResponse struct {
	status int
	body   map[string]any
}

func newMockAuthKit(t *testing.T, tokenResponses []mockResponse) *mockAuthKit {
	t.Helper()
	m := &mockAuthKit{tokenResponses: tokenResponses, serveDiscovery: true}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		if !m.serveDiscovery {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"device_authorization_endpoint": m.srv.URL + "/oauth2/device_authorization",
			"token_endpoint":                m.srv.URL + "/oauth2/token",
		})
	})
	mux.HandleFunc("/oauth2/device_authorization", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse device auth form: %v", err)
		}
		m.deviceAuthForm = r.PostForm
		writeJSON(w, http.StatusOK, map[string]any{
			"device_code":               "device-code-1",
			"user_code":                 "WDJB-MJHT",
			"verification_uri":          m.srv.URL + "/device",
			"verification_uri_complete": m.srv.URL + "/device?user_code=WDJB-MJHT",
			"expires_in":                299,
			"interval":                  5,
		})
	})
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token form: %v", err)
		}
		m.tokenForms = append(m.tokenForms, r.PostForm)
		if len(m.tokenResponses) == 0 {
			t.Errorf("unexpected extra token call")
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
			return
		}
		next := m.tokenResponses[0]
		m.tokenResponses = m.tokenResponses[1:]
		writeJSON(w, next.status, next.body)
	})

	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func writeJSON(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// newTestClient returns a Client against the mock with an instant sleep seam
// that records requested delays.
func newTestClient(m *mockAuthKit, sleeps *[]time.Duration) *Client {
	return &Client{
		Domain:   m.srv.URL,
		ClientID: "client_test_123",
		HTTP:     m.srv.Client(),
		sleep: func(ctx context.Context, d time.Duration) error {
			if sleeps != nil {
				*sleeps = append(*sleeps, d)
			}
			return ctx.Err()
		},
	}
}

func pendingResponse() mockResponse {
	return mockResponse{http.StatusBadRequest, map[string]any{"error": "authorization_pending"}}
}

func successResponse() mockResponse {
	return mockResponse{http.StatusOK, map[string]any{
		"access_token":  "access-1",
		"refresh_token": "refresh-1",
		"expires_in":    3600,
		"token_type":    "Bearer",
	}}
}

func TestStartDeviceAuthorizationEncodesRequest(t *testing.T) {
	t.Parallel()
	m := newMockAuthKit(t, nil)
	client := newTestClient(m, nil)

	da, err := client.StartDeviceAuthorization(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceAuthorization() error = %v", err)
	}

	if got := m.deviceAuthForm.Get("client_id"); got != "client_test_123" {
		t.Errorf("client_id = %q", got)
	}
	if got := m.deviceAuthForm.Get("scope"); got != "openid profile email offline_access" {
		t.Errorf("scope = %q", got)
	}
	if da.DeviceCode != "device-code-1" || da.UserCode != "WDJB-MJHT" {
		t.Errorf("unexpected device authorization: %+v", da)
	}
	if da.VerificationURIComplete == "" || da.Interval != 5 || da.ExpiresIn != 299 {
		t.Errorf("unexpected device authorization: %+v", da)
	}
}

func TestPollForTokenSuccessAfterPending(t *testing.T) {
	t.Parallel()
	m := newMockAuthKit(t, []mockResponse{pendingResponse(), pendingResponse(), successResponse()})
	var sleeps []time.Duration
	client := newTestClient(m, &sleeps)

	da := &DeviceAuthorization{DeviceCode: "device-code-1", Interval: 5, ExpiresIn: 299}
	before := time.Now()
	tokens, err := client.PollForToken(context.Background(), da)
	if err != nil {
		t.Fatalf("PollForToken() error = %v", err)
	}

	if tokens.AccessToken != "access-1" || tokens.RefreshToken != "refresh-1" {
		t.Errorf("unexpected tokens: %+v", tokens)
	}
	wantExpiry := before.Add(3600 * time.Second)
	if tokens.ExpiresAt.Before(wantExpiry.Add(-time.Minute)) || tokens.ExpiresAt.After(wantExpiry.Add(time.Minute)) {
		t.Errorf("ExpiresAt = %v, want ~%v", tokens.ExpiresAt, wantExpiry)
	}
	want := []time.Duration{5 * time.Second, 5 * time.Second, 5 * time.Second}
	if fmt.Sprint(sleeps) != fmt.Sprint(want) {
		t.Errorf("sleeps = %v, want %v", sleeps, want)
	}

	last := m.tokenForms[len(m.tokenForms)-1]
	if got := last.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:device_code" {
		t.Errorf("grant_type = %q", got)
	}
	if got := last.Get("device_code"); got != "device-code-1" {
		t.Errorf("device_code = %q", got)
	}
	if got := last.Get("client_id"); got != "client_test_123" {
		t.Errorf("client_id = %q", got)
	}
}

func TestPollForTokenSlowDownIncreasesInterval(t *testing.T) {
	t.Parallel()
	m := newMockAuthKit(t, []mockResponse{
		pendingResponse(),
		{http.StatusBadRequest, map[string]any{"error": "slow_down"}},
		successResponse(),
	})
	var sleeps []time.Duration
	client := newTestClient(m, &sleeps)

	da := &DeviceAuthorization{DeviceCode: "device-code-1", Interval: 5, ExpiresIn: 299}
	if _, err := client.PollForToken(context.Background(), da); err != nil {
		t.Fatalf("PollForToken() error = %v", err)
	}

	want := []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second}
	if fmt.Sprint(sleeps) != fmt.Sprint(want) {
		t.Errorf("sleeps = %v, want %v (slow_down must add 5s)", sleeps, want)
	}
}

func TestPollForTokenAccessDenied(t *testing.T) {
	t.Parallel()
	m := newMockAuthKit(t, []mockResponse{
		{http.StatusBadRequest, map[string]any{"error": "access_denied"}},
	})
	client := newTestClient(m, nil)

	_, err := client.PollForToken(context.Background(), &DeviceAuthorization{DeviceCode: "d", Interval: 5, ExpiresIn: 299})
	if !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("err = %v, want ErrAccessDenied", err)
	}
}

func TestPollForTokenExpiredToken(t *testing.T) {
	t.Parallel()
	m := newMockAuthKit(t, []mockResponse{
		{http.StatusBadRequest, map[string]any{"error": "expired_token"}},
	})
	client := newTestClient(m, nil)

	_, err := client.PollForToken(context.Background(), &DeviceAuthorization{DeviceCode: "d", Interval: 5, ExpiresIn: 299})
	if !errors.Is(err, ErrExpiredToken) {
		t.Fatalf("err = %v, want ErrExpiredToken", err)
	}
}

func TestPollForTokenHonorsContextCancellation(t *testing.T) {
	t.Parallel()
	m := newMockAuthKit(t, []mockResponse{pendingResponse()})
	client := newTestClient(m, nil)

	ctx, cancel := context.WithCancel(context.Background())
	client.sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		return ctx.Err()
	}

	_, err := client.PollForToken(ctx, &DeviceAuthorization{DeviceCode: "d", Interval: 5, ExpiresIn: 299})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// transientResponse simulates a server hiccup: a 5xx with no OAuth error
// code, which tokenGrant reports as a plain error.
func transientResponse() mockResponse {
	return mockResponse{http.StatusInternalServerError, map[string]any{}}
}

func TestPollForTokenToleratesTransientFailures(t *testing.T) {
	t.Parallel()
	// Two hiccups, a pending (which resets the failure counter), two more
	// hiccups, then success — the whole sequence must survive.
	m := newMockAuthKit(t, []mockResponse{
		transientResponse(), transientResponse(),
		pendingResponse(),
		transientResponse(), transientResponse(),
		successResponse(),
	})
	client := newTestClient(m, nil)

	tokens, err := client.PollForToken(context.Background(), &DeviceAuthorization{DeviceCode: "d", Interval: 5, ExpiresIn: 299})
	if err != nil {
		t.Fatalf("PollForToken() error = %v", err)
	}
	if tokens.AccessToken != "access-1" {
		t.Errorf("unexpected tokens: %+v", tokens)
	}
}

func TestPollForTokenGivesUpAfterConsecutiveFailures(t *testing.T) {
	t.Parallel()
	m := newMockAuthKit(t, []mockResponse{
		transientResponse(), transientResponse(), transientResponse(),
	})
	client := newTestClient(m, nil)

	_, err := client.PollForToken(context.Background(), &DeviceAuthorization{DeviceCode: "d", Interval: 5, ExpiresIn: 299})
	if err == nil || !strings.Contains(err.Error(), "poll for sign-in") {
		t.Fatalf("err = %v, want poll failure after consecutive errors", err)
	}
	if len(m.tokenForms) != 3 {
		t.Errorf("token calls = %d, want 3", len(m.tokenForms))
	}
}

func TestPollForTokenStopsOnRefusedRedirect(t *testing.T) {
	t.Parallel()
	var tokenCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		tokenCalls.Add(1)
		http.Redirect(w, r, "/target", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewResolver(config.Config{OAuthDomain: srv.URL, HTTPTimeout: time.Second}).OAuthClient()
	client.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }

	_, err := client.PollForToken(context.Background(), &DeviceAuthorization{DeviceCode: "d", Interval: 5, ExpiresIn: 299})

	var refused *httpclient.RefusedRedirectError
	if !errors.As(err, &refused) {
		t.Fatalf("PollForToken() error = %v, want a refused redirect", err)
	}
	if got := tokenCalls.Load(); got != 1 {
		t.Errorf("token calls = %d, want 1", got)
	}
}

func TestRefreshRotatesToken(t *testing.T) {
	t.Parallel()
	m := newMockAuthKit(t, []mockResponse{
		{http.StatusOK, map[string]any{
			"access_token":  "access-2",
			"refresh_token": "refresh-2",
			"expires_in":    3600,
		}},
	})
	client := newTestClient(m, nil)

	// The session's stored client id wins over the client's baked-in default.
	tokens, err := client.Refresh(context.Background(), m.srv.URL+"/oauth2/token", "client_stored_456", "refresh-1")
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if tokens.AccessToken != "access-2" || tokens.RefreshToken != "refresh-2" {
		t.Errorf("unexpected tokens: %+v", tokens)
	}

	form := m.tokenForms[0]
	if got := form.Get("grant_type"); got != "refresh_token" {
		t.Errorf("grant_type = %q", got)
	}
	if got := form.Get("refresh_token"); got != "refresh-1" {
		t.Errorf("refresh_token = %q", got)
	}
	if got := form.Get("client_id"); got != "client_stored_456" {
		t.Errorf("client_id = %q, want the session's stored client id", got)
	}
}

func TestRefreshInvalidGrant(t *testing.T) {
	t.Parallel()
	m := newMockAuthKit(t, []mockResponse{
		{http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "revoked"}},
	})
	client := newTestClient(m, nil)

	_, err := client.Refresh(context.Background(), m.srv.URL+"/oauth2/token", "", "refresh-1")
	if !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("err = %v, want ErrInvalidGrant", err)
	}
}

func TestRefreshFallsBackToConventionalTokenEndpoint(t *testing.T) {
	t.Parallel()
	m := newMockAuthKit(t, []mockResponse{successResponse()})
	client := newTestClient(m, nil)

	// Empty endpoint and client id (older stored session): the domain's
	// conventional token path and the client's default id are used.
	if _, err := client.Refresh(context.Background(), "", "", "refresh-1"); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if len(m.tokenForms) != 1 {
		t.Fatalf("expected 1 token call, got %d", len(m.tokenForms))
	}
	if got := m.tokenForms[0].Get("client_id"); got != "client_test_123" {
		t.Errorf("client_id = %q, want the client default", got)
	}
}

func TestEndpointsFallBackWithoutDiscovery(t *testing.T) {
	t.Parallel()
	m := newMockAuthKit(t, nil)
	m.serveDiscovery = false
	client := newTestClient(m, nil)

	// Discovery 404s; the conventional AuthKit paths still work.
	da, err := client.StartDeviceAuthorization(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceAuthorization() error = %v", err)
	}
	if da.DeviceCode != "device-code-1" {
		t.Errorf("unexpected device authorization: %+v", da)
	}
}

func TestExpiresAtFallsBackToJWTExp(t *testing.T) {
	t.Parallel()
	exp := time.Now().Add(30 * time.Minute).Unix()
	jwt := makeTestJWT(t, map[string]any{"sub": "user_1", "exp": exp})
	m := newMockAuthKit(t, []mockResponse{
		{http.StatusOK, map[string]any{"access_token": jwt, "refresh_token": "r"}},
	})
	client := newTestClient(m, nil)

	tokens, err := client.PollForToken(context.Background(), &DeviceAuthorization{DeviceCode: "d", Interval: 5, ExpiresIn: 299})
	if err != nil {
		t.Fatalf("PollForToken() error = %v", err)
	}
	if tokens.ExpiresAt.Unix() != exp {
		t.Errorf("ExpiresAt = %v, want JWT exp %v", tokens.ExpiresAt.Unix(), exp)
	}
}

func TestRefreshRefusesRedirect(t *testing.T) {
	t.Parallel()
	var targetHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			targetHits.Add(1)
			writeJSON(w, http.StatusOK, map[string]any{"access_token": "access-2", "expires_in": 3600})
			return
		}
		http.Redirect(w, r, "/target", http.StatusFound)
	}))
	defer srv.Close()

	client := NewResolver(config.Config{OAuthDomain: srv.URL, HTTPTimeout: time.Second}).OAuthClient()
	tokens, err := client.Refresh(context.Background(), srv.URL+"/oauth2/token", "client", "refresh-1")

	if err == nil {
		t.Fatalf("Refresh() succeeded with %+v", tokens)
	}
	var refused *httpclient.RefusedRedirectError
	if !errors.As(err, &refused) || refused.StatusCode != http.StatusFound {
		t.Fatalf("Refresh() error = %v, want a refused 302", err)
	}
	if got := targetHits.Load(); got != 0 {
		t.Fatalf("redirect target was hit %d times", got)
	}
}
