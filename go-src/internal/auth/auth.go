// Package auth loads and refreshes WorkBuddy account credentials.
//
// An account is identified by its UID and carries the tokens the upstream
// expects. Credentials live in a per-account JSON file so an operator can drop
// a new one in at any time; the pool watches the directory.
//
// Ported from Account / REALM_CONFIGS handling in the Python reference
// (wb_accounts.py).
package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"wb2hub/internal/config"
)

// Account is one upstream identity plus everything the gateway remembers about
// it. The mutex guards the token fields, which a refresh rewrites in place
// while requests are in flight.
type Account struct {
	mu sync.RWMutex

	UID   string `json:"uid"`
	Realm string `json:"realm"`
	Path  string `json:"-"`

	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"` // unix seconds

	// ProxySlot is the id of the outbound proxy this account is bound to, or
	// empty for a direct connection. It is what makes per-account egress
	// possible without a global switch.
	ProxySlot string `json:"proxy_slot"`

	// Credential blobs kept verbatim so a refresh can present whatever upstream
	// asked for last time.
	Raw map[string]any `json:"raw,omitempty"`

	Enabled bool `json:"enabled"`
}

// Filename is the on-disk name for a UID.
//
// UIDs come from upstream and could in principle contain anything, so anything
// that is not safe in a filename is replaced rather than trusted.
func Filename(uid string) string {
	var b strings.Builder
	for _, r := range uid {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	name := b.String()
	if name == "" {
		name = "unknown"
	}
	return name + ".json"
}

// LoadDir reads every *.json credential file in dir.
//
// A file that fails to parse is skipped with an error rather than aborting the
// load: one corrupt credential must not take the whole gateway down.
func LoadDir(dir string) ([]*Account, []error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("read auth dir %s: %w", dir, err)}
	}

	var accounts []*Account
	var problems []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		acct, err := LoadFile(path)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", entry.Name(), err))
			continue
		}
		accounts = append(accounts, acct)
	}

	sort.Slice(accounts, func(i, j int) bool { return accounts[i].UID < accounts[j].UID })
	return accounts, problems
}

// LoadFile reads one credential file.
func LoadFile(path string) (*Account, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var acct Account
	if err := json.Unmarshal(raw, &acct); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	acct.Path = path

	if acct.UID == "" {
		// Older files may only carry the token; the UID is recoverable from it.
		acct.UID = JWTUID(acct.AccessToken)
	}
	if acct.UID == "" {
		return nil, fmt.Errorf("no uid in credential file")
	}
	if acct.Realm == "" {
		acct.Realm = RealmFromToken(acct.AccessToken)
	}
	if acct.ExpiresAt == 0 {
		acct.ExpiresAt = JWTExp(acct.AccessToken)
	}
	return &acct, nil
}

// Realm returns the account's realm, resolved at read time.
func (a *Account) RealmID() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.Realm == "" {
		return config.RealmIntl
	}
	return a.Realm
}

// Config returns the realm definition for this account.
func (a *Account) RealmConfig() config.Realm { return config.Get(a.RealmID()) }

// Token returns the current access token and its expiry.
func (a *Account) Token() (string, int64) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.AccessToken, a.ExpiresAt
}

// NeedsRefresh reports whether the access token is expired or within skew of
// expiring. A zero expiry means "unknown", which is treated as needing refresh.
func (a *Account) NeedsRefresh(skew time.Duration) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.AccessToken == "" {
		return true
	}
	if a.ExpiresAt == 0 {
		return true
	}
	return time.Now().Add(skew).Unix() >= a.ExpiresAt
}

// SetTokens installs freshly refreshed tokens.
func (a *Account) SetTokens(access, refresh string, expires int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if access != "" {
		a.AccessToken = access
	}
	if refresh != "" {
		a.RefreshToken = refresh
	}
	if expires != 0 {
		a.ExpiresAt = expires
	}
}

// Proxy returns the bound proxy slot id, empty for a direct connection.
func (a *Account) Proxy() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ProxySlot
}

// SetProxy rebinds this account's egress.
func (a *Account) SetProxy(slot string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ProxySlot = strings.TrimSpace(slot)
}

// Enabled reports whether the account may serve traffic.
func (a *Account) IsEnabled() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Enabled
}

// SetEnabled toggles the account.
func (a *Account) SetEnabled(on bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Enabled = on
}

// View is a lock-free copy of an account's fields, safe to marshal and to hand
// to callers that must not reach back into live pool state.
//
// It is a distinct type rather than an Account so copying it can never drag the
// account's mutex along.
type View struct {
	UID        string         `json:"uid"`
	Realm      string         `json:"realm"`
	ProxySlot  string         `json:"proxy_slot"`
	Enabled    bool           `json:"enabled"`
	ExpiresAt  int64          `json:"expires_at"`
	HasToken   bool           `json:"has_token"`
	HasRefresh bool           `json:"has_refresh"`
	Raw        map[string]any `json:"raw,omitempty"`
}

