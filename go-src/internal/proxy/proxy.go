// Package proxy manages outbound proxy slots and probes their exits.
//
// A "slot" is one named outbound proxy url. Accounts are bound to slots, which
// is what lets different clients take different routes to the upstream without
// a global switch — the Python original calls this the per-key egress feature.
//
// Ported from the proxy-slot half of wb_proxy.py (probe_proxy_exit,
// probe_proxy_intel, discover_proxy_slots, proxy_slots_view) plus the storage
// helpers in wb_settings.py.
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"wb2hub/internal/ipintel"
)

// exitIPEndpoint returns the caller's public address, fetched through the proxy
// being probed — that is what makes it the exit rather than our own address.
const exitIPEndpoint = "https://api.ipify.org"

const (
	// DefaultDiscoverHost is the local proxy client whose ports are swept by
	// discovery (mihomo / Clash in the Python original).
	DefaultDiscoverHost = "cli-proxy-mihomo"
	// DefaultDiscoverPorts is the port range swept when discovery runs.
	DefaultDiscoverPorts = "17901-17910"
	// ProbeTimeout bounds one exit probe.
	ProbeTimeout = 12 * time.Second
)

// Slot is one stored outbound proxy.
type Slot struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`

	// Exit facts, refreshed by Probe. Every field may be empty, which means
	// "not known" and never "assume something".
	ExitIP      string `json:"exit_ip"`
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
	IPType      string `json:"ip_type"`
	ISP         string `json:"isp"`
	ASN         string `json:"asn"`

	LatencyMS int    `json:"latency_ms"`
	ProbedAt  int64  `json:"probed_at"`
	LastError string `json:"last_error"`

	// Bound is how many enabled accounts currently use this slot. It is derived
	// on read, never stored, so it cannot drift from the account files.
	Bound int `json:"bound"`
}

// Label is what to call a slot: the operator's name, else its exit, else its id.
func (s Slot) Label() string {
	if name := strings.TrimSpace(s.Name); name != "" {
		return name
	}
	if auto := ipintel.SlotName(s.Country, s.IPType); auto != "" {
		return auto
	}
	return s.ID
}

// URLFor returns a usable proxy url and whether the slot is usable at all.
func (s Slot) URLFor() (string, bool) {
	raw := strings.TrimSpace(s.URL)
	if raw == "" {
		return "", false
	}
	if !strings.Contains(raw, "://") {
		// A bare host:port is a plain HTTP proxy.
		raw = "http://" + raw
	}
	if _, err := url.Parse(raw); err != nil {
		return "", false
	}
	return raw, true
}

// ProbeResult is the outcome of probing one proxy url.
type ProbeResult struct {
	OK        bool   `json:"ok"`
	URL       string `json:"url"`
	ExitIP    string `json:"exit_ip"`
	LatencyMS int    `json:"latency_ms"`
	Error     string `json:"error"`
	Reachable bool   `json:"reachable"`

	ipintel.Intel
}

// ExitIP fetches the exit address through the proxy.
//
// A dedicated client is built per probe rather than reusing one: the transport
// carries the proxy configuration, so sharing it would leak one slot's route
// into another's request.
func ExitIP(ctx context.Context, proxyURL string, timeout time.Duration) (string, string) {
	raw, ok := normaliseProxyURL(proxyURL)
	if !ok {
		return "", "empty or invalid proxy url"
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", "invalid proxy url"
	}
	if timeout <= 0 {
		timeout = ProbeTimeout
	}

	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:               http.ProxyURL(parsed),
			TLSHandshakeTimeout: timeout,
			DisableKeepAlives:   true,
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, exitIPEndpoint, nil)
	if err != nil {
		return "", err.Error()
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", truncate(err.Error(), 160)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", truncate(err.Error(), 160)
	}
	return strings.TrimSpace(string(body)), ""
}

// Probe measures one proxy: its exit IP, and where and what that exit is.
//
// Both steps live here so the discovery list, the per-slot test, and the stored
// slot all describe an exit the same way. The latency covers the proxy probe
// only — the geolocation lookup goes out directly and would otherwise inflate
// it.
func Probe(ctx context.Context, proxyURL string, lookupClient *http.Client) ProbeResult {
	started := time.Now()
	exitIP, errText := ExitIP(ctx, proxyURL, ProbeTimeout)
	latency := int(time.Since(started).Milliseconds())

	result := ProbeResult{
		OK:        errText == "",
		URL:       proxyURL,
		ExitIP:    exitIP,
		LatencyMS: latency,
		Error:     errText,
		Reachable: errText == "",
	}
	if exitIP != "" {
		result.Intel = ipintel.Lookup(lookupClient, exitIP, ipintel.LookupTimeout)
	}
	return result
}

// normaliseProxyURL accepts host:port and bare host forms as well as full urls.
func normaliseProxyURL(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h", "socks4":
		return s, true
	default:
		return "", false
	}
}

// PortRange parses "17901-17910" or a single "17901" into a port list.
func PortRange(spec string) []int {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		spec = DefaultDiscoverPorts
	}
	if lo, hi, ok := strings.Cut(spec, "-"); ok {
		start, errA := parsePort(lo)
		end, errB := parsePort(hi)
		if errA == nil && errB == nil && start <= end && end-start < 4096 {
			out := make([]int, 0, end-start+1)
			for p := start; p <= end; p++ {
				out = append(out, p)
			}
			return out
		}
	}
	if p, err := parsePort(spec); err == nil {
		return []int{p}
	}
	return PortRange(DefaultDiscoverPorts)
}

func parsePort(s string) (int, error) {
	var p int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &p); err != nil {
		return 0, err
	}
	if p < 1 || p > 65535 {
		return 0, fmt.Errorf("port out of range: %d", p)
	}
	return p, nil
}

// Discover probes every port in the configured range on the local proxy host
// and reports which ones carry traffic.
//
// Probes run concurrently because a dead port costs a full TCP timeout; done
// serially, sweeping ten ports would take two minutes.
func Discover(ctx context.Context, host string, ports []int, lookupClient *http.Client) []ProbeResult {
	if strings.TrimSpace(host) == "" {
		host = DefaultDiscoverHost
	}
	if len(ports) == 0 {
		ports = PortRange("")
	}

	results := make([]ProbeResult, len(ports))
	var wg sync.WaitGroup
	for i, port := range ports {
		wg.Add(1)
		go func(idx, p int) {
			defer wg.Done()
			raw := fmt.Sprintf("http://%s:%d", host, p)
			results[idx] = Probe(ctx, raw, lookupClient)
		}(i, port)
	}
	wg.Wait()
	return results
}

// Store persists slots next to the accounts, so a slot survives a restart and
// can be bound from an account file.
type Store struct {
	mu   sync.RWMutex
	path string
}

// NewStore opens (or creates) the slot store at path.
func NewStore(path string) (*Store, error) {
	s := &Store{path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return s, nil
}

type storeFile struct {
	Slots []Slot `json:"proxy_slots"`
	Seq   int    `json:"proxy_slot_seq"`
}

func (s *Store) loadLocked() storeFile {
	var data storeFile
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return data
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		// Preserve the unreadable file instead of silently overwriting it; the
		// operator may want to recover hand-edited slots.
		_ = os.Rename(s.path, s.path+".bad")
		return storeFile{}
	}
	return data
}

// List returns every slot, ordered by id.
func (s *Store) List() []Slot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data := s.loadLocked()
	// Never return nil: the panel iterates the list, and JSON null would make
	// every client add a special case.
	out := make([]Slot, 0, len(data.Slots))
	out = append(out, data.Slots...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get returns one slot by id.
func (s *Store) Get(id string) (Slot, bool) {
	for _, slot := range s.List() {
		if slot.ID == id {
			return slot, true
		}
	}
	return Slot{}, false
}

// Save replaces the whole slot list.
func (s *Store) Save(slots []Slot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data := s.loadLocked()

	seen := map[string]bool{}
	cleaned := make([]Slot, 0, len(slots))
	for _, slot := range slots {
		slot.ID = strings.TrimSpace(slot.ID)
		slot.URL = strings.TrimSpace(slot.URL)
		slot.Name = strings.TrimSpace(slot.Name)
		if slot.ID == "" || seen[slot.ID] {
			continue
		}
		seen[slot.ID] = true
		cleaned = append(cleaned, slot)
	}
	data.Slots = cleaned
	return s.flushLocked(data)
}

// Add appends a slot with a freshly allocated id.
func (s *Store) Add(name, rawURL string) (Slot, error) {
	if _, ok := normaliseProxyURL(rawURL); !ok {
		return Slot{}, fmt.Errorf("invalid proxy url: %q", rawURL)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	data := s.loadLocked()

	data.Seq++
	slot := Slot{
		ID:   fmt.Sprintf("slot-%d", data.Seq),
		Name: strings.TrimSpace(name),
		URL:  strings.TrimSpace(rawURL),
	}
	data.Slots = append(data.Slots, slot)
	if err := s.flushLocked(data); err != nil {
		return Slot{}, err
	}
	return slot, nil
}

// Remove deletes a slot.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data := s.loadLocked()

	out := data.Slots[:0]
	for _, slot := range data.Slots {
		if slot.ID != id {
			out = append(out, slot)
		}
	}
	data.Slots = out
	return s.flushLocked(data)
}

// UpdateProbe records the latest probe outcome on a slot, creating it if the
// probe came from discovery and the operator has not saved it yet.
func (s *Store) UpdateProbe(id string, result ProbeResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data := s.loadLocked()

	found := false
	for i := range data.Slots {
		if data.Slots[i].ID != id {
			continue
		}
		found = true
		apply := &data.Slots[i]
		apply.ExitIP = result.ExitIP
		apply.Country = result.Country
		apply.CountryCode = result.CountryCode
		apply.IPType = result.IPType
		apply.ISP = result.ISP
		apply.ASN = result.ASN
		apply.LatencyMS = result.LatencyMS
		apply.ProbedAt = time.Now().Unix()
		apply.LastError = result.Error
		if apply.URL == "" {
			apply.URL = result.URL
		}
		break
	}
	if !found {
		return fmt.Errorf("no such slot: %s", id)
	}
	return s.flushLocked(data)
}

func (s *Store) flushLocked(data storeFile) error {
	body, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// truncate caps an error string so a pathological upstream cannot bloat the
// panel or the log.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
