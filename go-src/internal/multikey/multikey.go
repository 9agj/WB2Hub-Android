// Package multikey holds the gateway's client credentials: the secrets that
// callers present, the realm each one is bound to, and the spend attributed to
// each of them.
//
// The Python reference kept a single `api_key` in settings.json, which made
// every client share one name, one outbound realm and one usage record. Real
// installs do not work that way: a desktop client and a phone pointed at
// different exits need different keys, and the operator wants to see which of
// them burned the quota. A key therefore carries its own realm and its own
// counters.
//
// Keys are stored in their own file rather than in settings.json. The file is
// written whole (temp file + rename) because it holds plaintext secrets: a
// crash midway through a rewrite would otherwise truncate every credential the
// install has. A file that no longer parses is preserved as `<path>.bad`
// instead of being overwritten, so a hand-edit mistake costs a restart, not the
// keys themselves — the same recovery shape wb_settings.py uses.
//
// Usage counters live in the same record as the key so the admin panel reads
// one document and the numbers cannot outlive the key that earned them.
//
// Ported from the API-key section of wb_settings.py and the `_key_id()` /
// usage attribution in wb_proxy.py.
package multikey

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SecretPrefix marks a generated client secret. It is the OpenAI convention
// ("sk-") with the gateway's own name attached, so a leaked key is recognisable
// in a log or a screenshot instead of looking like opaque noise.
const SecretPrefix = "sk-wb2hub-"

// secretEntropyBytes is 32 bytes of CSPRNG output before encoding. A client
// secret is a bearer credential for the account's upstream quota, so it is
// sized for guessing resistance, not for shortness.
const secretEntropyBytes = 32

// Realm bounds. An unbound key ("" realm) follows whatever the gateway's
// global switch is set to, which is what every key written before realms
// existed behaves as.
const (
	RealmAny  = ""
	RealmIntl = "intl"
	RealmCN   = "cn"
)

// Realms is every realm a key may be bound to, in the order the panel shows.
//
// It is exported as a slice rather than a set because validating a submitted
// realm and rendering a dropdown want the same list in the same order.
var Realms = []string{RealmAny, RealmIntl, RealmCN}

// Errors returned by the store. They are values rather than formatted strings
// so a caller can map "no such key" onto a 404 without parsing text.
var (
	// ErrNoSuchKey is returned when an id does not resolve to a live key.
	ErrNoSuchKey = errors.New("multikey: no such key")
	// ErrDuplicateSecret is returned when a newly minted secret collides with
	// one already stored. It should be impossible; it is checked anyway so a
	// broken entropy source cannot silently produce two identical credentials.
	ErrDuplicateSecret = errors.New("multikey: duplicate secret")
	// ErrInvalidRealm is returned for a realm outside Realms. Accepting an
	// unknown realm would route the key through the default exit while the
	// panel displays the realm the operator typed.
	ErrInvalidRealm = errors.New("multikey: invalid realm")
)

// Usage is the spend attributed to one key.
//
// Credits is a float because the upstream bills fractional credits and a
// rounded total would drift against the account's own balance. The integers
// are the raw token counts the upstream reports per request.
type Usage struct {
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	Credits          float64 `json:"credits"`
}

// add accumulates one request into the usage record.
func (u Usage) add(prompt, completion int64, credits float64) Usage {
	u.Requests++
	u.PromptTokens += prompt
	u.CompletionTokens += completion
	// TotalTokens is summed from the parts rather than taken from the caller's
	// reply: when the upstream omits a total, the caller has nothing to pass,
	// and the two halves are always reported separately.
	u.TotalTokens += prompt + completion
	u.Credits += credits
	return u
}