// Snapshot returns a copy safe to marshal without holding the lock.
func (a *Account) Snapshot() View {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := View{
		UID:        a.UID,
		Realm:      a.Realm,
		ProxySlot:  a.ProxySlot,
		Enabled:    a.Enabled,
		ExpiresAt:  a.ExpiresAt,
		HasToken:   a.AccessToken != "",
		HasRefresh: a.RefreshToken != "",
	}
	if a.Raw != nil {
		out.Raw = make(map[string]any, len(a.Raw))
		for k, v := range a.Raw {
			out.Raw[k] = v
		}
	}
	return out
}

// Save writes the account back to its file atomically, so a crash mid-write
// leaves the previous credentials intact rather than a truncated file.
func (a *Account) Save() error {
	if a.Path == "" {
		return fmt.Errorf("account %s has no path", a.UID)
	}
	a.mu.RLock()
	payload := map[string]any{
		"uid":           a.UID,
		"realm":         a.Realm,
		"access_token":  a.AccessToken,
		"refresh_token": a.RefreshToken,
		"expires_at":    a.ExpiresAt,
		"proxy_slot":    a.ProxySlot,
		"enabled":       a.Enabled,
	}
	if a.Raw != nil {
		payload["raw"] = a.Raw
	}
	a.mu.RUnlock()

	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(a.Path, body, 0o600)
}

// writeAtomic writes body to path via a temp file + rename.
func writeAtomic(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// JWTClaims decodes the payload segment of a JWT without verifying it.
//
// The signature is not checked because this is not an authorisation decision —
// it only reads facts the upstream already told us (uid, issuer, expiry) to
// route the account to the right realm. A tampered token buys nothing here: the
// upstream still has to accept it.
func JWTClaims(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return nil
	}
	segment := parts[1]
	if pad := len(segment) % 4; pad != 0 {
		segment += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(segment)
	if err != nil {
		if raw, err = base64.RawURLEncoding.DecodeString(parts[1]); err != nil {
			return nil
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil
	}
	return claims
}

// JWTExp returns the token's exp claim as unix seconds, or 0.
func JWTExp(token string) int64 {
	claims := JWTClaims(token)
	if claims == nil {
		return 0
	}
	return NormalizeEpoch(claims["exp"])
}

// JWTUID returns the token's subject, or "".
func JWTUID(token string) string {
	claims := JWTClaims(token)
	if claims == nil {
		return ""
	}
	if sub, ok := claims["sub"].(string); ok {
		return sub
	}
	return ""
}

// JWTIssuer returns the token's issuer, or "".
func JWTIssuer(token string) string {
	claims := JWTClaims(token)
	if claims == nil {
		return ""
	}
	if iss, ok := claims["iss"].(string); ok {
		return iss
	}
	return ""
}

// NormalizeEpoch converts a JSON number to unix seconds.
//
// Upstream is inconsistent about units — some endpoints report milliseconds —
// so anything past 1e11 is treated as milliseconds.
func NormalizeEpoch(value any) int64 {
	var number float64
	switch v := value.(type) {
	case float64:
		number = v
	case int64:
		number = float64(v)
	case int:
		number = float64(v)
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0
		}
		number = f
	default:
		return 0
	}
	if number > 1e11 {
		number /= 1000
	}
	return int64(number)
}

// RealmFromToken guesses the realm from a token's issuer.
func RealmFromToken(token string) string {
	return config.DetectFromDomain(JWTIssuer(token))
}

// AccountFromFields builds an in-memory account from pasted credentials.
//
// The UID usually comes from the token, but a caller staging a credential while
// offline may supply it explicitly; an explicit value wins so an operator can
// import a file whose subject claims are not usable as an identifier.
func AccountFromFields(uid, realm, accessToken, refreshToken, proxySlot, dir string) *Account {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		uid = JWTUID(accessToken)
	}
	if uid == "" {
		return nil
	}
	if realm == "" {
		realm = RealmFromToken(accessToken)
	}
	acct := &Account{
		UID:          uid,
		Realm:        realm,
		AccessToken:  strings.TrimSpace(accessToken),
		RefreshToken: strings.TrimSpace(refreshToken),
		ExpiresAt:    JWTExp(accessToken),
		ProxySlot:    strings.TrimSpace(proxySlot),
		Enabled:      true,
	}
	if dir != "" {
		acct.Path = filepath.Join(dir, Filename(uid))
	}
	return acct
}
