package codearts

import (
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// SignedHeaders are sorted, lowercased header names covered by the signature.
//
// Host is included because the canonical request the Huawei SDK builds covers
// it; omitting it makes the signature verify against a different string than
// upstream constructs.
var SignedHeaders = []string{
	"content-type",
	"host",
	"x-content-sha256",
	"x-security-token",
}

// SignRequestHuawei signs req in place using the Huawei SDK-HMAC-SHA256 scheme.
//
// The scheme is deliberately not AWS SigV4, despite the surface resemblance:
// the canonical request is assembled as
//
//	Method\nCanonicalURI\nCanonicalQueryString\nCanonicalHeaders\nSignedHeaders\nHashedPayload
//
// where CanonicalHeaders is "name:value\n" for each signed header in sorted
// order, and the payload hash is the lowercase hex SHA-256 of the body, also
// echoed in the X-Content-Sha256 header. The signing key is the secret key
// itself — there is no date-scoped key derivation chain, which is the single
// biggest divergence from SigV4.
func SignRequestHuawei(req *http.Request, cred *Credential, body []byte, now time.Time) error {
	if cred == nil || !cred.Usable() {
		return ErrNotSupported
	}

	signTime := now.UTC().Format(SignTimeFormat)
	req.Header.Set("X-Sdk-Date", signTime)

	payloadHash := sha256Hex(body)
	req.Header.Set("X-Content-Sha256", payloadHash)

	if ct := req.Header.Get("Content-Type"); ct == "" {
		// The signer covers content-type, so it must be fixed before hashing the
		// canonical request rather than left to the server to infer.
		req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	}
	if cred.SecurityToken != "" {
		req.Header.Set(SecurityTokenHeader, cred.SecurityToken)
	}

	canonical := buildCanonicalRequest(req, payloadHash)
	signedHeaders := strings.Join(SignedHeaders, ";")

	stringToSign := strings.Join([]string{
		SignAlgorithm,
		signTime,
		sha256Hex([]byte(canonical)),
	}, "\n")

	mac := hmac.New(sha256.New, []byte(cred.SecretAccessKey))
	mac.Write([]byte(stringToSign))
	signature := hexEncode(mac.Sum(nil))

	req.Header.Set(AuthorizationHeader, fmt.Sprintf(
		"%s Access=%s, SignedHeaders=%s, Signature=%s",
		SignAlgorithm, cred.AccessKeyID, signedHeaders, signature))
	return nil
}

// buildCanonicalRequest assembles the string the signature is computed over.
func buildCanonicalRequest(req *http.Request, payloadHash string) string {
	uri := canonicalURI(req.URL)
	query := canonicalQuery(req.URL)

	var headers strings.Builder
	for _, name := range SignedHeaders {
		value := headerValue(req, name)
		headers.WriteString(name)
		headers.WriteByte(':')
		headers.WriteString(strings.TrimSpace(value))
		headers.WriteByte('\n')
	}

	return strings.Join([]string{
		req.Method,
		uri,
		query,
		headers.String(),
		strings.Join(SignedHeaders, ";"),
		payloadHash,
	}, "\n")
}

// canonicalURI returns the path with each segment percent-encoded.
//
// url.URL.EscapedPath already encodes most things, but it leaves a trailing
// slash off an empty path; upstream canonicalises "" to "/", and a mismatch
// there breaks every root-path signature.
func canonicalURI(u *url.URL) string {
	path := u.EscapedPath()
	if path == "" {
		return "/"
	}
	return path
}

// canonicalQuery sorts the query parameters and re-encodes them.
//
// Sorting is by encoded name then encoded value, which is not the same as
// sorting the decoded names: an encoded name that sorts differently would
// produce a signature upstream does not reproduce.
func canonicalQuery(u *url.URL) string {
	values := u.Query()
	if len(values) == 0 {
		return ""
	}
	type pair struct{ k, v string }
	pairs := make([]pair, 0, len(values))
	for k, vs := range values {
		for _, v := range vs {
			pairs = append(pairs, pair{
				k: huaweiEncode(k),
				v: huaweiEncode(v),
			})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})

	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p.k)
		b.WriteByte('=')
		b.WriteString(p.v)
	}
	return b.String()
}

