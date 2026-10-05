package codearts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LoginResult is the outcome of an interactive login attempt.
type LoginResult struct {
	Credential *Credential `json:"credential,omitempty"`
	AuthURL    string      `json:"auth_url,omitempty"`
	Error      string      `json:"error,omitempty"`
	Stage      string      `json:"stage"`
}

// ticketResponse is the snap-manager ticket exchange payload.
//
// Field names beyond TicketID are tolerated rather than required: upstream may
// or may not echo the credential inline depending on whether the ticket is
// already bound to a session, and a strict struct would turn that variation into
// a parse failure.
type ticketResponse struct {
	TicketID    string `json:"ticket_id"`
	Status      string `json:"status"`
	AccessKeyID string `json:"access_key_id"`
	SecretKey   string `json:"secret_access_key"`
	SecurityTok string `json:"security_token"`
	Expiration  string `json:"expiration"`
	Message     string `json:"message"`
	Code        string `json:"code"`
}

// tokenResponse is the STS OAuth2 token endpoint payload.
type tokenResponse struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SecurityToken   string `json:"security_token"`
	Expiration      string `json:"expiration"`
	ExpiresAt       string `json:"expires_at"`
	ExpiresIn       int    `json:"expires_in"`
}

// StartOAuthFlow builds the PKCE authorize URL for an interactive login.
//
// The caller opens AuthURL in a browser. Completion happens either through the
// loopback callback or through the ticket-polling path, depending on which one
// upstream is configured to use for this client.
func StartOAuthFlow(redirectURI string) (authURL string, pkce PkcePair, state string, err error) {
	pkce, err = GeneratePkcePair()
	if err != nil {
		return "", PkcePair{}, "", err
	}
	state = randHex(16)

	values := url.Values{}
	values.Set("response_type", "code")
	values.Set("client_id", "codearts")
	values.Set("redirect_uri", redirectURI)
	values.Set("scope", "openid profile")
	values.Set("state", state)
	values.Set("code_challenge", pkce.CodeChallenge)
	values.Set("code_challenge_method", "SHA-256")

	return PortalHost + PathPortalAuthorize + "?" + values.Encode(), pkce, state, nil
}

// ListenOnCallbackPort waits for the OAuth redirect on a loopback listener.
//
// It returns the authorization code. The listener binds loopback only: a
// callback port reachable from the network would let anyone who can reach the
// device complete a login as this user.
func ListenOnCallbackPort(ctx context.Context, port int, expectState string, timeout time.Duration) (string, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("codearts: listen on callback port %d: %w", port, err)
	}
	defer ln.Close()

	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	_ = ln.(*net.TCPListener).SetDeadline(deadline)

	type answer struct {
		code string
		err  error
	}
	results := make(chan answer, 1)

	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second}
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		// The state parameter is the only thing preventing a third party from
		// feeding us an authorization code for a different session.
		if expectState != "" && q.Get("state") != expectState {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			select {
			case results <- answer{err: errors.New("codearts: oauth state mismatch")}:
			default:
			}
			return
		}
		if errCode := q.Get("error"); errCode != "" {
			http.Error(w, "authorization failed", http.StatusBadRequest)
			select {
			case results <- answer{err: fmt.Errorf("codearts: authorize returned %s: %s",
				errCode, q.Get("error_description"))}:
			default:
			}
			return
		}

		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("Login complete. You can close this window."))
		select {
		case results <- answer{code: code}:
		default:
		}
	})

	go func() {
		_ = srv.Serve(ln)
	}()
	defer srv.Close()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case res := <-results:
		return res.code, res.err
	case <-time.After(timeout):
		return "", errors.New("codearts: timed out waiting for the OAuth callback")
	}
}

// ExchangeCodeForToken swaps an authorization code for STS credentials.
//
// This is the step whose request shape is least certain from the reverse
// engineering: the endpoint is known (/v1/oauth2/tokens) but the exact parameter
// names are not recovered from the binary. The OAuth2 conventions are sent as a
// form body, and the response is decoded tolerantly, so a server that answers
// with the credential under different key names still parses if it uses any of
// the variants above.
func ExchangeCodeForToken(ctx context.Context, client *http.Client, code string, pkce PkcePair, redirectURI string) (*Credential, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("code_verifier", pkce.CodeVerifier)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", "codearts")

	endpoint := STSHost + PathSTSTokens
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codearts: exchange code: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("codearts: exchange code: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("codearts: exchange code: %s",
			describeHTTPFailure(resp.StatusCode, body))
	}
	return parseCredentialJSON(body)
}

