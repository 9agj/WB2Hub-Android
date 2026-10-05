package multikey

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// newStoreIn builds a store over a path inside a fresh temp directory.
func newStoreIn(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api_keys.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s, path
}

// legacySecret stands in for the value the current build hard-codes.
const legacySecret = "wb2hub-local-key"

func TestAuthenticateAcceptsRightSecretAndRejectsOthers(t *testing.T) {
	s, _ := newStoreIn(t)
	added, err := s.Add("phone", RealmCN)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, ok := s.Authenticate(added.Secret)
	if !ok {
		t.Fatalf("correct secret was rejected")
	}
	if got.ID != added.ID {
		t.Fatalf("authenticated as %q, want %q", got.ID, added.ID)
	}
	if got.Realm != RealmCN {
		t.Fatalf("realm = %q, want %q", got.Realm, RealmCN)
	}

	wrong := []string{
		"",
		"   ",
		legacySecret,
		added.Secret + "x",
		added.Secret[:len(added.Secret)-1],
		"x" + added.Secret,
		strings.ToUpper(added.Secret),
		"sk-wb2hub-not-a-real-key",
	}
	for _, candidate := range wrong {
		if _, ok := s.Authenticate(candidate); ok {
			t.Errorf("Authenticate(%q) accepted a wrong secret", candidate)
		}
	}
}

func TestAuthenticateReturnsACopyNotStoreState(t *testing.T) {
	s, _ := newStoreIn(t)
	added, _ := s.Add("laptop", RealmIntl)

	first, ok := s.Authenticate(added.Secret)
	if !ok {
		t.Fatal("authenticate failed")
	}
	first.Name = "tampered"
	first.Realm = RealmCN
	first.Usage.Requests = 999

	second, _ := s.Authenticate(added.Secret)
	if second.Name != "laptop" || second.Realm != RealmIntl || second.Usage.Requests != 0 {
		t.Fatalf("caller mutation leaked into the store: %+v", second)
	}
}

