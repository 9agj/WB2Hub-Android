package codearts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Manager owns one account's CodeArts session.
//
// It caches the credential and the DPoP key in memory, refreshes the STS
// credential before it expires, and signs every outgoing request. A Manager is
// bound to a single account because the DPoP key and the security token are
// both per-account state; sharing one across accounts would make every request
// carry another account's identity.
type Manager struct {
	mu      sync.RWMutex
	cred    *Credential
	dpop    *DpopKeyPair
	realm   string
	baseURL string

	// client is the transport for signed calls. It is injected rather than built
	// here so the caller can route it through the account's proxy slot.
	client *http.Client

	now func() time.Time

	// onSave persists a refreshed credential. A nil callback means the caller
	// does not want durability, which is the case in tests.
	onSave func(*Credential) error
}

// Options configures a Manager.
type Options struct {
	Credential *Credential
	Realm      string
	// BaseURL overrides the snap-access host, for pointing at a test double.
	BaseURL string
	Client  *http.Client
	Now     func() time.Time
	// OnSave is called after a successful refresh so the caller can write the new
	// credential back to the account file.
	OnSave func(*Credential) error
}

// NewManager builds a Manager from stored credential material.
//
// A missing DPoP key is generated here rather than treated as an error: the key
// is ours to mint, and the only reason it is persisted at all is so that it
// stays stable across restarts.
func NewManager(opts Options) (*Manager, error) {
	if opts.Credential == nil {
		return nil, ErrNotSupported
	}
	m := &Manager{
		cred:    opts.Credential,
		realm:   opts.Realm,
		baseURL: opts.BaseURL,
		client:  opts.Client,
		now:     opts.Now,
		onSave:  opts.OnSave,
	}
	if m.baseURL == "" {
		m.baseURL = SnapHost
	}
	if m.client == nil {
		m.client = &http.Client{Timeout: 120 * time.Second}
	}
	if m.now == nil {
		m.now = time.Now
	}

	if raw := opts.Credential.DpopPrivateJwk; len(raw) > 0 {
		var key DpopKeyPair
		if err := json.Unmarshal(raw, &key); err != nil {
			return nil, err
		}
		m.dpop = &key
	}
	if m.dpop == nil || m.dpop.private == nil {
		key, err := GenerateDpopKeyPair()
		if err != nil {
			return nil, err
		}
		m.dpop = key
		if m.onSave != nil {
			// Persist immediately: losing the key means upstream will no longer
			// recognise this session.
			_ = m.persistLocked()
		}
	}
	return m, nil
}

// Credential returns a copy of the current credential.
func (m *Manager) Credential() Credential {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cred == nil {
		return Credential{}
	}
	out := *m.cred
	return out
}

// Refreshable reports whether this account has what a refresh needs.
func (m *Manager) Refreshable() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cred != nil && m.cred.Usable() && strings.TrimSpace(m.cred.SecretAccessKey) != ""
}

// NeedsRefresh reports whether the credential is missing or near expiry.
func (m *Manager) NeedsRefresh(skew time.Duration) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cred == nil {
		return true
	}
	return m.cred.NeedsRefresh(m.now(), skew)
}

// persistLocked writes the credential plus the DPoP key back to the caller.
func (m *Manager) persistLocked() error {
	if m.onSave == nil || m.cred == nil {
		return nil
	}
	cred := *m.cred
	if raw, err := json.Marshal(m.dpop); err == nil {
		cred.DpopPrivateJwk = raw
	}
	return m.onSave(&cred)
}

// Refresh re-exchanges the STS credential.
//
// The AK/SK pair is long-lived; only the security token and its expiry rotate.
// So a refresh presents the stored credential and adopts the new security token,
// leaving the keys alone unless upstream hands back new ones.
func (m *Manager) Refresh(ctx context.Context) error {
	m.mu.Lock()
	cred := m.cred
	m.mu.Unlock()

	if cred == nil || !cred.Usable() {
		return ErrReLoginRequired
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("access_key_id", cred.AccessKeyID)
	form.Set("secret_access_key", cred.SecretAccessKey)
	if cred.SecurityToken != "" {
		form.Set("security_token", cred.SecurityToken)
	}

	fresh, err := m.doTokenRequest(ctx, form)
	if err != nil {
		// A rejected refresh means only a new interactive login can recover it;
		// reporting that distinctly lets the caller surface the right message.
		if IsRefreshTokenExpired(err) {
			return fmt.Errorf("%w: %v", ErrReLoginRequired, err)
		}
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if fresh.AccessKeyID == "" {
		fresh.AccessKeyID = cred.AccessKeyID
	}
	if fresh.SecretAccessKey == "" {
		fresh.SecretAccessKey = cred.SecretAccessKey
	}
	m.cred = fresh
	return m.persistLocked()
}

// doTokenRequest posts a token request to the STS endpoint.
func (m *Manager) doTokenRequest(ctx context.Context, form url.Values) (*Credential, error) {
	endpoint := STSHost + PathSTSTokens
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codearts: refresh: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("codearts: refresh: %w", err)
	}
	if resp.StatusCode >= 400 {
		if IsAuthError(resp.StatusCode, body) {
			return nil, fmt.Errorf("%w: %s", ErrExpiredRefreshToken,
				describeHTTPFailure(resp.StatusCode, body))
		}
		return nil, fmt.Errorf("codearts: refresh: %s",
			describeHTTPFailure(resp.StatusCode, body))
	}
	return parseCredentialJSON(body)
}

// signedRequest builds, signs and issues one CodeArts API call.
//
// Signing happens after the body is materialised because the signature covers
// the payload hash, so the body cannot be streamed straight from the caller.
func (m *Manager) signedRequest(ctx context.Context, method, path string, payload any) (*http.Response, []byte, error) {
	var body []byte
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, nil, fmt.Errorf("codearts: encode request body: %w", err)
		}
		body = encoded
	}

	if m.NeedsRefresh(5 * time.Minute) {
		if err := m.Refresh(ctx); err != nil {
			return nil, nil, err
		}
	}

	m.mu.RLock()
	cred := *m.cred
	dpop := m.dpop
	endpoint := m.baseURL + path
	m.mu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "CodeArts/1.0")

	if err := SignRequestHuawei(req, &cred, body, m.now()); err != nil {
		return nil, nil, err
	}
	if err := ApplyDpop(req, dpop, cred.SecurityToken, m.now()); err != nil {
		return nil, nil, err
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("codearts: %s %s: %w", method, path, err)
	}
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	resp.Body.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("codearts: %s %s: %w", method, path, err)
	}
	return resp, respBody, nil
}