// Key is one client credential and everything the panel knows about it.
//
// CreatedAt and LastUsedAt are zero when unknown. LastUsedAt in particular is
// left zero until the key actually authenticates, so "never used" is
// distinguishable from "used at the start of the epoch" — the panel renders the
// two differently.
type Key struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Secret     string    `json:"secret"`
	Realm      string    `json:"realm"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	Usage      Usage     `json:"usage"`
}

// Masked returns the key with its secret replaced by a recognisable stub.
//
// It is a method rather than something List() does automatically because the
// two callers want opposite things: the admin panel must never serve a live
// secret it does not have to, while the request path needs the real value.
// Handing the choice to the caller keeps the safe path the explicit one.
//
// The stub keeps the prefix and the last four characters — enough for the
// operator to tell two keys apart in a list, not enough to use one.
func (k Key) Masked() Key {
	k.Secret = MaskSecret(k.Secret)
	return k
}

// MaskSecret renders one secret as its display form.
//
// A secret too short to keep a tail from is fully hidden: showing four of fewer
// than eight characters would leak half of it.
func MaskSecret(secret string) string {
	s := strings.TrimSpace(secret)
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "****"
	}
	return s[:9] + "…" + s[len(s)-4:]
}

// GenerateSecret mints a fresh client secret.
//
// crypto/rand only. math/rand seeded from the clock would make secrets
// predictable to anyone who knows when the process started, and these are the
// only thing standing between a stranger and the account's upstream quota.
//
// The encoding is URL-safe with no padding so a secret survives being pasted
// into a query string, a shell, or a JSON body untouched.
func GenerateSecret() string {
	buf := make([]byte, secretEntropyBytes)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand.Read on a supported platform cannot fail; if it ever
		// does, the process has no usable entropy and must not invent a key.
		panic(fmt.Sprintf("multikey: entropy source failed: %v", err))
	}
	return SecretPrefix + base64.RawURLEncoding.EncodeToString(buf)
}

// GenerateID mints a short, opaque key id.
//
// Ids are what the usage log records, so they must be stable and unique but
// need no structure — the panel shows the name, and a second key on the same
// realm is legitimate.
func GenerateID() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("multikey: entropy source failed: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// NormalizeRealm lowercases and validates a realm, returning RealmAny for an
// empty string.
func NormalizeRealm(realm string) (string, error) {
	r := strings.ToLower(strings.TrimSpace(realm))
	for _, known := range Realms {
		if r == known {
			return r, nil
		}
	}
	return "", fmt.Errorf("%w: %q", ErrInvalidRealm, realm)
}

// fileContents is the on-disk document. It is a struct rather than a bare slice
// so a future field (a schema version, a tombstone list) can be added without
// making the file's top level change type and breaking every existing install.
type fileContents struct {
	Keys []Key `json:"keys"`
}

// Store is the live credential table.
//
// Every exported method takes the write lock, including the read-only ones:
// Authenticate stamps LastUsedAt, and the alternative — copying the slice under
// a read lock and persisting afterwards — would let two concurrent
// authentications each write their own view of the table and lose the other's
// counters. Contention here is one lock acquisition per request, which is
// nothing beside the upstream call it guards.
type Store struct {
	path string

	mu   sync.RWMutex
	keys []Key
}

// NewStore opens (or creates the in-memory shape of) the store at path.
//
// A missing file is not an error: it means a fresh install, and the caller
// decides whether to seed it. A file that exists but does not parse IS an
// error, and is preserved as `<path>.bad` — the store comes back empty over the
// untouched original rather than over its remains.
func NewStore(path string) (*Store, error) {
	s := &Store{path: path}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Path reports where this store persists, for the panel and for error messages.
func (s *Store) Path() string { return s.path }

// load reads the file into memory. It is called only from NewStore, so it needs
// no lock: nothing else can hold a reference to the store yet.
func (s *Store) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var doc fileContents
	if err := json.Unmarshal(raw, &doc); err != nil {
		// Keep the unreadable file so the operator can recover whatever was
		// typed by hand; the store then behaves as a fresh install, and the
		// first write would otherwise have destroyed the evidence.
		bad := s.path + ".bad"
		if rmErr := os.Remove(bad); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return fmt.Errorf("multikey: unreadable store and stale %s: %w", bad, rmErr)
		}
		if saveErr := os.WriteFile(bad, raw, 0o600); saveErr != nil {
			return fmt.Errorf("multikey: unreadable store, could not preserve: %w", saveErr)
		}
		return fmt.Errorf("multikey: %s is not valid JSON (kept as %s): %w",
			s.path, bad, err)
	}
	s.keys = doc.Keys
	return nil
}

// Exists reports whether the store file is present on disk.
//
// EnsureSeed uses it to tell "fresh install" from "operator deleted every key
// on purpose"; re-seeding the latter would resurrect a credential that was
// deliberately removed.
func (s *Store) Exists() bool {
	_, err := os.Stat(s.path)
	return err == nil
}

// saveLocked writes the table out atomically. The caller holds the lock.
//
// Temp file in the same directory, then rename: rename is atomic within a
// filesystem, so a reader either sees the whole previous file or the whole new
// one. Writing in place would let a crash leave a truncated JSON document, and
// the credential file is the one file in the install that must never be lost.
//
// The mode is 0600 because the file holds plaintext secrets; the panel is not
// the only thing running as this user.
func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(fileContents{Keys: s.keys}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		// The temp file is ours; leaving it behind on a failed rename only
		// litters the accounts directory with a second copy of the secrets.
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// writeLocked persists a mutation, rolling memory back when the write fails.
//
// Without the rollback the store would answer from a table that no longer
// matches disk, and the next restart would silently undo whatever the panel
// just confirmed. A failed write must fail the operation.
func (s *Store) writeLocked(before []Key) error {
	if err := s.saveLocked(); err != nil {
		s.keys = before
		return err
	}
	return nil
}

// clone returns a deep copy of the table.
func (s *Store) clone() []Key {
	out := make([]Key, len(s.keys))
	copy(out, s.keys)
	return out
}

// indexOf returns the position of id, or -1.
func (s *Store) indexOf(id string) int {
	want := strings.TrimSpace(id)
	for i := range s.keys {
		if s.keys[i].ID == want {
			return i
		}
	}
	return -1
}

// List returns every key with its secret intact.
//
// The caller decides whether to mask: the request path needs the real value to
// report what a key configured, while the panel calls Masked() on each entry.
func (s *Store) List() []Key {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clone()
}

// Snapshot is List under a name that reads correctly at an API boundary — the
// admin panel's JSON view. Secrets are left intact; mask them with Masked().
func (s *Store) Snapshot() []Key {
	return s.List()
}

// Get returns one key by id, secret intact.
func (s *Store) Get(id string) (Key, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.indexOf(id)
	if i < 0 {
		return Key{}, false
	}
	return s.keys[i], true
}

// Add mints a new key bound to realm and persists it.
//
// A blank name becomes "未命名" — the same placeholder the Python panel uses.
// A name is display-only, and refusing the add over it would make the panel
// report an error for a request the operator can see is fine.
func (s *Store) Add(name, realm string) (Key, error) {
	normalized, err := NormalizeRealm(realm)
	if err != nil {
		return Key{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "未命名"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	secret := GenerateSecret()
	for _, existing := range s.keys {
		if subtle.ConstantTimeCompare([]byte(existing.Secret), []byte(secret)) == 1 {
			return Key{}, ErrDuplicateSecret
		}
	}

	entry := Key{
		ID:        GenerateID(),
		Name:      name,
		Secret:    secret,
		Realm:     normalized,
		Enabled:   true,
		CreatedAt: time.Now().UTC(),
	}
	before := s.clone()
	s.keys = append(s.keys, entry)
	if err := s.writeLocked(before); err != nil {
		return Key{}, err
	}
	return entry, nil
}

// Remove deletes a key outright.
//
// Unlike the Python reference, which soft-deleted rows to keep the usage log's
// key ids readable, the counters live inside the record here and go with it.
// The id appears in a deleted row only as a name; keeping a secret-less husk
// around forever would grow the credential file without ever letting the
// operator clear a key from the panel.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	i := s.indexOf(id)
	if i < 0 {
		return ErrNoSuchKey
	}
	before := s.clone()
	s.keys = append(s.keys[:i:i], s.keys[i+1:]...)
	return s.writeLocked(before)
}

// SetEnabled flips a key between live and refused.
//
// A disabled key keeps its secret and its counters: the operator is muting a
// client, not revoking it, and re-enabling must not silently reset what it
// spent.
func (s *Store) SetEnabled(id string, on bool) error {
	return s.update(id, func(k *Key) { k.Enabled = on })
}

// SetRealm rebinds a key to another outbound realm.
func (s *Store) SetRealm(id, realm string) error {
	normalized, err := NormalizeRealm(realm)
	if err != nil {
		return err
	}
	return s.update(id, func(k *Key) { k.Realm = normalized })
}

// Rename changes a key's display name.
func (s *Store) Rename(id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "未命名"
	}
	return s.update(id, func(k *Key) { k.Name = name })
}

// update applies mutate to one key and persists the result.
//
// The mutation runs against a copy and is only installed once the write
// succeeds, so a failed save leaves memory exactly as disk has it.
func (s *Store) update(id string, mutate func(*Key)) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	i := s.indexOf(id)
	if i < 0 {
		return ErrNoSuchKey
	}
	before := s.clone()
	entry := s.keys[i]
	mutate(&entry)
	s.keys[i] = entry
	return s.writeLocked(before)
}

// Authenticate resolves a presented secret to the key that owns it.
//
// Returns a pointer to a copy: the caller may keep it for the length of a
// request, and a live pointer into the table would let one request's handler
// scribble over another's view — or over the store itself.
//
// The comparison is constant time. Every stored secret is compared, without
// early exit on the first mismatch, so response timing does not reveal how long
// a shared prefix is; a caller who can time the gateway could otherwise
// reconstruct a secret one character at a time.
//
// Disabled keys never match. LastUsedAt is stamped and persisted on success —
// note that this writes the file on the request path, which is the price of the
// panel's "last used" column being accurate rather than updated in batches.
func (s *Store) Authenticate(secret string) (*Key, bool) {
	supplied := strings.TrimSpace(secret)
	if supplied == "" {
		return nil, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	match := -1
	for i := range s.keys {
		// Compare even for disabled and empty secrets: the loop must do the
		// same work regardless of which row — if any — is going to match.
		equal := subtle.ConstantTimeCompare([]byte(supplied), []byte(s.keys[i].Secret)) == 1
		if equal && s.keys[i].Enabled && match < 0 {
			match = i
		}
	}
	if match < 0 {
		return nil, false
	}

	s.keys[match].LastUsedAt = time.Now().UTC()
	// A stamp that fails to persist is not a reason to reject a valid
	// credential; the request proceeds and the file is correct on the next
	// successful write. load() still sees the truth after a restart.
	_ = s.saveLocked()

	out := s.keys[match]
	return &out, true
}

// RecordUsage adds one request's spend to the key's counters.
//
// The counter belongs to the key, not to the account, because a key is what the
// caller is billed through: two keys sharing an upstream account still have
// distinct bills.
func (s *Store) RecordUsage(id string, prompt, completion int64, credits float64) error {
	if prompt < 0 || completion < 0 {
		// A negative count would silently reduce the total and understate
		// what the caller spent, so it is refused rather than clamped.
		return fmt.Errorf("multikey: negative token count for key %q", id)
	}
	return s.update(id, func(k *Key) {
		k.Usage = k.Usage.add(prompt, completion, credits)
	})
}

// ResetUsage zeroes one key's counters and keeps the key itself.
func (s *Store) ResetUsage(id string) error {
	return s.update(id, func(k *Key) { k.Usage = Usage{} })
}

// EnsureSeed migrates a pre-existing single key into an empty store.
//
// This is the upgrade path from the plaintext `wb2hub-local-key` setup, where
// the gateway had exactly one credential stored beside the settings. That value
// is preserved verbatim as the first key — a migration that re-minted it would
// break every client already configured with it, which is precisely the failure
// the migration exists to prevent.
//
// The legacy key is always enabled and unbound (RealmAny), which reproduces the
// old behaviour: one global key that followed whatever realm the gateway was
// switched to.
//
// Seeding happens only when the store FILE does not exist, not merely when the
// table is empty. An operator who deleted every key meant it; resurrecting one
// on the next start would hand a live credential back to a client they retired.
// A blank legacy key is likewise nothing to migrate, and is a no-op.
func (s *Store) EnsureSeed(legacyKey string) error {
	legacy := strings.TrimSpace(legacyKey)
	if legacy == "" {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.Exists() {
		return nil
	}
	s.keys = []Key{{
		ID:        "default",
		Name:      "default",
		Secret:    legacy,
		Realm:     RealmAny,
		Enabled:   true,
		CreatedAt: time.Now().UTC(),
	}}
	return s.saveLocked()
}