func TestAuthenticateStampsLastUsedAndSkipsDisabled(t *testing.T) {
	s, path := newStoreIn(t)
	added, _ := s.Add("tablet", RealmAny)

	if !added.LastUsedAt.IsZero() {
		t.Fatalf("a fresh key already claims to have been used: %v", added.LastUsedAt)
	}
	if _, ok := s.Authenticate(added.Secret); !ok {
		t.Fatal("authenticate failed")
	}

	reloaded, err := NewStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	stored, ok := reloaded.Get(added.ID)
	if !ok {
		t.Fatal("key did not survive the reload")
	}
	if stored.LastUsedAt.IsZero() {
		t.Fatal("LastUsedAt was not persisted")
	}

	if err := s.SetEnabled(added.ID, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if _, ok := s.Authenticate(added.Secret); ok {
		t.Fatal("a disabled key still authenticates")
	}
	if err := s.SetEnabled(added.ID, true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if _, ok := s.Authenticate(added.Secret); !ok {
		t.Fatal("re-enabled key does not authenticate")
	}
}

func TestUsageAccumulatesAndResets(t *testing.T) {
	s, path := newStoreIn(t)
	added, _ := s.Add("default", RealmAny)

	for _, step := range []struct {
		prompt, completion int64
		credits            float64
	}{
		{10, 5, 0.25},
		{20, 7, 0.75},
		{0, 0, 1.0},
	} {
		if err := s.RecordUsage(added.ID, step.prompt, step.completion, step.credits); err != nil {
			t.Fatalf("RecordUsage: %v", err)
		}
	}

	key, _ := s.Get(added.ID)
	want := Usage{Requests: 3, PromptTokens: 30, CompletionTokens: 12, TotalTokens: 42, Credits: 2.0}
	if key.Usage != want {
		t.Fatalf("usage = %+v, want %+v", key.Usage, want)
	}

	reloaded, err := NewStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	persisted, _ := reloaded.Get(added.ID)
	if persisted.Usage != want {
		t.Fatalf("persisted usage = %+v, want %+v", persisted.Usage, want)
	}

	if err := s.ResetUsage(added.ID); err != nil {
		t.Fatalf("ResetUsage: %v", err)
	}
	reset, _ := s.Get(added.ID)
	if reset.Usage != (Usage{}) {
		t.Fatalf("usage after reset = %+v, want zero", reset.Usage)
	}
	if reset.Secret != added.Secret {
		t.Fatal("ResetUsage wiped the secret")
	}
}

func TestRecordUsageRejectsNegativeCounts(t *testing.T) {
	s, _ := newStoreIn(t)
	added, _ := s.Add("default", RealmAny)
	if err := s.RecordUsage(added.ID, -1, 5, 0); err == nil {
		t.Fatal("a negative prompt count was accepted")
	}
}

func TestEnsureSeedMigratesLegacyKey(t *testing.T) {
	s, path := newStoreIn(t)

	if err := s.EnsureSeed(legacySecret); err != nil {
		t.Fatalf("EnsureSeed: %v", err)
	}

	keys := s.List()
	if len(keys) != 1 {
		t.Fatalf("seeded %d keys, want 1", len(keys))
	}
	got := keys[0]
	if got.ID != "default" || got.Name != "default" {
		t.Fatalf("seeded key = %+v, want the default row", got)
	}
	// Verbatim: an existing client configured with the old key must keep
	// working, which is the whole reason to migrate rather than re-mint.
	if got.Secret != legacySecret {
		t.Fatalf("legacy secret = %q, want it preserved verbatim", got.Secret)
	}
	if got.Realm != RealmAny {
		t.Fatalf("legacy realm = %q, want unbound", got.Realm)
	}
	if !got.Enabled {
		t.Fatal("migrated key arrived disabled")
	}

	if _, ok := s.Authenticate(legacySecret); !ok {
		t.Fatal("the legacy key does not authenticate after migration")
	}

	reloaded, err := NewStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded.List()) != 1 {
		t.Fatal("the seeded key did not survive a reload")
	}
}

func TestEnsureSeedIsIdempotentAndDoesNotResurrect(t *testing.T) {
	s, _ := newStoreIn(t)
	if err := s.EnsureSeed(legacySecret); err != nil {
		t.Fatalf("first EnsureSeed: %v", err)
	}
	if err := s.EnsureSeed("some-other-key"); err != nil {
		t.Fatalf("second EnsureSeed: %v", err)
	}
	if keys := s.List(); len(keys) != 1 || keys[0].Secret != legacySecret {
		t.Fatalf("a second seed changed the store: %+v", keys)
	}

	// A blank legacy value is nothing to migrate.
	empty, emptyPath := newStoreIn(t)
	if err := empty.EnsureSeed("   "); err != nil {
		t.Fatalf("EnsureSeed(blank): %v", err)
	}
	if _, err := os.Stat(emptyPath); !os.IsNotExist(err) {
		t.Fatal("a blank legacy key created a store file")
	}

	// Deleting every key must not be undone by the next start.
	added, _ := newStoreIn(t)
	if err := added.Remove((func() string { k, _ := added.Add("only", RealmAny); return k.ID })()); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := added.EnsureSeed(legacySecret); err != nil {
		t.Fatalf("EnsureSeed after removal: %v", err)
	}
	if keys := added.List(); len(keys) != 0 {
		t.Fatalf("an emptied store was re-seeded: %+v", keys)
	}
}

func TestMaskedHidesTheSecretWithoutLosingThePattern(t *testing.T) {
	secret := GenerateSecret()
	masked := Key{Secret: secret}.Masked()

	// The hidden middle is everything the stub does not show, and it must not
	// appear anywhere in the masked value.
	if strings.Contains(masked.Secret, secret[9:len(secret)-4]) {
		t.Fatalf("masked value still exposes the body: %q", masked.Secret)
	}
	if !strings.HasPrefix(masked.Secret, SecretPrefix[:len(SecretPrefix)-1]) {
		t.Fatalf("masked value = %q, want the recognisable prefix kept", masked.Secret)
	}
	if !strings.HasSuffix(masked.Secret, secret[len(secret)-4:]) {
		t.Fatalf("masked value = %q, want the tail kept so keys are tellable apart", masked.Secret)
	}
	// Whatever a masked value is, it must never authenticate.
	if MaskSecret(secret) == secret {
		t.Fatal("MaskSecret returned the secret unchanged")
	}
	if MaskSecret("") != "" {
		t.Fatal("an empty secret should mask to empty")
	}
	if got := MaskSecret("short"); got != "****" {
		t.Fatalf("MaskSecret(short) = %q, want all stars", got)
	}
	if got := MaskSecret("12345678"); got != "****" {
		t.Fatalf("MaskSecret(8 chars) = %q, want all stars", got)
	}
}

func TestGeneratedSecretsAreUniqueAndURLSafe(t *testing.T) {
	seen := make(map[string]bool, 512)
	for i := 0; i < 512; i++ {
		secret := GenerateSecret()
		if !strings.HasPrefix(secret, SecretPrefix) {
			t.Fatalf("secret %q lacks the prefix", secret)
		}
		if len(secret) <= len(SecretPrefix)+16 {
			t.Fatalf("secret %q is too short to resist guessing", secret)
		}
		for _, r := range strings.TrimPrefix(secret, SecretPrefix) {
			if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_", r) {
				t.Fatalf("secret %q is not URL-safe", secret)
			}
		}
		if seen[secret] {
			t.Fatalf("duplicate secret after %d draws", i)
		}
		seen[secret] = true
	}
}

