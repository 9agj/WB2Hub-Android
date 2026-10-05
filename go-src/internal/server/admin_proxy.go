package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wb2hub/internal/ipintel"
	"wb2hub/internal/proxy"
)

// handleProxySlots lists the configured slots, or creates a new one.
//
// The bound count is computed here rather than stored, so it can never drift
// from the account files.
func (h *Handler) handleProxySlots(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Slots == nil {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, map[string]any{"slots": []proxy.Slot{}, "enabled": false})
			return
		}
		writeError(w, http.StatusServiceUnavailable, "slots_disabled", "Proxy slots are not configured.")
		return
	}

	switch r.Method {
	case http.MethodGet:
		bound := h.boundCounts()
		slots := h.cfg.Slots.List()
		if slots == nil {
			// Marshal an empty list rather than null so the client can iterate
			// without a nil check.
			slots = []proxy.Slot{}
		}
		for i := range slots {
			slots[i].Bound = bound[slots[i].ID]
		}
		writeJSON(w, http.StatusOK, map[string]any{"slots": slots, "enabled": true})

	case http.MethodPost:
		var body struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		slot, err := h.cfg.Slots.Add(body.Name, body.URL)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_slot", err.Error())
			return
		}
		h.logLine("proxy slot %s created (%s)", slot.ID, slot.URL)
		writeJSON(w, http.StatusOK, slot)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use GET or POST.")
	}
}

// boundCounts tallies enabled accounts per slot id.
func (h *Handler) boundCounts() map[string]int {
	counts := map[string]int{}
	if h.cfg.Pool == nil {
		return counts
	}
	for _, entry := range h.cfg.Pool.List() {
		if entry.Account == nil || !entry.Account.IsEnabled() {
			continue
		}
		if slot := entry.Account.Proxy(); slot != "" {
			counts[slot]++
		}
	}
	return counts
}

// handleProxySlotAction handles create / update / delete / test / bind.
func (h *Handler) handleProxySlotAction(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Slots == nil {
		writeError(w, http.StatusServiceUnavailable, "slots_disabled", "Proxy slots are not configured.")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/admin/proxy/slots/")
	parts := splitPath(rest)
	if len(parts) == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "Missing slot id.")
		return
	}
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch {
	case r.Method == http.MethodDelete || action == "delete":
		if err := h.cfg.Slots.Remove(id); err != nil {
			writeError(w, http.StatusInternalServerError, "remove_failed", err.Error())
			return
		}
		h.logLine("proxy slot %s removed", id)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case action == "test":
		h.testProxySlot(w, r, id)

	case action == "bind":
		h.bindProxySlot(w, r, id)

	case r.Method == http.MethodPut || r.Method == http.MethodPost:
		var body struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		slots := h.cfg.Slots.List()
		found := false
		for i := range slots {
			if slots[i].ID != id {
				continue
			}
			if body.Name != "" {
				slots[i].Name = body.Name
			}
			if body.URL != "" {
				slots[i].URL = body.URL
			}
			found = true
			break
		}
		if !found {
			writeError(w, http.StatusNotFound, "no_such_slot", "No slot with that id.")
			return
		}
		if err := h.cfg.Slots.Save(slots); err != nil {
			writeError(w, http.StatusInternalServerError, "save_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Unsupported method.")
	}
}

// testProxySlot probes one slot and records what it found.
//
// The probe runs with a bounded context so a black-holed proxy cannot hold the
// request open until the client gives up.
func (h *Handler) testProxySlot(w http.ResponseWriter, r *http.Request, id string) {
	slot, ok := h.cfg.Slots.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no_such_slot", "No slot with that id.")
		return
	}
	rawURL, ok := slot.URLFor()
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_url", "This slot has no usable proxy url.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), proxy.ProbeTimeout+5*time.Second)
	defer cancel()

	result := proxy.Probe(ctx, rawURL, h.cfg.Upstream.Direct)
	if err := h.cfg.Slots.UpdateProbe(id, result); err != nil {
		h.logLine("proxy slot %s probe persist failed: %v", id, err)
	}
	h.logLine("proxy slot %s probe: ok=%v exit=%s %s %s",
		id, result.OK, result.ExitIP, result.Country, ipintel.TypeLabel(result.IPType))

	writeJSON(w, http.StatusOK, result)
}

// bindProxySlot attaches an account (or the whole pool) to a slot.
func (h *Handler) bindProxySlot(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		UID string `json:"uid"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if _, ok := h.cfg.Slots.Get(id); !ok {
		writeError(w, http.StatusNotFound, "no_such_slot", "No such slot.")
		return
	}

	applied := 0
	for _, entry := range h.cfg.Pool.List() {
		if entry.Account == nil {
			continue
		}
		if body.UID != "" && entry.Account.UID != body.UID {
			continue
		}
		entry.Account.SetProxy(id)
		if err := entry.Account.Save(); err != nil {
			h.logLine("bind %s -> %s save failed: %v", entry.Account.UID, id, err)
			continue
		}
		applied++
	}
	h.logLine("proxy slot %s bound to %d account(s)", id, applied)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "applied": applied})
}

// handleProxyDiscover sweeps the local proxy client's port range.
//
// Results are advisory: discovery reports what is reachable but does not save
// anything, so an operator can look before adopting a slot.
func (h *Handler) handleProxyDiscover(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Query().Get("host")
	if host == "" {
		host = proxy.DefaultDiscoverHost
	}
	ports := proxy.PortRange(r.URL.Query().Get("ports"))

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	results := proxy.Discover(ctx, host, ports, h.cfg.Upstream.Direct)
	reachable := 0
	for _, res := range results {
		if res.OK {
			reachable++
		}
	}
	h.logLine("proxy discover %s: %d/%d reachable", host, reachable, len(results))

	writeJSON(w, http.StatusOK, map[string]any{
		"host":      host,
		"ports":     ports,
		"reachable": reachable,
		"results":   results,
	})
}

// handleWebTools reports and toggles the gateway-run search/fetch executor.
func (h *Handler) handleWebTools(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled":   h.cfg.LocalWebTools,
			"search":    "web_search",
			"fetch":     "web_fetch",
			"maxRounds": maxRoundsOrZero(h.cfg.LocalWebTools),
		})
	case http.MethodPost:
		var body struct {
			Enabled bool `json:"enabled"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "bad_json", err.Error())
			return
		}
		h.cfg.LocalWebTools = body.Enabled
		h.logLine("local web tools enabled=%v", body.Enabled)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": body.Enabled})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Use GET or POST.")
	}
}

// splitPath drops empty segments so "/a//b/" and "/a/b" behave the same.
func splitPath(p string) []string {
	raw := strings.Split(strings.Trim(p, "/"), "/")
	out := make([]string, 0, len(raw))
	for _, seg := range raw {
		if seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

// queryInt reads an integer query parameter with a default.
func queryInt(r *http.Request, name string, fallback int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}

// marshalPretty is used by the log/diagnostic endpoints that echo JSON blobs.
func marshalPretty(v any) string {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return string(body)
}
