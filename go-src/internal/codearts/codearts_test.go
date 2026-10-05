package codearts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testCredential() *Credential {
	return &Credential{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "SECRETEXAMPLE",
		SecurityToken:   "TOKENEXAMPLE",
		ExpiresAt:       time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}
}

// signAndAuthorize runs the signer against a request and returns the header it
// produced, so individual assertions stay readable.
func signAndAuthorize(t *testing.T, method, rawURL string, body []byte, headers map[string]string) string {
	t.Helper()
	req, err := http.NewRequest(method, rawURL, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if err := SignRequestHuawei(req, testCredential(), body, time.Now()); err != nil {
		t.Fatalf("sign: %v", err)
	}
	return req.Header.Get(AuthorizationHeader)
}

func TestSignRequestProducesHuaweiAuthorizationHeader(t *testing.T) {
	auth := signAndAuthorize(t, http.MethodGet, "https://example.com/v1/model/builtin", nil, nil)

	// The scheme name is load-bearing: upstream rejects a SigV4 header outright.
	if !strings.HasPrefix(auth, SignAlgorithm+" ") {
		t.Fatalf("authorization = %q, want prefix %q", auth, SignAlgorithm)
	}
	for _, part := range []string{"Access=AKIDEXAMPLE", "SignedHeaders=", "Signature="} {
		if !strings.Contains(auth, part) {
			t.Errorf("authorization %q is missing %q", auth, part)
		}
	}
}

func TestSignatureIsDeterministicForSameInput(t *testing.T) {
	// The signature covers the payload hash and the X-Sdk-Date header, so two
	// calls in the same second with the same body must agree; otherwise the
	// signer is leaking state and upstream will see mismatched retries.
	fixed := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	body := []byte(`{"model":"x"}`)

	build := func() string {
		req, _ := http.NewRequest(http.MethodPost, "https://example.com/api/v2/chat/completions",
			strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json;charset=UTF-8")
		if err := SignRequestHuawei(req, testCredential(), body, fixed); err != nil {
			t.Fatalf("sign: %v", err)
		}
		return req.Header.Get(AuthorizationHeader)
	}

	if first, second := build(), build(); first != second {
		t.Errorf("signature not deterministic:\n  %s\n  %s", first, second)
	}
}

func TestSignatureChangesWithBody(t *testing.T) {
	// A signer that ignores the payload hash would let a tampered body through,
	// so this is the assertion that the hash is actually covered.
	fixed := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

	sign := func(body string) string {
		req, _ := http.NewRequest(http.MethodPost, "https://example.com/v1/x", strings.NewReader(body))
		if err := SignRequestHuawei(req, testCredential(), []byte(body), fixed); err != nil {
			t.Fatalf("sign: %v", err)
		}
		return req.Header.Get(AuthorizationHeader)
	}

	if sign(`{"a":1}`) == sign(`{"a":2}`) {
		t.Error("signature did not change when the body changed")
	}
}

func TestSignatureChangesWithQueryOrder(t *testing.T) {
	// Canonicalisation must sort the query, which means two URLs that differ only
	// in parameter order sign identically. If they did not, proxies that reorder
	// parameters would break every request.
	fixed := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

	sign := func(rawURL string) string {
		req, _ := http.NewRequest(http.MethodGet, rawURL, nil)
		if err := SignRequestHuawei(req, testCredential(), nil, fixed); err != nil {
			t.Fatalf("sign: %v", err)
		}
		return req.Header.Get(AuthorizationHeader)
	}

	a := sign("https://example.com/v1/x?b=2&a=1")
	b := sign("https://example.com/v1/x?a=1&b=2")
	if a != b {
		t.Errorf("query order affected the signature:\n  %s\n  %s", a, b)
	}
}

func TestCanonicalQueryEncodesSpaceAsPercent20(t *testing.T) {
	// Go's url.QueryEscape emits "+" for a space; the canonical form requires
	// %20. Getting this wrong changes every signature that carries a space.
	u, err := url.Parse("https://example.com/x?q=a+b%20c")
	if err != nil {
		t.Fatal(err)
	}
	got := canonicalQuery(u)
	if strings.Contains(got, "+") {
		t.Errorf("canonical query = %q, must not contain '+'", got)
	}
	if !strings.Contains(got, "%20") {
		t.Errorf("canonical query = %q, want a percent-encoded space", got)
	}
}

func TestCanonicalURIFallsBackToRoot(t *testing.T) {
	u, _ := url.Parse("https://example.com")
	if got := canonicalURI(u); got != "/" {
		t.Errorf("canonicalURI = %q, want %q", got, "/")
	}
}

func TestSecurityTokenHeaderOnlyWhenPresent(t *testing.T) {
	// A credential without a security token must not send an empty header:
	// upstream reads presence as a signal and an empty value reads as a
	// malformed one.
	cred := testCredential()
	cred.SecurityToken = ""

	req, _ := http.NewRequest(http.MethodGet, "https://example.com/v1/x", nil)
	if err := SignRequestHuawei(req, cred, nil, time.Now()); err != nil {
		t.Fatalf("sign: %v", err)
	}
	if v := req.Header.Get(SecurityTokenHeader); v != "" {
		t.Errorf("%s = %q, want absent", SecurityTokenHeader, v)
	}
}

func TestSignRejectsUnusableCredential(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/x", nil)
	err := SignRequestHuawei(req, &Credential{}, nil, time.Now())
	if err == nil {
		t.Fatal("expected an error for a credential with no keys")
	}
	if !strings.Contains(err.Error(), "no credential") {
		t.Errorf("error = %v, want it to mention the missing credential", err)
	}
}

// ----------------------------------------------------------------- DPoP

func TestDpopProofIsWellFormedES256(t *testing.T) {
	key, err := GenerateDpopKeyPair()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	proof, err := SignDpopJws(key, http.MethodPost,
		"https://example.com/api/v2/chat/completions?x=1", "token-abc", time.Now())
	if err != nil {
		t.Fatalf("sign dpop: %v", err)
	}

	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		t.Fatalf("proof has %d segments, want 3", len(parts))
	}

	headerRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var header struct {
		Typ string `json:"typ"`
		Alg string `json:"alg"`
		JWK struct {
			Kty string `json:"kty"`
			Crv string `json:"crv"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"jwk"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if header.Typ != "dpop+jwt" {
		t.Errorf("typ = %q, want dpop+jwt", header.Typ)
	}
	if header.Alg != "ES256" {
		t.Errorf("alg = %q, want ES256", header.Alg)
	}
	if header.JWK.Kty != "EC" || header.JWK.Crv != "P-256" {
		t.Errorf("jwk = %s/%s, want EC/P-256", header.JWK.Kty, header.JWK.Crv)
	}
	if header.JWK.X == "" || header.JWK.Y == "" {
		t.Error("jwk is missing a coordinate")
	}
}

func TestDpopClaimsBindMethodAndURL(t *testing.T) {
	key, _ := GenerateDpopKeyPair()
	token := "access-token-value"

	proof, err := SignDpopJws(key, http.MethodPost,
		"https://example.com/api/v2/chat/completions?ignored=1#frag", token, time.Now())
	if err != nil {
		t.Fatalf("sign dpop: %v", err)
	}

	parts := strings.Split(proof, ".")
	claimsRaw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Htm string `json:"htm"`
		Htu string `json:"htu"`
		Jti string `json:"jti"`
		Iat int64  `json:"iat"`
		Ath string `json:"ath"`
	}
	if err := json.Unmarshal(claimsRaw, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}

	if claims.Htm != "POST" {
		t.Errorf("htm = %q, want POST", claims.Htm)
	}
	// The URL claim must drop the query and fragment; upstream compares it
	// against the request line it received.
	if claims.Htu != "https://example.com/api/v2/chat/completions" {
		t.Errorf("htu = %q, want the bare URL without query or fragment", claims.Htu)
	}
	if claims.Jti == "" {
		t.Error("jti is empty; a proof without one is replayable")
	}
	if claims.Iat == 0 {
		t.Error("iat is zero")
	}

	sum := sha256.Sum256([]byte(token))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if claims.Ath != want {
		t.Errorf("ath = %q, want %q", claims.Ath, want)
	}
}

func TestDpopProofIsVerifiableWithThePublishedKey(t *testing.T) {
	// This is the assertion that the signature is real rather than merely
	// well-shaped: verify it against the JWK the proof itself carries.
	key, _ := GenerateDpopKeyPair()
	proof, err := SignDpopJws(key, http.MethodGet, "https://example.com/v1/x", "", time.Now())
	if err != nil {
		t.Fatalf("sign dpop: %v", err)
	}

	parts := strings.Split(proof, ".")
	headerRaw, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var header struct {
		JWK struct {
			X string `json:"x"`
			Y string `json:"y"`
		} `json:"jwk"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	xBytes, _ := base64.RawURLEncoding.DecodeString(header.JWK.X)
	yBytes, _ := base64.RawURLEncoding.DecodeString(header.JWK.Y)

	pub := ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(xBytes),
		Y:     new(big.Int).SetBytes(yBytes),
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if len(sigBytes)%2 != 0 {
		t.Fatalf("signature length %d is odd; raw r||s must be even", len(sigBytes))
	}
	half := len(sigBytes) / 2
	r := new(big.Int).SetBytes(sigBytes[:half])
	s := new(big.Int).SetBytes(sigBytes[half:])

	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&pub, digest[:], r, s) {
		t.Error("DPoP signature did not verify against the key in its own header")
	}
}

func TestDpopJtiIsUniquePerProof(t *testing.T) {
	// Upstream rejects a replayed jti, so two proofs for the same request must
	// still differ.
	key, _ := GenerateDpopKeyPair()
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		proof, err := SignDpopJws(key, http.MethodGet, "https://example.com/v1/x", "", time.Now())
		if err != nil {
			t.Fatalf("sign dpop: %v", err)
		}
		claimsRaw, _ := base64.RawURLEncoding.DecodeString(strings.Split(proof, ".")[1])
		var claims struct {
			Jti string `json:"jti"`
		}
		_ = json.Unmarshal(claimsRaw, &claims)
		if seen[claims.Jti] {
			t.Fatalf("duplicate jti %q at iteration %d", claims.Jti, i)
		}
		seen[claims.Jti] = true
	}
}

func TestDpopOmitsAthWithoutToken(t *testing.T) {
	key, _ := GenerateDpopKeyPair()
	proof, _ := SignDpopJws(key, http.MethodGet, "https://example.com/x", "", time.Now())
	claimsRaw, _ := base64.RawURLEncoding.DecodeString(strings.Split(proof, ".")[1])
	if strings.Contains(string(claimsRaw), `"ath"`) {
		t.Errorf("claims carry ath without an access token: %s", claimsRaw)
	}
}

// ------------------------------------------------------- key persistence

func TestDpopKeyRoundTripsThroughJSON(t *testing.T) {
	// The key is persisted with the credential, so a failure to round-trip it
	// would make every restart look like a new client to upstream.
	original, _ := GenerateDpopKeyPair()
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var restored DpopKeyPair
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if restored.private == nil {
		t.Fatal("restored key has no private material")
	}
	if original.private.D.Cmp(restored.private.D) != 0 {
		t.Error("private scalar changed across the round trip")
	}
	if original.private.PublicKey.X.Cmp(restored.private.PublicKey.X) != 0 {
		t.Error("public X changed across the round trip")
	}

	// Both must produce the same public JWK, which is what upstream compares.
	before, _ := json.Marshal(original.publicJWK())
	after, _ := json.Marshal(restored.publicJWK())
	if string(before) != string(after) {
		t.Errorf("public JWK differs:\n  %s\n  %s", before, after)
	}
}

func TestDpopRejectsWrongCurve(t *testing.T) {
	bad := `{"kty":"EC","crv":"P-384","x":"AA","y":"AA","d":"AA"}`
	var key DpopKeyPair
	if err := json.Unmarshal([]byte(bad), &key); err == nil {
		t.Error("accepted a P-384 key; ES256 requires P-256")
	}
}

func TestDpopRejectsPointOffCurve(t *testing.T) {
	// A corrupt key must be caught at load, not at the first request.
	bad := `{"kty":"EC","crv":"P-256","x":"AAAA","y":"AAAA","d":"AAAA"}`
	var key DpopKeyPair
	err := json.Unmarshal([]byte(bad), &key)
	if err == nil {
		t.Error("accepted a point that is not on the curve")
	}
}

// ------------------------------------------------------------- PKCE

func TestPkceChallengeMatchesVerifier(t *testing.T) {
	pair, err := GeneratePkcePair()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	sum := sha256.Sum256([]byte(pair.CodeVerifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if pair.CodeChallenge != want {
		t.Errorf("challenge = %q, want %q", pair.CodeChallenge, want)
	}
	// RFC 7636 sets the verifier window at 43..128 characters.
	if n := len(pair.CodeVerifier); n < 43 || n > 128 {
		t.Errorf("verifier length %d is outside the RFC 7636 window", n)
	}
}

func TestPkcePairsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		pair, _ := GeneratePkcePair()
		if seen[pair.CodeVerifier] {
			t.Fatal("generated a duplicate verifier")
		}
		seen[pair.CodeVerifier] = true
	}
}

func TestStartOAuthFlowUsesS256(t *testing.T) {
	authURL, pkce, state, err := StartOAuthFlow("http://127.0.0.1:18080/callback")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := parsed.Query()

	if q.Get("code_challenge_method") != "SHA-256" {
		t.Errorf("code_challenge_method = %q, want SHA-256", q.Get("code_challenge_method"))
	}
	if q.Get("code_challenge") != pkce.CodeChallenge {
		t.Error("challenge in the URL does not match the returned pair")
	}
	if q.Get("state") != state || state == "" {
		t.Error("state is missing or inconsistent")
	}
	// The verifier must never leave the client; only its hash goes to the server.
	if strings.Contains(authURL, pkce.CodeVerifier) {
		t.Error("the verifier leaked into the authorization URL")
	}
}

// -------------------------------------------------------- credential parsing

func TestCredentialUsableRequiresBothKeys(t *testing.T) {
	cases := []struct {
		name string
		cred Credential
		want bool
	}{
		{"complete", Credential{AccessKeyID: "a", SecretAccessKey: "b"}, true},
		{"no secret", Credential{AccessKeyID: "a"}, false},
		{"no access key", Credential{SecretAccessKey: "b"}, false},
		{"blank strings", Credential{AccessKeyID: "  ", SecretAccessKey: "  "}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cred.Usable(); got != tc.want {
				t.Errorf("Usable() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNeedsRefreshTreatsUnparseableExpiryAsExpired(t *testing.T) {
	// Keeping a credential whose lifetime is unknown eventually surfaces as a
	// mid-request 403; refreshing early is the safer failure.
	cred := &Credential{
		AccessKeyID:     "a",
		SecretAccessKey: "b",
		SecurityToken:   "t",
		ExpiresAt:       "not-a-date",
	}
	if !cred.NeedsRefresh(time.Now(), 5*time.Minute) {
		t.Error("wanted a refresh for an unparseable expiry")
	}
}

func TestNeedsRefreshRespectsSkew(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

	soon := &Credential{
		AccessKeyID: "a", SecretAccessKey: "b", SecurityToken: "t",
		ExpiresAt: now.Add(2 * time.Minute).Format(time.RFC3339),
	}
	if !soon.NeedsRefresh(now, 5*time.Minute) {
		t.Error("a token expiring inside the skew window should refresh now")
	}

	later := &Credential{
		AccessKeyID: "a", SecretAccessKey: "b", SecurityToken: "t",
		ExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
	}
	if later.NeedsRefresh(now, 5*time.Minute) {
		t.Error("a token valid for an hour should not refresh")
	}
}

func TestParseCredentialJSONUnwrapsEnvelope(t *testing.T) {
	// Upstream wraps payloads under data/result; a top-level-only decode would
	// yield an empty credential and look like an auth failure.
	body := []byte(`{"data":{"access_key_id":"AK","secret_access_key":"SK","security_token":"ST"}}`)
	cred, err := parseCredentialJSON(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cred.AccessKeyID != "AK" || cred.SecretAccessKey != "SK" || cred.SecurityToken != "ST" {
		t.Errorf("got %+v", cred)
	}
}

func TestParseCredentialJSONDerivesExpiryFromExpiresIn(t *testing.T) {
	body := []byte(`{"access_key_id":"AK","secret_access_key":"SK","security_token":"ST","expires_in":3600}`)
	cred, err := parseCredentialJSON(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cred.ExpiresAt == "" {
		t.Fatal("ExpiresAt was not derived from expires_in")
	}
	expiry, err := time.Parse(time.RFC3339, cred.ExpiresAt)
	if err != nil {
		t.Fatalf("derived expiry is not RFC3339: %q", cred.ExpiresAt)
	}
	// Allow slack for the clock advancing during the test.
	if delta := time.Until(expiry); delta < 50*time.Minute || delta > 70*time.Minute {
		t.Errorf("derived expiry is %v away, want about an hour", delta)
	}
}

func TestParseCredentialJSONFailsWithoutKeys(t *testing.T) {
	if _, err := parseCredentialJSON([]byte(`{"message":"nope"}`)); err == nil {
		t.Error("expected an error when no key material is present")
	}
}

// --------------------------------------------------------- ticket polling

func TestParseTicketResponsePendingKeepsPolling(t *testing.T) {
	// "Not yet" must not be an error, or the login loop would abort on the first
	// poll instead of waiting for the user to finish in the browser.
	for _, body := range []string{
		`{"status":"pending"}`,
		`{"status":"waiting"}`,
		`{}`,
		`not json at all`,
	} {
		cred, done, err := parseTicketResponse(http.StatusOK, []byte(body))
		if err != nil || done || cred != nil {
			t.Errorf("body %q: cred=%v done=%v err=%v; want pending", body, cred, done, err)
		}
	}
}

func TestParseTicketResponseNotFoundKeepsPolling(t *testing.T) {
	cred, done, err := parseTicketResponse(http.StatusNotFound, nil)
	if err != nil || done || cred != nil {
		t.Errorf("404 should be treated as pending, got cred=%v done=%v err=%v", cred, done, err)
	}
}

func TestParseTicketResponseTerminalFailureStops(t *testing.T) {
	cred, done, err := parseTicketResponse(http.StatusOK,
		[]byte(`{"status":"failed","message":"denied"}`))
	if !done || err == nil || cred != nil {
		t.Errorf("a failed ticket should stop with an error, got cred=%v done=%v err=%v",
			cred, done, err)
	}
}

func TestParseTicketResponseSuccessYieldsCredential(t *testing.T) {
	body := []byte(`{"status":"ok","access_key_id":"AK","secret_access_key":"SK","security_token":"ST"}`)
	cred, done, err := parseTicketResponse(http.StatusOK, body)
	if err != nil || !done {
		t.Fatalf("cred=%v done=%v err=%v", cred, done, err)
	}
	if cred == nil || cred.AccessKeyID != "AK" {
		t.Fatalf("got %+v", cred)
	}
	if cred.ExpiresAt == "" {
		t.Error("a credential with no expiry should get a conservative default")
	}
}

func TestParseTicketResponseUnwrapsCredentialEnvelope(t *testing.T) {
	body := []byte(`{"status":"ok","data":{"access_key_id":"AK","secret_access_key":"SK","security_token":"ST"}}`)
	cred, done, err := parseTicketResponse(http.StatusOK, body)
	if err != nil || !done || cred == nil {
		t.Fatalf("cred=%v done=%v err=%v", cred, done, err)
	}
	if cred.AccessKeyID != "AK" {
		t.Errorf("access key = %q, want AK", cred.AccessKeyID)
	}
}

// ------------------------------------------------------------ error typing

func TestIsRefreshTokenExpiredRecognisesMarkers(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{ErrExpiredRefreshToken, true},
		{ErrReLoginRequired, true},
		{errString("oauth: invalid_grant"), true},
		{errString("refresh materials missing"), true},
		{errString("connection reset by peer"), false},
	}
	for _, tc := range cases {
		if got := IsRefreshTokenExpired(tc.err); got != tc.want {
			t.Errorf("IsRefreshTokenExpired(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestIsAuthErrorDistinguishesAuthFromTransport(t *testing.T) {
	if !IsAuthError(http.StatusUnauthorized, nil) {
		t.Error("401 should count as an auth error")
	}
	if !IsAuthError(http.StatusForbidden, nil) {
		t.Error("403 should count as an auth error")
	}
	if !IsAuthError(http.StatusOK, []byte("SignatureDoesNotMatch: signature does not match")) {
		t.Error("a signature mismatch in the body should count as an auth error")
	}
	if IsAuthError(http.StatusBadGateway, []byte("upstream timeout")) {
		t.Error("a transport failure must not cool the account as an auth error")
	}
}

func TestIsQueueErrorCode(t *testing.T) {
	if !IsQueueErrorCode("queue_full") {
		t.Error("queue_full should be recognised")
	}
	if !IsQueueErrorCode("BUSY") {
		t.Error("the check should be case-insensitive")
	}
	if IsQueueErrorCode("invalid_parameter") {
		t.Error("an invalid parameter is not a queue condition")
	}
}

// ------------------------------------------------------------ helpers

type errString string

func (e errString) Error() string { return string(e) }
