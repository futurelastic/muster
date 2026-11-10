package grant

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultIssuer and DefaultClientID are where, and as whom, the runtime itself
// signs in to a ChatGPT subscription. This package signs in as the same client
// because the grant it holds is the one the runtime then presents: a token issued
// to another client would not be the sign-in the runtime expects.
const (
	DefaultIssuer   = "https://auth.openai.com"
	DefaultClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
)

// Client talks to the provider's token endpoint. The zero value is not usable;
// NewClient fills the defaults. Issuer and HTTP are fields so a test points it at
// a server of its own.
type Client struct {
	Issuer   string
	ClientID string
	HTTP     *http.Client
	// Sleep waits between device-flow polls. A field so a test does not wait.
	Sleep func(ctx context.Context, d time.Duration) error
}

// NewClient returns a Client for the real provider.
func NewClient() *Client {
	return &Client{
		Issuer:   DefaultIssuer,
		ClientID: DefaultClientID,
		HTTP:     &http.Client{Timeout: 30 * time.Second},
		Sleep:    sleepCtx,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Tokens is what the token endpoint returns for a sign-in or a refresh.
type Tokens struct {
	Access    string
	Refresh   string
	ExpiresIn time.Duration
	AccountID string
}

// RejectedError means the provider said the refresh token is no good: it was
// revoked, expired, or already used. Nothing retried later will change that.
type RejectedError struct {
	Status int
	Code   string
}

func (e *RejectedError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("the provider refused the refresh token (HTTP %d, %s)", e.Status, e.Code)
	}
	return fmt.Sprintf("the provider refused the refresh token (HTTP %d)", e.Status)
}

// TransientError means a refresh did not complete for a reason that says nothing
// about the grant: the provider unreachable, throttling, or failing. It is
// retried with a backoff and never revokes anything.
type TransientError struct {
	Status int
	Err    error
}

func (e *TransientError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("the token endpoint answered HTTP %d", e.Status)
	}
	return fmt.Sprintf("the token endpoint could not be reached: %v", e.Err)
}

func (e *TransientError) Unwrap() error { return e.Err }

// rejectCodes are the error codes that mean the grant itself is finished. A 401
// from the token endpoint is always that; a 400 or 403 only when it says so,
// because a malformed request of our own would also be a 400 and must not
// revoke a good grant.
var rejectCodes = map[string]bool{
	"invalid_grant":             true,
	"token_expired":             true,
	"refresh_token_expired":     true,
	"refresh_token_reused":      true,
	"refresh_token_invalidated": true,
	"invalid_token":             true,
	"refresh_token_not_found":   true,
	"refresh_token_invalid":     true,
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    *int64 `json:"expires_in"`
}

// Refresh exchanges a refresh token for a new set. The response's refresh token
// replaces the one passed in: it rotates on every use.
func (c *Client) Refresh(ctx context.Context, refresh string) (Tokens, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {c.ClientID},
	}
	body, status, err := c.postForm(ctx, "/oauth/token", form)
	if err != nil {
		return Tokens{}, &TransientError{Err: err}
	}
	if status/100 != 2 {
		return Tokens{}, classify(status, body)
	}
	return parseTokens(body, refresh)
}

// classify turns a non-2xx token-endpoint answer into a RejectedError (the grant
// is finished) or a TransientError (it says nothing about the grant).
func classify(status int, body []byte) error {
	code := errorCode(body)
	switch {
	case status == http.StatusUnauthorized:
		return &RejectedError{Status: status, Code: code}
	case (status == http.StatusBadRequest || status == http.StatusForbidden) && rejectCodes[code]:
		return &RejectedError{Status: status, Code: code}
	}
	return &TransientError{Status: status}
}

// errorCode pulls the machine-readable code out of either error shape the
// provider uses: {"error":{"code":..}} or {"error":"invalid_grant",..}. It reads
// nothing else of the body, so nothing a response carries can reach a log.
func errorCode(body []byte) string {
	var shape struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &shape) != nil || len(shape.Error) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(shape.Error, &s) == nil {
		return sanitizeCode(s)
	}
	var obj struct {
		Code string `json:"code"`
		Type string `json:"type"`
	}
	if json.Unmarshal(shape.Error, &obj) == nil {
		if obj.Code != "" {
			return sanitizeCode(obj.Code)
		}
		return sanitizeCode(obj.Type)
	}
	return ""
}

// sanitizeCode keeps a code short and printable: it is stored and shown.
func sanitizeCode(s string) string {
	if len(s) > 64 {
		return ""
	}
	for _, r := range s {
		ok := r == '_' || r == '-' || r == '.' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return ""
		}
	}
	return s
}

func parseTokens(body []byte, prevRefresh string) (Tokens, error) {
	var r tokenResponse
	if err := json.Unmarshal(body, &r); err != nil || r.AccessToken == "" {
		return Tokens{}, &TransientError{Err: errors.New("the token endpoint's answer carried no access token")}
	}
	t := Tokens{Access: r.AccessToken, Refresh: r.RefreshToken, ExpiresIn: time.Hour}
	if t.Refresh == "" {
		t.Refresh = prevRefresh
	}
	if r.ExpiresIn != nil && *r.ExpiresIn > 0 {
		t.ExpiresIn = time.Duration(*r.ExpiresIn) * time.Second
	}
	t.AccountID = accountID(r.IDToken, r.AccessToken)
	return t, nil
}

