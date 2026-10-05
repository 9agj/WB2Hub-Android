// Package upstream talks to the WorkBuddy / CodeBuddy model endpoints.
//
// Every request carries the identity headers the upstream fingerprints: the
// User-Agent of the desktop build for that realm, the origin/referer pair, and
// the per-account stable identifiers. Getting these wrong is not a cosmetic
// detail — the upstream rejects or misroutes the call.
//
// Ported from the request-building half of the Python reference (wb_accounts.py
// plus the WAF/sanitise helpers in wb_proxy.py).
package upstream

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"wb2hub/internal/auth"
	"wb2hub/internal/config"
)

// Client issues upstream requests, optionally through a per-account proxy.
type Client struct {
	HTTP *http.Client

	// Direct is used for requests that must not go through an account's proxy,
	// such as the geolocation lookup for a proxy slot.
	Direct *http.Client
}

// New builds a client with sane streaming-friendly timeouts.
func New() *Client {
	transport := &http.Transport{
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	return &Client{
		HTTP:   &http.Client{Transport: transport, Timeout: 120 * time.Second},
		Direct: &http.Client{Timeout: 20 * time.Second},
	}
}

// ForAccount returns an http.Client that egresses through the account's bound
// proxy slot, or the shared client when it has none.
//
// A separate client is built per proxy url rather than mutating the shared
// transport: the transport carries the route, so sharing one would let one
// account's egress leak into another's request.
func (c *Client) ForAccount(proxyURL string) *http.Client {
	parsed, ok := parseProxy(proxyURL)
	if !ok {
		return c.HTTP
	}
	return &http.Client{
		Timeout: c.HTTP.Timeout,
		Transport: &http.Transport{
			Proxy:             http.ProxyURL(parsed),
			MaxIdleConns:      16,
			IdleConnTimeout:   60 * time.Second,
			ForceAttemptHTTP2: true,
		},
	}
}

func parseProxy(raw string) (*url.URL, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, false
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return nil, false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
		return u, true
	}
	return nil, false
}

// Headers builds the full header set for one account and purpose.
//
// kind is "chat" or "billing"; the two use different User-Agents upstream.
func Headers(acct *auth.Account, kind string) http.Header {
	realm := acct.RealmConfig()
	token, _ := acct.Token()

	ua := realm.ChatUA
	if kind == "billing" {
		ua = realm.BillingUA
	}

	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("User-Agent", ua)
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	h.Set("Content-Type", "application/json")
	h.Set("Origin", realm.Origin)
	h.Set("Referer", realm.Origin+"/")
	h.Set("X-Domain", realm.Origin)
	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("X-Service", "workbuddy")
	h.Set("X-Account-UID", acct.UID)

	// Stable per-account identifiers. Sending the same values every time keeps
	// one account looking like one machine; deriving them from the UID (rather
	// than randomising per request) is what prevents multi-account correlation
	// from standing out as a burst of new devices.
	h.Set("X-Device-Id", stableID(acct.UID, "device"))
	h.Set("X-Session-Id", stableID(acct.UID, "session"))
	h.Set("X-Conversation-Request-ID", NewRequestID())
	return h
}

// stableID derives a hex identifier from the uid and a label.
func stableID(uid, label string) string {
	sum := fnv64a(label + "\x00" + uid)
	return hex.EncodeToString([]byte{
		byte(sum >> 56), byte(sum >> 48), byte(sum >> 40), byte(sum >> 32),
		byte(sum >> 24), byte(sum >> 16), byte(sum >> 8), byte(sum),
	})
}

// fnv64a is FNV-1a, chosen because it is short, dependency-free, and only needs
// to be stable — not cryptographic.
func fnv64a(s string) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	var h uint64 = offset
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

// NewRequestID makes a random request identifier.
//
// Unlike the stable identifiers this one is random per request on purpose: it
// is what upstream correlates an SSE stream's frames back to.
func NewRequestID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// ChatEndpoint returns the full URL for a chat completion against a realm.
func ChatEndpoint(realm config.Realm) string {
	return realm.ChatUpstream + config.PathChatComplet
}

// ModelsEndpoint returns the model catalog URL.
func ModelsEndpoint(realm config.Realm) string {
	return realm.ChatUpstream + "/v1/models"
}

// DoJSON performs a JSON request and decodes the response body.
//
// The body is returned raw as well, because the panel shows upstream complaints
// verbatim and a decoded map loses the exact wording.
func (c *Client) DoJSON(ctx context.Context, client *http.Client, method, url string,
	headers http.Header, payload any) (map[string]any, []byte, int, error) {

	if client == nil {
		client = c.HTTP
	}
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, nil, 0, err
		}
		body = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, nil, 0, err
	}
	for k, values := range headers {
		for _, v := range values {
			req.Header.Add(k, v)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, nil, resp.StatusCode, err
	}

	var decoded map[string]any
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			// A non-JSON body is not fatal: the status code still tells the
			// caller what happened, and the raw text is kept for the panel.
			decoded = nil
		}
	}
	return decoded, raw, resp.StatusCode, nil
}

// truncate caps a message so a pathological upstream response cannot bloat the
// panel, the log ring, or an error string handed back to a client.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