func TestRealmValidation(t *testing.T) {
	s, _ := newStoreIn(t)
	if _, err := s.Add("bad", "moon"); err == nil {
		t.Fatal("an unknown realm was accepted")
	}
	added, err := s.Add("lenient", "  CN  ")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if added.Realm != RealmCN {
		t.Fatalf("realm = %q, want it normalized to %q", added.Realm, RealmCN)
	}
	if err := s.SetRealm(added.ID, "intl"); err != nil {
		t.Fatalf("SetRealm: %v", err)
	}
	if err := s.SetRealm(added.ID, "jupiter"); err == nil {
		t.Fatal("SetRealm accepted an unknown realm")
	}
	current, _ := s.Get(added.ID)
	if current.Realm != RealmIntl {
		t.Fatalf("a rejected SetRealm changed the realm to %q", current.Realm)
	}
}

func TestMutationsReportMissingKeys(t *testing.T) {
	s, _ := newStoreIn(t)
	if err := s.Remove("nope"); err == nil {
		t.Fatal("Remove accepted an unknown id")
	}
	if err := s.SetEnabled("nope", true); err == nil {
		t.Fatal("SetEnabled accepted an unknown id")
	}
	if err := s.Rename("nope", "x"); err == nil {
		t.Fatal("Rename accepted an unknown id")
	}
	if err := s.RecordUsage("nope", 1, 1, 0); err == nil {
		t.Fatal("RecordUsage accepted an unknown id")
	}
}

func TestPersistenceIsAtomicAndCorruptFileIsPreserved(t *testing.T) {
	s, path := newStoreIn(t)
	added, _ := s.Add("default", RealmAny)

	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("the temp file was left behind by a successful write")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if err := json.Unmarshal(raw, &struct {
		Keys []Key `json:"keys"`
	}{}); err != nil {
		t.Fatalf("store is not valid JSON: %v", err)
	}
	// snake_case field names are the on-disk contract with the Python-era
	// tooling and the panel; a rename here is a silent data migration.
	for _, field := range []string{`"id"`, `"name"`, `"secret"`, `"realm"`, `"enabled"`,
		`"created_at"`, `"last_used_at"`, `"usage"`, `"requests"`, `"prompt_tokens"`,
		`"completion_tokens"`, `"total_tokens"`, `"credits"`} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("store JSON is missing the field %s", field)
		}
	}

	// Corrupt the file: the old contents must survive as .bad and the store
	// must report the problem instead of silently starting empty.
	if err := os.WriteFile(path, []byte("{not json at all"), 0o600); err != nil {
		t.Fatalf("corrupt: %v", err)
	}
	if _, err := NewStore(path); err == nil {
		t.Fatal("NewStore accepted a corrupt file")
	}
	bad, err := os.ReadFile(path + ".bad")
	if err != nil {
		t.Fatalf("the unreadable file was not preserved: %v", err)
	}
	if string(bad) != "{not json at all" {
		t.Fatalf(".bad holds %q, want the original bytes", bad)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "{not json at all" {
		t.Fatal("the original file was overwritten instead of preserved")
	}
	_ = added
}

func TestConcurrentAccessStaysConsistent(t *testing.T) {
	s, path := newStoreIn(t)
	added, _ := s.Add("default", RealmAny)

	const workers = 16
	const perWorker = 40
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				if _, ok := s.Authenticate(added.Secret); !ok {
					t.Errorf("authenticate failed under contention")
					return
				}
				if err := s.RecordUsage(added.ID, 1, 1, 0.5); err != nil {
					t.Errorf("RecordUsage: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	key, _ := s.Get(added.ID)
	want := int64(workers * perWorker)
	if key.Usage.Requests != want || key.Usage.TotalTokens != 2*want {
		t.Fatalf("usage = %+v, want %d requests / %d tokens", key.Usage, want, 2*want)
	}

	reloaded, err := NewStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	persisted, _ := reloaded.Get(added.ID)
	if persisted.Usage.Requests != want {
		t.Fatalf("persisted requests = %d, want %d", persisted.Usage.Requests, want)
	}
}

func TestSnapshotAndListAgree(t *testing.T) {
	s, _ := newStoreIn(t)
	if _, err := s.Add("a", RealmIntl); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add("b", RealmCN); err != nil {
		t.Fatal(err)
	}
	if len(s.Snapshot()) != len(s.List()) {
		t.Fatal("Snapshot and List disagree")
	}
	masked := s.Snapshot()
	for i := range masked {
		masked[i] = masked[i].Masked()
		if strings.Contains(s.List()[i].Secret, "…") {
			t.Fatal("List returned a masked secret")
		}
	}
}
