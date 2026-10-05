// Package codearts speaks to Huawei CodeArts, the third upstream.
//
// It is unrelated to the WorkBuddy / CodeBuddy realms: different hosts, a
// different auth model, and a different quota system. WorkBuddy authenticates
// with a bearer token that is refreshed directly; CodeArts hand out short-lived
// STS credentials (an access key, a secret key and a security token) which must
// then be used to sign every request with the Huawei SDK-HMAC-SHA256 scheme.
//
// Layered on top of that signature is DPoP (RFC 9449): each request also carries
// a proof-of-possession JWT bound to the request's method and URL, signed by a
// per-account P-256 key. The private key is generated once and persisted with
// the credential, because upstream remembers the key it first saw.
//
// The flow, end to end:
//
//	login    PKCE S256 authorize in a browser -> loopback callback -> code
//	         -> STS exchange -> AK/SK/security_token
//	use      sign each request (SDK-HMAC-SHA256 + DPoP) and call snap-access
//	refresh  re-exchange before the STS credential expires
//
// The credential JSON this reads and writes is documented in
// docs/CODEARTS-REVERSE-NOTES.md; it is the same shape the desktop client uses,
// so an existing account file can be dropped in unchanged.
package codearts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
)

// Endpoints. The region is fixed at cn-north-4 because that is the only one the
// desktop client has been observed to use; a different region would need its own
// host, and nothing in the credential indicates which.
const (
	STSHost    = "https://sts.cn-north-4.myhuaweicloud.com"
	SnapHost   = "https://snap-access.cn-north-4.myhuaweicloud.com"
	PortalHost = "https://codearts.huaweicloud.com"
	OpenGWHost = "https://opengw.developer.huaweicloud.com"

	PathSTSTokens       = "/v1/oauth2/tokens"
	PathLoginTicket     = "/snap-manager/v1/login/ticket"
	PathPluginStats     = "/snap-manager/v1/statistics/plugin"
	PathSnapTokens      = "/v1/oauth2/tokens"
	PathBuiltinModels   = "/v1/model/builtin"
	PathChatCompletions = "/api/v2/chat/completions"
	PathOpsClaim        = "/v1/ops/claim"
	PathOpsConfirm      = "/v1/ops/confirm"
	PathOpsDelivery     = "/v1/ops/delivery"
	PathStats           = "/v1/stats"
	PathGatewayConfig   = "/api/v1/gateway/config"

	PathPortalLogin     = "/portal/login"
	PathPortalAuthorize = "/portal/authorize"

	// SignAlgorithm is the Huawei SDK's signing scheme. It is not AWS SigV4:
	// the canonical request and the header set differ, so the AWS signer cannot
	// be substituted.
	SignAlgorithm = "SDK-HMAC-SHA256"

	// AuthorizationHeader carries the signature.
	AuthorizationHeader = "Authorization"
	// SecurityTokenHeader carries the STS security token with each signed call.
	SecurityTokenHeader = "X-Security-Token"

	// SignTimeFormat is ISO8601 basic, as the Huawei SDK emits it.
	SignTimeFormat = "20060102T150405Z"
)

// Errors callers are expected to branch on.
var (
	// ErrNotSupported means the account has no CodeArts credential.
	ErrNotSupported = errors.New("codearts: no credential on account")
	// ErrReLoginRequired means the refresh material is gone and only a fresh
	// interactive login can recover the account.
	ErrReLoginRequired = errors.New("codearts: credential not usable (re-login required)")
	// ErrExpiredRefreshToken means the stored refresh token is past its life.
	ErrExpiredRefreshToken = errors.New("codearts: refresh token expired")
)

// Credential is one account's CodeArts material.
//
// The three secret fields together are what the Huawei signer needs; ExpiresAt
// applies to SecurityToken only, because AK/SK are long-lived while the security
// token is issued per STS session.
type Credential struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SecurityToken   string `json:"security_token"`
	ExpiresAt       string `json:"expires_at"`
	UserName        string `json:"user_name,omitempty"`
	UserID          string `json:"user_id,omitempty"`
	DomainID        string `json:"domain_id,omitempty"`

	// DpopPrivateJwk is the P-256 key this account signs DPoP proofs with. It is
	// persisted because upstream associates the key with the session; rotating
	// it on every start would make every request look like a new client.
	DpopPrivateJwk json.RawMessage `json:"dpop_private_key_jwk,omitempty"`
}

// Expiry parses ExpiresAt, tolerating the several layouts Huawei uses.
//
// A value that cannot be parsed returns the zero time, which callers treat as
// "unknown" and therefore refresh immediately. Returning an error here would
// just force every caller to handle it the same way.
func (c *Credential) Expiry() time.Time {
	raw := strings.TrimSpace(c.ExpiresAt)
	if raw == "" {
		return time.Time{}
	}
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t
		}
	}
	return time.Time{}
}

// Usable reports whether the credential has everything the signer needs.
func (c *Credential) Usable() bool {
	return c != nil &&
		strings.TrimSpace(c.AccessKeyID) != "" &&
		strings.TrimSpace(c.SecretAccessKey) != ""
}