// PollForCredential polls the ticket endpoint until credentials appear.
//
// The desktop client's flow is ticket-based: the browser authorization yields a
// ticket id, and the client polls until the ticket is bound. Polling stops on
// the first usable credential, on an explicit failure status, or when the
// context ends.
func PollForCredential(ctx context.Context, client *http.Client, ticketID string, interval, timeout time.Duration) (*Credential, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	endpoint := SnapHost + PathLoginTicket + "?ticket_id=" + url.QueryEscape(ticketID)

	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
		} else {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if readErr != nil {
				lastErr = readErr
			} else if cred, done, err := parseTicketResponse(resp.StatusCode, body); done {
				if err != nil {
					return nil, err
				}
				return cred, nil
			} else if err != nil {
				lastErr = err
			}
		}

		if time.Now().After(deadline) {
			if lastErr != nil {
				return nil, fmt.Errorf("codearts: poll for credential: %w", lastErr)
			}
			return nil, errors.New("codearts: timed out waiting for the ticket to bind")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// parseTicketResponse interprets one poll response.
//
// done reports whether polling should stop. A ticket that is still pending
// returns done=false and no error, which is what keeps the loop going rather
// than treating "not yet" as a failure.
func parseTicketResponse(status int, body []byte) (cred *Credential, done bool, err error) {
	if status >= 400 {
		// 404 while pending is normal: the ticket exists but is not bound yet.
		if status == http.StatusNotFound || status == http.StatusAccepted {
			return nil, false, nil
		}
		return nil, true, fmt.Errorf("codearts: ticket exchange: %s",
			describeHTTPFailure(status, body))
	}

	var payload ticketResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false, nil // unparseable while pending; keep trying
	}

	switch strings.ToLower(strings.TrimSpace(payload.Status)) {
	case "pending", "waiting", "processing", "":
		// An empty status with no key material is also "not yet".
		if payload.AccessKeyID == "" {
			return nil, false, nil
		}
	case "failed", "error", "expired", "denied":
		msg := payload.Message
		if msg == "" {
			msg = payload.Status
		}
		return nil, true, fmt.Errorf("codearts: login %s: %s", payload.Status, msg)
	}

	if payload.AccessKeyID == "" {
		// The credential may still be nested under a data envelope. A successful
		// unwrap ends polling; a failure to find one means "still pending".
		cred, err := parseCredentialJSON(body)
		if err != nil {
			return nil, false, nil
		}
		return cred, true, nil
	}

	expiry := payload.Expiration
	if expiry == "" {
		expiry = time.Now().Add(12 * time.Hour).UTC().Format(time.RFC3339)
	}
	return &Credential{
		AccessKeyID:     payload.AccessKeyID,
		SecretAccessKey: payload.SecretKey,
		SecurityToken:   payload.SecurityTok,
		ExpiresAt:       expiry,
	}, true, nil
}

// parseCredentialJSON decodes a credential that may be top-level or wrapped.
//
// Upstream wraps most payloads in {"data": {...}} and sometimes in
// {"result": {...}}; both are unwrapped before decoding, because a strict
// top-level decode would silently produce an empty credential.
func parseCredentialJSON(body []byte) (*Credential, error) {
	candidates := [][]byte{body}

	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) == nil {
		for _, key := range []string{"data", "result", "credential", "codearts"} {
			if raw, ok := envelope[key]; ok && len(raw) > 0 && raw[0] == '{' {
				candidates = append(candidates, raw)
			}
		}
	}

	var firstErr error
	for _, candidate := range candidates {
		var cred Credential
		if err := json.Unmarshal(candidate, &cred); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// A credential with no key material is not a credential; keep looking at
		// the wrapped candidates before giving up.
		if cred.Usable() {
			if cred.ExpiresAt == "" {
				var tok tokenResponse
				if json.Unmarshal(candidate, &tok) == nil {
					cred.ExpiresAt = firstNonEmpty(tok.Expiration, tok.ExpiresAt)
					if cred.ExpiresAt == "" && tok.ExpiresIn > 0 {
						cred.ExpiresAt = time.Now().Add(
							time.Duration(tok.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
					}
				}
			}
			return &cred, nil
		}
	}

	if firstErr != nil {
		return nil, fmt.Errorf("codearts: parse credential: %w", firstErr)
	}
	return nil, errors.New("codearts: credential not usable")
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// describeHTTPFailure renders an upstream error without dumping a whole body
// into the log. The body is capped because a gateway error page can be large and
// is not more informative past the first few hundred bytes.
func describeHTTPFailure(status int, body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 400 {
		text = text[:400] + "…"
	}
	if text == "" {
		return fmt.Sprintf("HTTP %d", status)
	}
	return fmt.Sprintf("HTTP %d: %s", status, text)
}

// IsRefreshTokenExpired reports whether an error means only a fresh interactive
// login can recover the account.
func IsRefreshTokenExpired(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrExpiredRefreshToken) || errors.Is(err, ErrReLoginRequired) {
		return true
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{
		"refresh token expired",
		"refresh_token expired",
		"invalid_grant",
		"re-login required",
		"refresh materials missing",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// IsAuthError reports whether an error is an authentication problem, as opposed
// to a transport or quota problem. The distinction matters because an auth error
// should take the account out of rotation, while a transport error should not.
func IsAuthError(status int, body []byte) bool {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return true
	}
	text := strings.ToLower(string(body))
	for _, marker := range []string{
		"invalid access key", "signature does not match", "token expired",
		"authentication failed", "no permission",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// IsQueueErrorCode reports whether an upstream code means "busy, retry later".
//
// CodeArts queues chat requests and answers with a distinct code when the queue
// is full; treating that as a hard failure would cool a perfectly healthy
// account.
func IsQueueErrorCode(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "queue_full", "queuefull", "too_many_requests", "busy", "throttled":
		return true
	}
	return false
}
