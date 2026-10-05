package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"wb2hub/internal/growth"
	"wb2hub/internal/pool"
	"wb2hub/internal/trial"
)

// handleGrowthView reports the growth subsystem's state for one account.
//
// Everything here is China-realm only: the international build has no growth
// centre at all, so an intl account gets an explicit "not supported" rather than
// a request that is guaranteed to fail upstream.
func (h *Handler) handleGrowthView(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.requireGrowthAccount(w, r.URL.Query().Get("uid"))
	if !ok {
		return
	}
	if !entry.Account.RealmConfig().HasCheckin {
		writeError(w, http.StatusBadRequest, "not_supported",
			"国际版没有成长中心，该功能仅国内版可用。")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()

	client := h.clientFor(entry)
	gc := growth.New(h.cfg.Upstream)
	out := map[string]any{
		"uid":          entry.Account.UID,
		"night_window": growth.InNightWindow(time.Now()),
	}

	// Each call is independent: a failure in one panel section must not blank
	// the others, so errors are reported per section rather than aborting.
	if tasks, err := gc.Tasks(ctx, client, entry.Account); err == nil {
		rows := make([]map[string]any, 0, len(tasks))
		for _, task := range tasks {
			skip, reason := growth.ShouldSkip(task.Code, time.Now())
			rows = append(rows, map[string]any{
				"code":        task.Code,
				"name":        firstNonEmptyStr(task.Name, growth.TaskName(task.Code)),
				"current":     task.Current,
				"target":      task.Target,
				"reward":      task.Reward,
				"status":      task.Status,
				"done":        task.Done(),
				"jump_url":    task.JumpURL,
				"unforgeable": task.Unforgeable,
				"skip":        skip,
				"skip_reason": reason,
			})
		}
		out["tasks"] = rows
	} else {
		out["tasks_error"] = err.Error()
	}

	if energy, err := gc.Energy(ctx, client, entry.Account); err == nil {
		out["energy"] = energy
	} else {
		out["energy_error"] = err.Error()
	}

	if streak, err := gc.Streak(ctx, client, entry.Account); err == nil {
		out["streak"] = streak
	} else {
		out["streak_error"] = err.Error()
	}

	if cards, err := gc.MakeupCards(ctx, client, entry.Account); err == nil {
		out["makeup_cards"] = cards
	} else {
		out["makeup_cards_error"] = err.Error()
	}

	if travel, err := gc.TravelStatus(ctx, client, entry.Account); err == nil {
		out["travel"] = travel
	} else {
		out["travel_error"] = err.Error()
	}

	writeJSON(w, http.StatusOK, out)
}

// handleGrowthAction runs one growth operation.
//
// The action set is closed on purpose: each entry maps to a specific upstream
// call, so a typo cannot silently become a no-op that looks like success.
func (h *Handler) handleGrowthAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "请用 POST。")
		return
	}

	var body struct {
		UID      string `json:"uid"`
		Code     string `json:"code"`
		Tier     string `json:"tier"`
		Location string `json:"location"`
		Date     string `json:"date"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}

	entry, ok := h.requireGrowthAccount(w, body.UID)
	if !ok {
		return
	}
	if !entry.Account.RealmConfig().HasCheckin {
		writeError(w, http.StatusBadRequest, "not_supported",
			"国际版没有成长中心，该功能仅国内版可用。")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	client := h.clientFor(entry)
	gc := growth.New(h.cfg.Upstream)

	action := strings.TrimPrefix(r.URL.Path, "/admin/growth/")
	var payload any
	var err error

	switch action {
	case "tasks/accept":
		err = gc.AcceptTask(ctx, client, entry.Account, body.Code)

	case "tasks/claim":
		// A desktop-only task cannot be advanced by this gateway at all; the
		// Python reference documents that faking the report event does not move
		// it. Refusing here is more honest than a claim that always 400s.
		if skip, reason := growth.ShouldSkip(body.Code, time.Now()); skip {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": false, "skipped": true, "reason": reason,
			})
			return
		}
		var result growth.RedeemResult
		result, err = gc.ClaimTask(ctx, client, entry.Account, body.Code)
		payload = result

	case "lottery/draw":
		payload, err = gc.LotteryDraw(ctx, client, entry.Account)

	case "redeem":
		payload, err = gc.RedeemTier(ctx, client, entry.Account, atoiSafe(body.Tier))

	case "travel/depart":
		err = gc.TravelDepart(ctx, client, entry.Account, atoiSafe(body.Location))

	case "travel/claim":
		payload, err = gc.TravelClaim(ctx, client, entry.Account)

	case "buddy/first":
		err = gc.BuddyFirst(ctx, client, entry.Account)

	case "buddy/info":
		payload, err = gc.BuddyInfo(ctx, client, entry.Account)

	case "makeup/use":
		payload, err = gc.UseMakeupCard(ctx, client, entry.Account, body.Date)

	default:
		writeError(w, http.StatusNotFound, "unknown_action", "未知操作："+action)
		return
	}

	if err != nil {
		h.logLine("growth %s uid=%s: %v", action, entry.Account.UID, err)
		writeError(w, http.StatusBadGateway, "action_failed", err.Error())
		return
	}
	h.logLine("growth %s uid=%s ok", action, entry.Account.UID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": payload})
}

// handleTrialView reports trial and quota state.
func (h *Handler) handleTrialView(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.requireGrowthAccount(w, r.URL.Query().Get("uid"))
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	tc := trial.New(h.cfg.Upstream)
	resource, err := tc.UserResource(ctx, h.clientFor(entry), entry.Account)
	if err != nil {
		writeError(w, http.StatusBadGateway, "fetch_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"uid":      entry.Account.UID,
		"resource": resource,
	})
}

// handleTrialAction claims trial credit, check-in, compensation or a gift.
func (h *Handler) handleTrialAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "请用 POST。")
		return
	}

	var body struct {
		UID string `json:"uid"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}

	entry, ok := h.requireGrowthAccount(w, body.UID)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	client := h.clientFor(entry)
	tc := trial.New(h.cfg.Upstream)

	action := strings.TrimPrefix(r.URL.Path, "/admin/trial/")
	var result trial.Result
	var err error

	switch action {
	case "claim":
		result, err = tc.Claim(ctx, client, entry.Account)
	case "checkin":
		if !entry.Account.RealmConfig().HasCheckin {
			writeError(w, http.StatusBadRequest, "not_supported",
				"国际版没有每日签到，该功能仅国内版可用。")
			return
		}
		result, err = tc.Checkin(ctx, client, entry.Account)
	case "compensation":
		result, err = tc.ClaimCompensation(ctx, client, entry.Account)
	case "gift":
		result, err = tc.ClaimGift(ctx, client, entry.Account)
	default:
		writeError(w, http.StatusNotFound, "unknown_action", "未知操作："+action)
		return
	}

	if err != nil {
		writeError(w, http.StatusBadGateway, "action_failed", err.Error())
		return
	}
	h.logLine("trial %s uid=%s", action, entry.Account.UID)
	writeJSON(w, http.StatusOK, result)
}