// huaweiEncode percent-encodes a query component the way the Huawei SDK does.
//
// Go's url.QueryEscape encodes space as "+", but the canonical form requires
// %20, and it leaves "~" alone which the SDK also encodes. Both differences
// change the signature.
func huaweiEncode(s string) string {
	encoded := url.QueryEscape(s)
	encoded = strings.ReplaceAll(encoded, "+", "%20")
	encoded = strings.ReplaceAll(encoded, "%7E", "~")
	return encoded
}

// headerValue reads a header case-insensitively, synthesising the host value.
func headerValue(req *http.Request, name string) string {
	if name == "host" {
		if h := req.Header.Get("Host"); h != "" {
			return h
		}
		if req.Host != "" {
			return req.Host
		}
		return req.URL.Host
	}
	return req.Header.Get(name)
}

// sha256Hex returns the lowercase hex SHA-256 of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hexEncode(sum[:])
}

// hexEncode hex-encodes b.
func hexEncode(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = digits[c>>4]
		out[i*2+1] = digits[c&0x0f]
	}
	return string(out)
}

// ------------------------------------------------------------------- DPoP

// SignDpopJws builds a DPoP proof JWT for one request (RFC 9449).
//
// The proof is an ES256 JWT whose header carries the public key and whose claims
// bind the token to this exact request:
//
//	htm  the HTTP method
//	htu  the request URL, without query or fragment
//	jti  a unique identifier, so a replay is detectable
//	iat  issue time
//	ath  the SHA-256 of the access token, when one is being presented
//
// Signed with the account's persisted P-256 key, because upstream remembers the
// key it first saw for a session.
func SignDpopJws(key *DpopKeyPair, method, rawURL, accessToken string, now time.Time) (string, error) {
	if key == nil || key.private == nil {
		return "", fmt.Errorf("codearts: sign dpop: %w", ErrNotSupported)
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("codearts: parse url: %w", err)
	}
	// The htu claim must not carry a query string or fragment; upstream compares
	// it against the request line it receives.
	htu := fmt.Sprintf("%s://%s%s", parsed.Scheme, parsed.Host, parsed.Path)

	header := map[string]any{
		"typ": "dpop+jwt",
		"alg": "ES256",
		"jwk": key.publicJWK(),
	}

	claims := map[string]any{
		"htm": strings.ToUpper(method),
		"htu": htu,
		"jti": key.nextJTI(),
		"iat": now.Unix(),
	}
	if accessToken != "" {
		sum := sha256.Sum256([]byte(accessToken))
		claims["ath"] = base64.RawURLEncoding.EncodeToString(sum[:])
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("codearts: sign dpop: %w", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("codearts: sign dpop: %w", err)
	}

	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON)

	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key.private, digest[:])
	if err != nil {
		return "", fmt.Errorf("codearts: sign dpop: %w", err)
	}

	// JWS wants the raw r||s concatenation at the curve width, not the DER
	// encoding ecdsa.Sign produces.
	size := (key.private.Curve.Params().BitSize + 7) / 8
	sig := append(leftPad(r.Bytes(), size), leftPad(s.Bytes(), size)...)

	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// ApplyDpop attaches a DPoP proof to an outgoing request.
func ApplyDpop(req *http.Request, key *DpopKeyPair, accessToken string, now time.Time) error {
	proof, err := SignDpopJws(key, req.Method, req.URL.String(), accessToken, now)
	if err != nil {
		return err
	}
	req.Header.Set("DPoP", proof)
	return nil
}

// randHex returns n random bytes hex-encoded, for identifiers and nonces.
func randHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// A failing CSPRNG is not recoverable here and the caller only wanted a
		// nonce; fall back to a time-derived value rather than failing the call.
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hexEncode(buf)
}

// randomInt63n returns a uniform value in [0,n) or 0 when n is not positive.
func randomInt63n(n int64) int64 {
	if n <= 0 {
		return 0
	}
	max := big.NewInt(n)
	v, err := rand.Int(rand.Reader, max)
	if err != nil {
		return 0
	}
	return v.Int64()
}