// accountID reads the provider's account id out of the id token, else the access
// token. Both are JWTs; only the payload is read, and it is not verified here —
// it is an identifier the provider's own endpoint just issued over TLS.
func accountID(tokens ...string) string {
	for _, tok := range tokens {
		parts := strings.Split(tok, ".")
		if len(parts) != 3 {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			continue
		}
		var claims struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
			Auth             struct {
				ChatGPTAccountID string `json:"chatgpt_account_id"`
			} `json:"https://api.openai.com/auth"`
			Organizations []struct {
				ID string `json:"id"`
			} `json:"organizations"`
		}
		if json.Unmarshal(raw, &claims) != nil {
			continue
		}
		switch {
		case claims.ChatGPTAccountID != "":
			return claims.ChatGPTAccountID
		case claims.Auth.ChatGPTAccountID != "":
			return claims.Auth.ChatGPTAccountID
		case len(claims.Organizations) > 0 && claims.Organizations[0].ID != "":
			return claims.Organizations[0].ID
		}
	}
	return ""
}

// Device is a device-code sign-in in progress: the person opens URL, types Code,
// and approves, on any machine with a browser.
type Device struct {
	URL      string
	Code     string
	id       string
	interval time.Duration
}

// StartDevice begins the device-code sign-in.
func (c *Client) StartDevice(ctx context.Context) (Device, error) {
	raw, _ := json.Marshal(map[string]string{"client_id": c.ClientID})
	body, status, err := c.postJSON(ctx, "/api/accounts/deviceauth/usercode", raw)
	if err != nil {
		return Device{}, fmt.Errorf("grant: starting the sign-in: %w", err)
	}
	if status/100 != 2 {
		return Device{}, fmt.Errorf("grant: starting the sign-in: the provider answered HTTP %d", status)
	}
	var r struct {
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
		Interval     any    `json:"interval"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.DeviceAuthID == "" || r.UserCode == "" {
		return Device{}, errors.New("grant: starting the sign-in: the provider's answer carried no code")
	}
	iv := 5 * time.Second
	switch v := r.Interval.(type) {
	case float64:
		iv = time.Duration(v) * time.Second
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			iv = time.Duration(n) * time.Second
		}
	}
	if iv < time.Second {
		iv = time.Second
	}
	return Device{URL: strings.TrimRight(c.Issuer, "/") + "/codex/device", Code: r.UserCode, id: r.DeviceAuthID, interval: iv}, nil
}

// PollDevice waits for the person to approve the sign-in and returns the tokens.
// It stops with ctx's error when ctx ends first.
func (c *Client) PollDevice(ctx context.Context, d Device) (Tokens, error) {
	sleep := c.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	for {
		raw, _ := json.Marshal(map[string]string{"device_auth_id": d.id, "user_code": d.Code})
		body, status, err := c.postJSON(ctx, "/api/accounts/deviceauth/token", raw)
		if err != nil {
			if ctx.Err() != nil {
				return Tokens{}, ctx.Err()
			}
			return Tokens{}, fmt.Errorf("grant: waiting for the sign-in: %w", err)
		}
		switch {
		case status/100 == 2:
			return c.exchangeDevice(ctx, body)
		case status == http.StatusForbidden || status == http.StatusNotFound:
			// Not approved yet. The runtime waits the interval and three seconds more.
			if err := sleep(ctx, d.interval+3*time.Second); err != nil {
				return Tokens{}, err
			}
		default:
			return Tokens{}, fmt.Errorf("grant: waiting for the sign-in: the provider answered HTTP %d", status)
		}
	}
}

func (c *Client) exchangeDevice(ctx context.Context, approved []byte) (Tokens, error) {
	var a struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
	}
	if err := json.Unmarshal(approved, &a); err != nil || a.AuthorizationCode == "" {
		return Tokens{}, errors.New("grant: the provider's approval carried no authorization code")
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {a.AuthorizationCode},
		"redirect_uri":  {strings.TrimRight(c.Issuer, "/") + "/deviceauth/callback"},
		"client_id":     {c.ClientID},
		"code_verifier": {a.CodeVerifier},
	}
	body, status, err := c.postForm(ctx, "/oauth/token", form)
	if err != nil {
		return Tokens{}, fmt.Errorf("grant: finishing the sign-in: %w", err)
	}
	if status/100 != 2 {
		return Tokens{}, fmt.Errorf("grant: finishing the sign-in: the provider answered HTTP %d", status)
	}
	t, err := parseTokens(body, "")
	if err != nil {
		return Tokens{}, err
	}
	if t.Refresh == "" {
		return Tokens{}, errors.New("grant: the provider's answer carried no refresh token, so the sign-in cannot be kept")
	}
	return t, nil
}

func (c *Client) postForm(ctx context.Context, path string, form url.Values) ([]byte, int, error) {
	return c.post(ctx, path, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
}

func (c *Client) postJSON(ctx context.Context, path string, raw []byte) ([]byte, int, error) {
	return c.post(ctx, path, "application/json", strings.NewReader(string(raw)))
}

// scrub reduces a transport error to what went wrong, dropping the address it
// names. The error is kept and shown (a listing, a doctor row), and nothing there
// needs to say where the provider is.
func scrub(err error) error {
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		return fmt.Errorf("%s: %w", oe.Op, oe.Err)
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

// post sends one request and reads at most 1 MiB of the answer. The error it
// returns is the transport's, with the address stripped (scrub).
func (c *Client) post(ctx context.Context, path, contentType string, body io.Reader) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.Issuer, "/")+path, body)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, scrub(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, 0, err
	}
	return raw, resp.StatusCode, nil
}