// growthAccount resolves the target account: the named uid, or the first
// enabled one when the caller did not name any.
//
// The two failure modes are reported separately by the caller. A named uid that
// does not exist is a genuine 404; an empty roster is not, because the endpoint
// exists and answering 404 would tell a client the feature is missing.
func (h *Handler) growthAccount(uid string) (pool.EntryView, bool) {
	uid = strings.TrimSpace(uid)
	for _, entry := range h.cfg.Pool.Views() {
		if entry.Account == nil {
			continue
		}
		if uid != "" {
			if entry.Account.UID == uid {
				return entry, true
			}
			continue
		}
		if entry.Account.IsEnabled() {
			return entry, true
		}
	}
	return pool.EntryView{}, false
}

// requireGrowthAccount resolves the account or writes the appropriate response.
//
// It reports whether the caller should continue.
func (h *Handler) requireGrowthAccount(w http.ResponseWriter, uid string) (pool.EntryView, bool) {
	entry, ok := h.growthAccount(uid)
	if ok {
		return entry, true
	}
	if strings.TrimSpace(uid) != "" {
		writeError(w, http.StatusNotFound, "no_such_account", "找不到该账号："+uid)
		return pool.EntryView{}, false
	}
	// No roster yet: the panel should say so rather than show an error, so this
	// is a successful answer that carries an explanation.
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      false,
		"reason":  "no_account",
		"message": "还没有添加账号，先在「添加账号」里导入凭证。",
	})
	return pool.EntryView{}, false
}

// firstNonEmptyStr returns the first non-blank value.
func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// atoiSafe parses a numeric string, yielding 0 for anything unusable.
//
// The panel sends these as strings because they come from text fields, so a
// blank field must not become a parse error the caller has to handle.
func atoiSafe(s string) int {
	n := 0
	neg := false
	for i, r := range strings.TrimSpace(s) {
		if i == 0 && r == '-' {
			neg = true
			continue
		}
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
		if n > 1<<30 {
			return 0
		}
	}
	if neg {
		return -n
	}
	return n
}