// NeedsRefresh reports whether the STS credential is missing, unparseable, or
// within skew of expiring.
//
// An unparseable expiry counts as needing refresh: the alternative is to keep
// using a credential whose lifetime is unknown, and the failure mode there is a
// mid-request 403 rather than a clean refresh.
func (c *Credential) NeedsRefresh(now time.Time, skew time.Duration) bool {
	if !c.Usable() {
		return true
	}
	if strings.TrimSpace(c.SecurityToken) == "" {
		return true
	}
	expiry := c.Expiry()
	if expiry.IsZero() {
		return true
	}
	return !now.Add(skew).Before(expiry)
}

// PkcePair is one PKCE challenge/verifier pair (RFC 7636, method S256).
type PkcePair struct {
	CodeVerifier  string `json:"code_verifier"`
	CodeChallenge string `json:"code_challenge"`
}

// GeneratePkcePair mints a verifier and its S256 challenge.
//
// The verifier is 32 random bytes base64url-encoded without padding, which lands
// inside the RFC's 43..128 character window and is the width the desktop client
// uses.
func GeneratePkcePair() (PkcePair, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return PkcePair{}, fmt.Errorf("codearts: generate pkce verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf)

	sum := sha256.Sum256([]byte(verifier))
	return PkcePair{
		CodeVerifier:  verifier,
		CodeChallenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}

// DpopKeyPair is a P-256 key plus its JWK rendering.
//
// Only the private JWK is persisted; the public half is derived on load. Storing
// both would let them drift apart, and a mismatch is undetectable until upstream
// rejects a proof.
type DpopKeyPair struct {
	private *ecdsa.PrivateKey

	// mu guards reuse detection: a DPoP proof must carry a unique jti, and
	// upstream rejects replays, so the counter has to be monotonic per key.
	mu  sync.Mutex
	seq uint64
}

// GenerateDpopKeyPair mints a fresh P-256 key.
func GenerateDpopKeyPair() (*DpopKeyPair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("codearts: generate dpop key: %w", err)
	}
	return &DpopKeyPair{private: key}, nil
}

// jwk is the JSON Web Key form of a P-256 public key.
type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	D   string `json:"d,omitempty"`
}

func (k *DpopKeyPair) publicJWK() jwk {
	// ecdsa public keys are stored as a big.Int pair; JWK wants each coordinate
	// as fixed-width base64url, so the byte slices are left-padded to the curve
	// size rather than trimmed — a short coordinate would produce an invalid JWK.
	size := (k.private.Curve.Params().BitSize + 7) / 8
	x := k.private.PublicKey.X.Bytes()
	y := k.private.PublicKey.Y.Bytes()
	return jwk{
		Kty: "EC",
		Crv: "P-256",
		X:   base64.RawURLEncoding.EncodeToString(leftPad(x, size)),
		Y:   base64.RawURLEncoding.EncodeToString(leftPad(y, size)),
	}
}

// MarshalJSON emits the private JWK, which is what gets persisted.
func (k *DpopKeyPair) MarshalJSON() ([]byte, error) {
	if k == nil || k.private == nil {
		return []byte("null"), nil
	}
	size := (k.private.Curve.Params().BitSize + 7) / 8
	j := k.publicJWK()
	j.D = base64.RawURLEncoding.EncodeToString(
		leftPad(k.private.D.Bytes(), size))
	return json.Marshal(j)
}

// UnmarshalJSON loads a persisted private JWK.
func (k *DpopKeyPair) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	var in jwk
	if err := json.Unmarshal(data, &in); err != nil {
		return fmt.Errorf("codearts: invalid dpop jwk: %w", err)
	}
	if in.Kty != "EC" || in.Crv != "P-256" {
		return fmt.Errorf("codearts: invalid dpop jwk: want EC/P-256, got %s/%s", in.Kty, in.Crv)
	}
	x, err := base64.RawURLEncoding.DecodeString(in.X)
	if err != nil {
		return fmt.Errorf("codearts: decode dpop x: %w", err)
	}
	y, err := base64.RawURLEncoding.DecodeString(in.Y)
	if err != nil {
		return fmt.Errorf("codearts: decode dpop y: %w", err)
	}
	d, err := base64.RawURLEncoding.DecodeString(in.D)
	if err != nil {
		return fmt.Errorf("codearts: decode dpop d: %w", err)
	}

	curve := elliptic.P256()
	priv := new(ecdsa.PrivateKey)
	priv.Curve = curve
	priv.D = new(big.Int).SetBytes(d)
	priv.PublicKey.X = new(big.Int).SetBytes(x)
	priv.PublicKey.Y = new(big.Int).SetBytes(y)
	if !curve.IsOnCurve(priv.PublicKey.X, priv.PublicKey.Y) {
		return errors.New("codearts: invalid dpop jwk: point not on curve")
	}
	k.private = priv
	return nil
}

// leftPad pads b to width n with leading zero bytes.
func leftPad(b []byte, n int) []byte {
	if len(b) >= n {
		return b
	}
	out := make([]byte, n)
	copy(out[n-len(b):], b)
	return out
}

// nextJTI returns a unique, monotonic identifier for a DPoP proof.
func (k *DpopKeyPair) nextJTI() string {
	k.mu.Lock()
	k.seq++
	seq := k.seq
	k.mu.Unlock()

	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d-%d", time.Now().UnixNano(), seq)
	}
	return hex.EncodeToString(buf)
}