// doJSON issues a signed call and decodes the JSON reply.
func (m *Manager) doJSON(ctx context.Context, method, path string, payload any) (map[string]any, []byte, int, error) {
	resp, body, err := m.signedRequest(ctx, method, path, payload)
	if err != nil {
		return nil, nil, 0, err
	}
	var decoded map[string]any
	if len(bytes.TrimSpace(body)) > 0 {
		_ = json.Unmarshal(body, &decoded)
	}
	return decoded, body, resp.StatusCode, nil
}

// Models fetches the built-in model catalog.
func (m *Manager) FetchModels(ctx context.Context) ([]string, error) {
	doc, body, status, err := m.doJSON(ctx, http.MethodGet, PathBuiltinModels, nil)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("codearts: fetch models: %s", describeHTTPFailure(status, body))
	}
	names := extractArray(doc, "models", "data", "items")
	if len(names) == 0 {
		return nil, errors.New("codearts: no models fetched")
	}
	return names, nil
}

// extractArray pulls a list of model names out of a loosely shaped response.
//
// The catalog has been observed both as a bare array and as an object with the
// array under one of several keys, with entries that are either strings or
// objects carrying a name/id. All of those are accepted; none are required.
func extractArray(doc map[string]any, keys ...string) []string {
	seen := map[string]bool{}
	out := []string{}

	collect := func(v any) {
		switch item := v.(type) {
		case string:
			if item != "" && !seen[item] {
				seen[item] = true
				out = append(out, item)
			}
		case map[string]any:
			for _, key := range []string{"name", "id", "model", "model_name"} {
				if s, ok := item[key].(string); ok && s != "" && !seen[s] {
					seen[s] = true
					out = append(out, s)
					return
				}
			}
		}
	}

	var walk func(node any)
	walk = func(node any) {
		switch v := node.(type) {
		case []any:
			for _, item := range v {
				collect(item)
			}
		case map[string]any:
			for _, key := range keys {
				if inner, ok := v[key]; ok {
					walk(inner)
				}
			}
		}
	}

	walk(doc)
	return out
}

// AccountInfo fetches the account's identity and quota view.
func (m *Manager) FetchAccountInfo(ctx context.Context) (map[string]any, error) {
	doc, body, status, err := m.doJSON(ctx, http.MethodGet, PathStats, nil)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("codearts: fetch account info: %s", describeHTTPFailure(status, body))
	}
	return doc, nil
}

// OpsActivities fetches the operations/activity feed.
func (m *Manager) FetchOpsActivities(ctx context.Context) (map[string]any, error) {
	doc, body, status, err := m.doJSON(ctx, http.MethodGet, PathOpsClaim, nil)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("codearts: fetch ops activities: %s", describeHTTPFailure(status, body))
	}
	return doc, nil
}

// ClaimDailyCheckin claims the CodeArts daily check-in.
func (m *Manager) ClaimDailyCheckin(ctx context.Context) (map[string]any, error) {
	doc, body, status, err := m.doJSON(ctx, http.MethodPost, PathOpsConfirm, map[string]any{})
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("codearts: checkin: %s", describeHTTPFailure(status, body))
	}
	return doc, nil
}

// ChatStream forwards a chat completion to CodeArts.
//
// The response is SSE, so it is relayed rather than buffered. A queue rejection
// is reported distinctly so the caller can retry instead of cooling the account.
func (m *Manager) ChatStream(ctx context.Context, payload map[string]any, w io.Writer) (int, error) {
	resp, body, err := m.signedRequest(ctx, http.MethodPost, PathChatCompletions, payload)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode >= 400 {
		// A queue rejection is a "come back later", not an account fault.
		var envelope struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(body, &envelope)
		if IsQueueErrorCode(envelope.Code) {
			return resp.StatusCode, fmt.Errorf("codearts: queue busy (%s)", envelope.Code)
		}
		return resp.StatusCode, fmt.Errorf("codearts: chat: %s",
			describeHTTPFailure(resp.StatusCode, body))
	}
	if w == nil {
		return resp.StatusCode, nil
	}
	if _, err := w.Write(body); err != nil {
		return resp.StatusCode, fmt.Errorf("codearts: chat transport: %w", err)
	}
	return resp.StatusCode, nil
}

// NewSessionID mints a session identifier for a chat call.
func NewSessionID() string { return randHex(16) }
