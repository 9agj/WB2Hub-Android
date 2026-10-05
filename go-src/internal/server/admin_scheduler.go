package server

import (
	"context"
	"net/http"
	"strings"
	"time"

	"wb2hub/internal/scheduler"
)

// handleSchedulerStatus reports the schedule and the last batch's outcome.
func (h *Handler) handleSchedulerStatus(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Scheduler == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false,
			"note":    "定时巡检未启用。",
		})
		return
	}
	writeJSON(w, http.StatusOK, h.cfg.Scheduler.Status())
}

// handleSchedulerRun runs one job immediately.
//
// The manual path shares the batch lock with the scheduled one, so pressing the
// button while a batch is in flight is refused with a reason rather than queued
// behind it. The job is named explicitly because "run everything" would make the
// lock contention ambiguous.
func (h *Handler) handleSchedulerRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "请用 POST。")
		return
	}
	if h.cfg.Scheduler == nil {
		writeError(w, http.StatusServiceUnavailable, "disabled", "定时巡检未启用。")
		return
	}

	job := strings.TrimSpace(r.URL.Query().Get("job"))
	if job == "" {
		job = string(scheduler.JobCheckin)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var result scheduler.Result
	switch scheduler.Job(job) {
	case scheduler.JobCheckin:
		result = h.cfg.Scheduler.RunCheckinNow(ctx)
	case scheduler.JobTravel:
		result = h.cfg.Scheduler.RunTravelNow(ctx)
	case scheduler.JobKeepalive:
		result = h.cfg.Scheduler.RunKeepaliveNow(ctx)
	case scheduler.JobCat:
		result = h.cfg.Scheduler.RunCatNow(ctx)
	case scheduler.JobGrowth:
		result = h.cfg.Scheduler.RunGrowthNow(ctx)
	default:
		writeError(w, http.StatusBadRequest, "unknown_job",
			"未知任务："+job+"。可用：checkin / travel / keepalive / cat / growth。")
		return
	}

	h.logLine("手动触发 %s: 成功 %d · 跳过 %d · 失败 %d",
		job, result.Success, result.Skip, result.Failure)
	writeJSON(w, http.StatusOK, result)
}

// handleSchedulerConfig replaces the configured hours.
//
// Hours are validated here rather than passed through: an out-of-range hour
// would never match a wall clock, so accepting it would silently disable a job
// while the panel still showed it as configured.
func (h *Handler) handleSchedulerConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "请用 POST。")
		return
	}
	if h.cfg.Scheduler == nil {
		writeError(w, http.StatusServiceUnavailable, "disabled", "定时巡检未启用。")
		return
	}

	var body struct {
		Checkin   []int `json:"checkin_hours"`
		Travel    []int `json:"travel_hours"`
		Keepalive []int `json:"keepalive_hours"`
		Cat       []int `json:"cat_hours"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}

	for name, hours := range map[string][]int{
		"checkin_hours":   body.Checkin,
		"travel_hours":    body.Travel,
		"keepalive_hours": body.Keepalive,
		"cat_hours":       body.Cat,
	} {
		for _, hour := range hours {
			if hour < 0 || hour > 23 {
				writeError(w, http.StatusBadRequest, "bad_hour",
					name+" 中的 "+itoa(hour)+" 不是合法小时（0-23）。")
				return
			}
		}
	}

	h.cfg.Scheduler.SetHours(scheduler.Hours{
		Checkin:   body.Checkin,
		Travel:    body.Travel,
		Keepalive: body.Keepalive,
		Cat:       body.Cat,
	})
	h.logLine("定时巡检时间已更新")
	writeJSON(w, http.StatusOK, h.cfg.Scheduler.Status())
}
