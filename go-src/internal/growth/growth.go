// Package growth is the China-realm growth/activity subsystem: the daily task
// centre, the streak/heatmap/energy counters, the lottery, and the buddy (cat)
// travel loop.
//
// It exists because the growth endpoints are not a clean API. Every request that
// matters is a *state machine held upstream* that the client has to advance in
// the right order — accept, then report an event, then wait for the progress to
// land, then claim. Driving that from a scheduler is the entire point of the
// subsystem, and the two things that make it non-trivial are both hard
// constraints discovered by measurement, not by reading a spec:
//
//  1. Four tasks (see DesktopOnlyTasks) only recognise a real desktop client.
//     A forged /v2/report for them is accepted by the transport and ignored by
//     the task engine, so the progress stays 0/1 and the claim that follows
//     always fails with "task not completed". Faking them wastes a request and
//     pollutes the log with a failure that is not a failure. ShouldSkip returns
//     a reason for these instead, and the caller tells the operator what real
//     action is needed.
//  2. black_cat (the night-owl task) only counts an event reported inside
//     23:00–08:00. Reporting it at any other hour is silently dropped.
//
// Everything here is gated on the realm: the international build has no growth
// centre at all, so a call there is refused locally with ErrNotSupported rather
// than fired off to earn a guaranteed 400.
//
// Ported from wb_tasks.py. Standard library only.
package growth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"wb2hub/internal/auth"
	"wb2hub/internal/upstream"
)

// ErrNotSupported reports that the account's realm has no growth subsystem.
//
// It is a sentinel rather than a formatted error so the caller can answer the
// panel with "国际版无此功能" instead of forwarding an upstream complaint that
// describes a request the gateway should never have sent.
var ErrNotSupported = errors.New("growth: not available on this realm")

// Request paths. The mix of /v2 and /v1 prefixes is upstream's, not a typo:
// the list and accept endpoints live under /v2/activity and the rest under
// /activity.
const (
	pathTasks       = "/v2/activity/growth/tasks"
	pathAccept      = "/v2/activity/growth/tasks/accept"
	pathClaim       = "/activity/growth/tasks/%s/claim"
	pathEnergy      = "/v2/activity/growth/energy"
	pathStreak      = "/activity/growth/streak"
	pathHeatmap     = "/activity/growth/heatmap"
	pathLotteryCh   = "/activity/growth/lottery/chances"
	pathLotteryDraw = "/activity/growth/lottery/draw"
	pathRedeem      = "/activity/growth/redeem"
	pathMakeupCards = "/activity/growth/makeup-cards"
	pathMakeupUse   = "/activity/growth/makeup-cards/use"
	pathBuddyInfo   = "/activity/growth/buddy/info"
	pathBuddyFirst  = "/activity/growth/buddy/first"
	pathBuddyAgree  = "/activity/growth/buddy/agreement"
	pathTravelState = "/activity/growth/buddy/travel/status"
	pathTravelGo    = "/activity/growth/buddy/travel/depart"
	pathTravelClaim = "/activity/growth/buddy/travel/claim"
)

// NightWindowStart and NightWindowEnd bound the window in which the night-owl
// task counts. The window wraps midnight, so it is two comparisons rather than
// a range.
const (
	NightWindowStart = 23
	NightWindowEnd   = 8
)

// DesktopOnlyTasks maps a task code to the real desktop action that completes
// it.
//
// These four are not "hard tasks" — they are tasks the upstream completes only
// from a signal a genuine desktop client emits (a workbuddy:// deep link being
// opened). /v2/report for them is accepted with code=0 and has no effect, so
// the honest behaviour is to skip and hand the operator the action.
var DesktopOnlyTasks = map[string]string{
	"RichMeow_Chat": "在桌面端发起 1 次对话",
	"Library_read":  "在桌面端打开「资料库」并读完介绍文档",
	"Buddy_App":     "在桌面端左上角「发现应用」进入任意一个 Buddy 应用",
	"Buddy_App_QQ":  "在桌面端「发现应用」进入「企鹅教师助手」",
}

// NightTaskCodes are the task codes that only count inside the night window.
//
// It is a map rather than a slice so membership is a lookup; the slice form of
// this constant was the shape that made the Python reference miss the check in
// one of its two call sites.
var NightTaskCodes = map[string]struct{}{
	"black_cat": {},
}

// taskSpec is the locally held half of a task definition: what the upstream
// sometimes omits and what the reporting code needs to know anyway.
type taskSpec struct {
	kind   string
	target int
	reward int
	name   string
	// unforgeable marks a task no amount of event reporting can complete
	// (a real donation, for instance). It is skipped outright rather than
	// reported at.
	unforgeable bool
	reason      string
}

// taskTable is the task table from the Python reference, copied faithfully.
// It is the fallback when the upstream's own list omits a target, a reward or
// a title — which it does for several tasks.
var taskTable = map[string]taskSpec{
	"create_canvas":       {kind: "canvas", target: 1, reward: 300, name: "创建设计任务"},
	"template_5":          {kind: "template", target: 5, reward: 200, name: "模板创建任务"},
	"expert_5":            {kind: "expert", target: 5, reward: 200, name: "使用专家助手"},
	"Expert_team_use_3":   {kind: "team", target: 3, reward: 150, name: "使用专家团队"},
	"skill_1":             {kind: "skill", target: 1, reward: 100, name: "体验技能"},
	"automation_1":        {kind: "automation", target: 1, reward: 100, name: "创建自动化任务"},
	"playbook_prompt":     {kind: "playbook", target: 1, reward: 100, name: "灵感案例使用"},
	"Expert_lighthouse":   {kind: "lighthouse", target: 1, reward: 100, name: "轻量云专家使用"},
	"Buddy_App":           {kind: "buddy5", target: 1, reward: 100, name: "进入 Buddy 应用"},
	"Buddy_App_QQ":        {kind: "buddy5", target: 1, reward: 100, name: "企鹅教师助手"},
	"Hp_Appearance":       {kind: "skin", target: 1, reward: 100, name: "应用主题外观"},
	"chat_5":              {kind: "chat", target: 5, reward: 100, name: "发起 5 次对话"},
	"Model_chat_GLM5.2":   {kind: "glmchat", target: 1, reward: 100, name: "体验 GLM-5.2"},
	"black_cat":           {kind: "cat", target: 3, reward: 100, name: "夜猫子任务 (23:00-08:00)"},
	"RichMeow_Chat":       {kind: "richmeow", target: 1, reward: 100, name: "桌面对话事件链"},
	"Library_read":        {kind: "library", target: 1, reward: 100, name: "浏览资料库"},
	"first_buddy":         {kind: "buddy_first", target: 1, reward: 0, name: "领养首只猫猫"},
	"Expert_Philanthropy": {target: 1, reward: 0, name: "公益爱心捐赠", unforgeable: true, reason: "真实捐款动作"},
}

// idEntry is one entry of the expert / team id pool.
//
// The ids are the substance of the entry: the upstream deduplicates growth
// events by (eventCode, id), so reporting the *same* expert twice advances the
// task by one, not two. Walking the pool is what makes expert_5 and
// Expert_team_use_3 reachable at all.
type idEntry struct {
	ID   string
	Name string
}

// ExpertIDPool is the set of expert ids verified in 2026-09 to advance the
// expert tasks.
var ExpertIDPool = []idEntry{
	{"ex_PZw8Gu81HfN4", "运维工程师"},
	{"ex_ROsDtJbzADFV", "产品经理"},
	{"ex_SMUnl0nJbPix", "UI设计师"},
	{"ex_ZTR062oVBOCW", "数据分析师"},
	{"ex_a3sSSFBy8qaC", "后端架构师"},
	{"ex_aG1kvKbq8lPx", "文案策划"},
	{"ex_al1vxtUOYQ10", "测试专家"},
	{"ex_cZfiyuET9UQP", "安全顾问"},
	{"ex_eggOvQuVP0hq", "算法工程师"},
	{"ex_hSwsQjkSKnkX", "前端工程师"},
	{"ex_mMbwwmFA9n9P", "项目管理专家"},
	{"ex_uAQE5POfk7Zh", "增长运营专家"},
	{"ex_uZzSAScSy7FZ", "行业研究员"},
	{"ex_LHywGrZOtG7G", "数据分析师"},
	{"ex_NX5C8GBciVed", "测试架构师"},
	{"ex_DdCsaoq4AtcO", "云端运维专家"},
	{"ex_KzqKQguubrNQ", "内容创作专家"},
	{"ex_2cvvUZQhDyeJ", "腾讯轻量云专家"},
}

// TeamIDPool is the first entries of the official expert_center.json team list.
// The Python reference keeps eight; the first three were measured to advance
// Expert_team_use_3.
var TeamIDPool = []idEntry{
	{"CloudOpsTeam", "运维专家团队"},
	{"CloudContentTeam", "内容专家团队"},
	{"CloudDevTeam", "研发专家团队"},
	{"ProductStrategyTeam", "产品战略团队"},
	{"MarketingCampaignTeam", "营销活动团队"},
	{"SalesBattleTeam", "销售作战团队"},
	{"DesignEngineTeam", "设计引擎团队"},
	{"HrOperationsTeam", "人力运营团队"},
}

// Task is one growth task as the panel shows it.
//
// Current/Target and Status are separate fields rather than a derived
// "complete" bool because the upstream reports both a progress pair and an
// accept/claim status, and they disagree during the window between an event
// landing and the task being marked completed.
type Task struct {
	Code        string `json:"task_code"`
	Name        string `json:"name"`
	Description string `json:"description"`
	JumpURL     string `json:"jump_url"`
	Status      string `json:"status"`
	Current     int    `json:"current"`
	Target      int    `json:"target"`
	Reward      int    `json:"reward_credit"`
	Energy      int    `json:"reward_energy"`
	// Unforgeable marks a task only a real-world action completes; Reason is
	// the human sentence explaining it.
	Unforgeable bool   `json:"unforgeable"`
	Reason      string `json:"reason"`
}

// Done reports whether the task's progress has reached its target.
func (t Task) Done() bool {
	if t.Target <= 0 {
		return false
	}
	return t.Current >= t.Target
}

// StreakInfo is the consecutive check-in counter and the makeup cards attached
// to it.
//
// The card fields are pointers-free zero values: upstream omits them when the
// account has none, and "no card" and "a card worth zero" are the same thing
// to every caller here.
type StreakInfo struct {
	Days         int `json:"days"`
	MaxDays      int `json:"max_days"`
	Cards        int `json:"cards"`
	CardsUsed    int `json:"cards_used"`
	Compensation int `json:"compensation"`
}

// HeatmapCell is one day of the activity heatmap.
type HeatmapCell struct {
	Date   string `json:"date"`
	Count  int    `json:"count"`
	Level  int    `json:"level"`
	Streak int    `json:"streak"`
}

// LotteryChances is the lottery state: how many draws are banked and what is on
// offer.
//
// Chances is the spendable count; Total is what upstream reports as the day's
// allowance, which can exceed Chances when draws have already been made.
type LotteryChances struct {
	Chances int `json:"chances"`
	Total   int `json:"total"`
	Used    int `json:"used"`
	Drawn   int `json:"drawn"`
	// Prizes is the prize table, kept as a list of specs rather than a map
	// because the order is the wheel order.
	Prizes []LotteryPrize `json:"prizes"`
}

// LotteryPrize is one slice of the lottery wheel.
type LotteryPrize struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Amount int    `json:"amount"`
}

// RedeemResult is what a tier redemption paid out.
type RedeemResult struct {
	OK     bool `json:"ok"`
	Tier   int  `json:"tier"`
	Credit int  `json:"credit"`
	Energy int  `json:"energy"`
	Cost   int  `json:"cost"`
}

// TravelState is the buddy's current trip.
//
// State is the upstream's own word ("idle" / "traveling" / "arrived"); it is
// kept as a string rather than an enum because a new state added upstream must
// not make the gateway report an unknown account as idle.
type TravelState struct {
	State             string `json:"state"`
	LocationID        int    `json:"location_id"`
	Location          string `json:"location"`
	DepartAt          int64  `json:"depart_at"`
	ArriveAt          int64  `json:"arrive_at"`
	DailyLimitReached bool   `json:"daily_limit_reached"`
	RewardCredit      int    `json:"reward_credit"`
}

// Buddy is the adopted cat.
type Buddy struct {
	Adopted  bool   `json:"adopted"`
	Name     string `json:"name"`
	Level    int    `json:"level"`
	Energy   int    `json:"energy"`
	Nickname string `json:"nickname"`
	Avatar   string `json:"avatar"`
}

// MakeupCards is the makeup-checkin inventory.
type MakeupCards struct {
	Available int      `json:"available"`
	Used      int      `json:"used"`
	Total     int      `json:"total"`
	Items     []string `json:"items"`
}

// Client talks to the growth endpoints for one gateway.
//
// The http.Client is passed per call rather than held here because the egress
// route belongs to the account (its proxy slot), and a client cached on this
// struct would pin whichever account asked first.
type Client struct {
	Up *upstream.Client
}

// New builds a growth client over an existing upstream transport.
func New(up *upstream.Client) *Client {
	if up == nil {
		up = upstream.New()
	}
	return &Client{Up: up}
}

// InNightWindow reports whether now falls inside the 23:00–08:00 window.
//
// The window is half-open at the end: 23:00 and 07:59 count, 08:00 does not.
// The upstream's own scheduler fires at 01:00, well inside it.
func InNightWindow(now time.Time) bool {
	h := now.Hour()
	return h >= NightWindowStart || h < NightWindowEnd
}

// TaskReward returns the credit reward for a known task code, or 0.
func TaskReward(code string) int { return taskTable[strings.TrimSpace(code)].reward }

// TaskName returns the local display name for a known task code, or "".
func TaskName(code string) string { return taskTable[strings.TrimSpace(code)].name }

// TaskKind returns the reporting event kind for a task code, or "".
//
// Exported because the scheduler that builds /v2/report events needs the same
// table the panel renders from, and two copies would drift.
func TaskKind(code string) string { return taskTable[strings.TrimSpace(code)].kind }

// TaskTarget returns the progress target for a known task code, or 0.
func TaskTarget(code string) int { return taskTable[strings.TrimSpace(code)].target }

// IsUnforgeable reports whether a task can only be completed by a real action,
// and why.
func IsUnforgeable(code string) (bool, string) {
	spec := taskTable[strings.TrimSpace(code)]
	return spec.unforgeable, spec.reason
}

// ShouldSkip decides whether reporting an event for code would be wasted work
// right now, and says why in one sentence for the operator's log.
//
// The two cases are the hard constraints at the top of this package: a task
// that needs a desktop client, and the night-owl task outside its window. Both
// would otherwise "succeed" (HTTP 200, code 0) while changing nothing, which is
// the worst possible failure mode — it looks like progress.
func ShouldSkip(code string, now time.Time) (bool, string) {
	trimmed := strings.TrimSpace(code)
	if reason, ok := DesktopOnlyTasks[trimmed]; ok {
		return true, fmt.Sprintf("任务 [%s] 需桌面端真实操作: %s, 跳过事件伪造",
			taskDisplayName(trimmed), reason)
	}
	if _, ok := NightTaskCodes[trimmed]; ok && !InNightWindow(now) {
		return true, fmt.Sprintf("任务 [%s] 仅 23:00-08:00 上报计数, 当前不在窗口, 跳过",
			taskDisplayName(trimmed))
	}
	if spec := taskTable[trimmed]; spec.unforgeable {
		return true, fmt.Sprintf("任务 [%s] 需真实动作 (%s), 跳过", taskDisplayName(trimmed), spec.reason)
	}
	return false, ""
}

// taskDisplayName is the task's local name, falling back to the code so a
// message about an unknown task still identifies it.
func taskDisplayName(code string) string {
	if name := taskTable[code].name; name != "" {
		return name
	}
	return code
}

// Tasks lists the task centre's tasks.
//
// The upstream reports a task's own title, target and reward, but omits them on
// some rows; the local table fills the gaps so the panel never renders a task
// with no name or a 0/0 progress bar.
func (c *Client) Tasks(ctx context.Context, client *http.Client, acct *auth.Account) ([]Task, error) {
	body, err := c.get(ctx, client, acct, pathTasks)
	if err != nil {
		return nil, err
	}
	rows := listFrom(body, "tasks")
	out := make([]Task, 0, len(rows))
	for _, row := range rows {
		code := stringField(row, "task_code")
		if code == "" {
			// A row with no code cannot be accepted, reported against or
			// claimed. Dropping it keeps every downstream consume of a Task
			// from having to guard against it.
			continue
		}
		spec := taskTable[code]
		progress, _ := row["progress"].(map[string]any)

		task := Task{
			Code:        code,
			Name:        firstNonEmpty(stringField(row, "title"), spec.name, code),
			Description: firstNonEmpty(stringField(row, "description"), stringField(row, "task_desc")),
			JumpURL:     stringField(row, "jump_url"),
			Status:      firstNonEmpty(stringField(row, "accept_status"), "not_accepted"),
			Current:     intField(progress, "current"),
			Target:      intField(progress, "target"),
			Reward:      intField(row, "reward_credit"),
			Energy:      intField(row, "reward_energy"),
			Unforgeable: spec.unforgeable,
			Reason:      spec.reason,
		}
		if task.Target <= 0 {
			task.Target = spec.target
		}
		if task.Target <= 0 {
			// Nothing in either source: the task is at least a one-shot.
			task.Target = 1
		}
		if task.Reward == 0 {
			task.Reward = spec.reward
		}
		out = append(out, task)
	}
	return out, nil
}

// AcceptTask accepts one task code.
//
// Accepting is not optional: upstream only accumulates progress for accepted
// tasks, so reporting an event first and accepting later is what produces the
// "progress stays 0" symptom this package exists to avoid.
func (c *Client) AcceptTask(ctx context.Context, client *http.Client, acct *auth.Account, code string) error {
	return c.accept(ctx, client, acct, []string{code})
}

// AcceptTasks accepts several task codes in one request.
//
// Fewer round trips is the whole reason: the upstream rate-limits and the
// caller runs on a phone, but a batch is also atomic on the upstream's side —
// either the whole batch is processed or the reply's per-task statuses say
// which entries failed.
func (c *Client) AcceptTasks(ctx context.Context, client *http.Client, acct *auth.Account, codes []string) error {
	// Trim before deciding, not after. A list of blanks is an empty batch just
	// as a nil list is, so both must be the same no-op; deciding on the raw
	// length sent one of them to the network path and errored there. The
	// single-code path keeps its own guard, where a blank code really is a bug.
	codes = trimCodes(codes)
	if len(codes) == 0 {
		return nil
	}
	return c.accept(ctx, client, acct, codes)
}

// accept posts one accept batch and turns a non-zero envelope code into an
// error carrying the upstream's own wording.
func (c *Client) accept(ctx context.Context, client *http.Client, acct *auth.Account,
	codes []string) error {

	if err := c.gate(acct); err != nil {
		return err
	}
	accountPatch := trimCodes(codes)
	if len(accountPatch) == 0 {
		return fmt.Errorf("growth: no task codes to accept")
	}
	payload := map[string]any{"task_codes": accountPatch}

	decoded, _, status, err := c.Up.DoJSON(ctx, client, http.MethodPost,
		c.chatURL(acct, pathAccept), upstream.Headers(acct, "chat"), payload)
	if err != nil {
		return fmt.Errorf("growth: accept: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("growth: accept: HTTP %d", status)
	}
	if err := envelopeError(decoded); err != nil {
		return fmt.Errorf("growth: accept: %w", err)
	}
	return nil
}

// ClaimTask claims one task's reward.
//
// A claim can legitimately fail with a business error — "task not completed"
// when an event has been reported but the progress has not landed yet. That is
// reported as an error rather than swallowed, because the caller's back-off
// decision depends on telling it apart from a transport failure.
func (c *Client) ClaimTask(ctx context.Context, client *http.Client, acct *auth.Account, code string) (RedeemResult, error) {
	if err := c.gate(acct); err != nil {
		return RedeemResult{}, err
	}
	trimmed := strings.TrimSpace(code)
	if trimmed == "" {
		return RedeemResult{}, fmt.Errorf("growth: claim: empty task code")
	}

	decoded, _, status, err := c.Up.DoJSON(ctx, client, http.MethodPost,
		c.chatURL(acct, fmt.Sprintf(pathClaim, escapePath(trimmed))),
		upstream.Headers(acct, "chat"), map[string]any{})
	if err != nil {
		return RedeemResult{}, fmt.Errorf("growth: claim %s: %w", trimmed, err)
	}
	if status != http.StatusOK {
		return RedeemResult{}, fmt.Errorf("growth: claim %s: HTTP %d", trimmed, status)
	}
	if err := envelopeError(decoded); err != nil {
		return RedeemResult{}, fmt.Errorf("growth: claim %s: %w", trimmed, err)
	}

	data := dataObject(decoded)
	return RedeemResult{
		OK:     true,
		Credit: intField(data, "credit"),
		Energy: intField(data, "energy"),
	}, nil
}

// Energy returns the account's energy balance.
func (c *Client) Energy(ctx context.Context, client *http.Client, acct *auth.Account) (int, error) {
	body, err := c.get(ctx, client, acct, pathEnergy)
	if err != nil {
		return 0, err
	}
	return intField(dataObject(body), "balance"), nil
}

// Streak returns the check-in streak.
func (c *Client) Streak(ctx context.Context, client *http.Client, acct *auth.Account) (StreakInfo, error) {
	body, err := c.get(ctx, client, acct, pathStreak)
	if err != nil {
		return StreakInfo{}, err
	}
	// The upstream nests the counters differently across builds: sometimes
	// under "streak", sometimes at the top of "data". Reading both costs one
	// lookup and removes a class of "0 days" reports.
	node := dataObject(body)
	if nested, ok := node["streak"].(map[string]any); ok {
		node = nested
	}
	return StreakInfo{
		Days:         intField(node, "days"),
		MaxDays:      intField(node, "max_days"),
		Cards:        intField(node, "cards"),
		CardsUsed:    intField(node, "cards_used"),
		Compensation: intField(node, "compensation"),
	}, nil
}

// Heatmap returns the activity heatmap, oldest cell first.
func (c *Client) Heatmap(ctx context.Context, client *http.Client, acct *auth.Account) ([]HeatmapCell, error) {
	body, err := c.get(ctx, client, acct, pathHeatmap)
	if err != nil {
		return nil, err
	}
	rows := listFrom(body, "cells")
	if len(rows) == 0 {
		rows = listFrom(body, "heatmap")
	}
	out := make([]HeatmapCell, 0, len(rows))
	for _, row := range rows {
		out = append(out, HeatmapCell{
			Date:   stringField(row, "date"),
			Count:  intField(row, "count"),
			Level:  intField(row, "level"),
			Streak: intField(row, "streak"),
		})
	}
	return out, nil
}

// LotteryChances returns the banked draws and the prize table.
func (c *Client) LotteryChances(ctx context.Context, client *http.Client, acct *auth.Account) (LotteryChances, error) {
	body, err := c.get(ctx, client, acct, pathLotteryCh)
	if err != nil {
		return LotteryChances{}, err
	}
	node := dataObject(body)
	out := LotteryChances{
		Chances: intField(node, "chances"),
		Total:   intField(node, "total"),
		Used:    intField(node, "used"),
		Drawn:   intField(node, "drawn"),
	}
	for _, prize := range listFrom(body, "prizes") {
		out.Prizes = append(out.Prizes, LotteryPrize{
			ID:     stringField(prize, "id"),
			Name:   stringField(prize, "name"),
			Type:   stringField(prize, "type"),
			Amount: intField(prize, "amount"),
		})
	}
	return out, nil
}

// LotteryDraw spends one draw.
func (c *Client) LotteryDraw(ctx context.Context, client *http.Client, acct *auth.Account) (RedeemResult, error) {
	body, err := c.post(ctx, client, acct, pathLotteryDraw)
	if err != nil {
		return RedeemResult{}, err
	}
	data := dataObject(body)
	return RedeemResult{
		OK:     true,
		Credit: intField(data, "credit"),
		Energy: intField(data, "energy"),
	}, nil
}

// RedeemTier redeems one tier of the ladder.
//
// The tier is validated locally because the upstream answers an out-of-range
// tier with a 400 that reads like a server fault; the caller's tier comes from
// a panel control and a mistyped one should be caught before it is sent.
func (c *Client) RedeemTier(ctx context.Context, client *http.Client, acct *auth.Account, tier int) (RedeemResult, error) {
	if err := c.gate(acct); err != nil {
		return RedeemResult{}, err
	}
	if tier <= 0 {
		return RedeemResult{}, fmt.Errorf("growth: redeem: tier must be positive, got %d", tier)
	}
	body, err := c.post(ctx, client, acct, pathRedeem)
	if err != nil {
		return RedeemResult{}, err
	}
	data := dataObject(body)
	return RedeemResult{
		OK:     true,
		Tier:   tier,
		Credit: intField(data, "credit"),
		Energy: intField(data, "energy"),
		Cost:   intField(data, "cost"),
	}, nil
}

// MakeupCards returns the makeup-checkin inventory.
func (c *Client) MakeupCards(ctx context.Context, client *http.Client, acct *auth.Account) (MakeupCards, error) {
	body, err := c.get(ctx, client, acct, pathMakeupCards)
	if err != nil {
		return MakeupCards{}, err
	}
	node := dataObject(body)
	out := MakeupCards{
		Available: intField(node, "available"),
		Used:      intField(node, "used"),
		Total:     intField(node, "total"),
	}
	for _, item := range listFrom(body, "items") {
		if id := stringField(item, "id"); id != "" {
			out.Items = append(out.Items, id)
		}
	}
	return out, nil
}

// UseMakeupCard spends a makeup card on one date.
//
// date is the missed local date, YYYY-MM-DD. An empty date asks the upstream to
// pick the most recent miss, which is what the panel's one-click button wants.
func (c *Client) UseMakeupCard(ctx context.Context, client *http.Client, acct *auth.Account, date string) (RedeemResult, error) {
	if err := c.gate(acct); err != nil {
		return RedeemResult{}, err
	}
	payload := map[string]any{}
	if trimmed := strings.TrimSpace(date); trimmed != "" {
		payload["date"] = trimmed
	}
	decoded, _, status, err := c.Up.DoJSON(ctx, client, http.MethodPost,
		c.chatURL(acct, pathMakeupUse), upstream.Headers(acct, "chat"), payload)
	if err != nil {
		return RedeemResult{}, fmt.Errorf("growth: use makeup card: %w", err)
	}
	if status != http.StatusOK {
		return RedeemResult{}, fmt.Errorf("growth: use makeup card: HTTP %d", status)
	}
	if err := envelopeError(decoded); err != nil {
		return RedeemResult{}, fmt.Errorf("growth: use makeup card: %w", err)
	}
	data := dataObject(decoded)
	return RedeemResult{
		OK:     true,
		Credit: intField(data, "credit"),
		Energy: intField(data, "energy"),
	}, nil
}

// BuddyInfo returns the adopted cat's state.
func (c *Client) BuddyInfo(ctx context.Context, client *http.Client, acct *auth.Account) (Buddy, error) {
	body, err := c.get(ctx, client, acct, pathBuddyInfo)
	if err != nil {
		return Buddy{}, err
	}
	node := dataObject(body)
	if nested, ok := node["buddy"].(map[string]any); ok {
		node = nested
	}
	return Buddy{
		Adopted:  boolField(node, "adopted") || boolField(node, "has_buddy"),
		Name:     stringField(node, "name"),
		Level:    intField(node, "level"),
		Energy:   intField(node, "energy"),
		Nickname: stringField(node, "nickname"),
		Avatar:   stringField(node, "avatar"),
	}, nil
}

// BuddyFirst adopts the first cat.
func (c *Client) BuddyFirst(ctx context.Context, client *http.Client, acct *auth.Account) error {
	_, err := c.post(ctx, client, acct, pathBuddyFirst)
	return err
}

// BuddyAgreement fetches the adoption agreement text the client shows before
// BuddyFirst.
//
// The body is returned as an object rather than a string because the upstream
// ships the agreement as structured content (title + sections), and flattening
// it here would be the wrong place to decide how it renders.
func (c *Client) BuddyAgreement(ctx context.Context, client *http.Client, acct *auth.Account) (map[string]any, error) {
	return c.get(ctx, client, acct, pathBuddyAgree)
}

// TravelStatus returns the buddy's trip state.
func (c *Client) TravelStatus(ctx context.Context, client *http.Client, acct *auth.Account) (TravelState, error) {
	body, err := c.get(ctx, client, acct, pathTravelState)
	if err != nil {
		return TravelState{}, err
	}
	node := dataObject(body)
	if nested, ok := node["travel"].(map[string]any); ok {
		node = nested
	}
	state := TravelState{
		State:             stringField(node, "state"),
		LocationID:        intField(node, "location_id"),
		DepartAt:          int64Field(node, "depart_at"),
		ArriveAt:          int64Field(node, "arrive_at"),
		DailyLimitReached: boolField(node, "daily_limit_reached"),
		RewardCredit:      intField(node, "reward_credit"),
	}
	if location, ok := node["location"].(map[string]any); ok {
		state.Location = stringField(location, "name")
		if state.LocationID == 0 {
			state.LocationID = intField(location, "id")
		}
	} else {
		state.Location = stringField(node, "location")
	}
	return state, nil
}

// TravelDepart sends the cat out, optionally to a named destination.
//
// locationID 0 asks the upstream to choose. The Python reference sends the
// first entry of the /travel/config list because an empty body is rejected;
// that lookup belongs to the caller's policy, so it takes the id here instead
// of hiding a second request inside this one.
func (c *Client) TravelDepart(ctx context.Context, client *http.Client, acct *auth.Account, locationID int) error {
	if err := c.gate(acct); err != nil {
		return err
	}
	payload := map[string]any{}
	if locationID > 0 {
		payload["location_id"] = locationID
	}
	decoded, _, status, err := c.Up.DoJSON(ctx, client, http.MethodPost,
		c.chatURL(acct, pathTravelGo), upstream.Headers(acct, "chat"), payload)
	if err != nil {
		return fmt.Errorf("growth: travel depart: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("growth: travel depart: HTTP %d", status)
	}
	if err := envelopeError(decoded); err != nil {
		return fmt.Errorf("growth: travel depart: %w", err)
	}
	return nil
}

// TravelClaim collects the reward for a finished trip.
func (c *Client) TravelClaim(ctx context.Context, client *http.Client, acct *auth.Account) (RedeemResult, error) {
	body, err := c.post(ctx, client, acct, pathTravelClaim)
	if err != nil {
		return RedeemResult{}, err
	}
	data := dataObject(body)
	return RedeemResult{
		OK:     true,
		Credit: intField(data, "reward_credit"),
		Energy: intField(data, "energy"),
	}, nil
}

// gate refuses a realm that has no growth centre.
//
// Every request builder in this file goes through it, so the "国际版无此功能"
// answer is not something a caller has to remember to check first.
func (c *Client) gate(acct *auth.Account) error {
	if acct == nil {
		return fmt.Errorf("%w: no account", ErrNotSupported)
	}
	if !acct.RealmConfig().HasCheckin {
		return fmt.Errorf("%w: realm %s", ErrNotSupported, acct.RealmID())
	}
	return nil
}

// get performs one gated GET and returns the decoded envelope.
func (c *Client) get(ctx context.Context, client *http.Client, acct *auth.Account, path string) (map[string]any, error) {
	if err := c.gate(acct); err != nil {
		return nil, err
	}
	decoded, _, status, err := c.Up.DoJSON(ctx, client, http.MethodGet,
		c.chatURL(acct, path), upstream.Headers(acct, "chat"), nil)
	if err != nil {
		return nil, fmt.Errorf("growth: GET %s: %w", path, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("growth: GET %s: HTTP %d", path, status)
	}
	if err := envelopeError(decoded); err != nil {
		return nil, fmt.Errorf("growth: GET %s: %w", path, err)
	}
	return decoded, nil
}

// post performs one gated POST with an empty body and returns the envelope.
func (c *Client) post(ctx context.Context, client *http.Client, acct *auth.Account, path string) (map[string]any, error) {
	if err := c.gate(acct); err != nil {
		return nil, err
	}
	decoded, _, status, err := c.Up.DoJSON(ctx, client, http.MethodPost,
		c.chatURL(acct, path), upstream.Headers(acct, "chat"), map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("growth: POST %s: %w", path, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("growth: POST %s: HTTP %d", path, status)
	}
	if err := envelopeError(decoded); err != nil {
		return nil, fmt.Errorf("growth: POST %s: %w", path, err)
	}
	return decoded, nil
}

// chatURL joins a growth path onto the account's chat upstream.
//
// Growth lives on the chat host, not the billing host, even for the endpoints
// that talk about credits — the two hosts are only the same on the
// international build, which has no growth at all.
func (c *Client) chatURL(acct *auth.Account, path string) string {
	return acct.RealmConfig().ChatUpstream + path
}

// escapePath makes one path segment safe to interpolate.
//
// Task codes come from upstream but are used to build a URL, so anything that
// could change the path shape is percent-encoded rather than trusted.
func escapePath(segment string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(segment); i++ {
		ch := segment[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9',
			ch == '-', ch == '_', ch == '.', ch == '~':
			b.WriteByte(ch)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[ch>>4])
			b.WriteByte(hexDigits[ch&0x0f])
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// envelopeError turns an upstream business code into an error.
//
// The growth endpoints answer HTTP 200 with {"code":<non-zero>,"msg":"..."}
// for refusals, and the message is the only useful part — "task not completed"
// is a different situation from a rate limit, and the caller's retry policy
// needs to see which one it is.
func envelopeError(decoded map[string]any) error {
	if decoded == nil {
		return fmt.Errorf("empty upstream response")
	}
	code := intField(decoded, "code")
	if code == 0 {
		return nil
	}
	msg := firstNonEmpty(stringField(decoded, "msg"), stringField(decoded, "message"))
	if msg == "" {
		msg = fmt.Sprintf("code=%d", code)
	}
	return fmt.Errorf("%s (code=%d)", msg, code)
}

// dataObject returns the envelope's "data" object, or an empty map.
//
// A missing or mistyped "data" is not an error: several endpoints answer with a
// bare {"code":0} and every field's absence has a defined zero value, so
// treating it as a failure would turn "nothing to report" into an outage.
func dataObject(decoded map[string]any) map[string]any {
	if decoded == nil {
		return map[string]any{}
	}
	if data, ok := decoded["data"].(map[string]any); ok {
		return data
	}
	return map[string]any{}
}

// listFrom reads an array out of the envelope, preferring the snake_case field
// and falling back to the camelCase spelling some builds emit.
func listFrom(decoded map[string]any, field string) []map[string]any {
	if decoded == nil {
		return nil
	}
	raw, ok := dataObject(decoded)[field]
	if !ok {
		raw, ok = dataObject(decoded)[camel(field)]
		if !ok {
			return nil
		}
	}
	items, ok := raw.([]any)
	if !ok {
		// A single object where a list was expected is still usable: wrapping
		// it is strictly better than reporting "no tasks today".
		if one, ok := raw.(map[string]any); ok {
			return []map[string]any{one}
		}
		return nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if row, ok := item.(map[string]any); ok {
			out = append(out, row)
		}
	}
	return out
}

// camel converts a snake_case field name to lowerCamelCase.
func camel(field string) string {
	idx := strings.IndexByte(field, '_')
	if idx < 0 {
		return field
	}
	var b strings.Builder
	b.WriteString(field[:idx])
	upper := true
	for i := idx; i < len(field); i++ {
		if field[i] == '_' {
			upper = true
			continue
		}
		if upper {
			b.WriteByte(field[i] &^ 0x20)
			upper = false
			continue
		}
		b.WriteByte(field[i])
	}
	return b.String()
}

// stringField reads a string, tolerating a numeric value that upstream encoded
// as a number rather than a string.
func stringField(node map[string]any, field string) string {
	if node == nil {
		return ""
	}
	switch v := node[field].(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	case float64:
		return trimFloat(v)
	case bool:
		return ""
	default:
		return ""
	}
}

// intField reads an integer, tolerating a string or float encoding.
func intField(node map[string]any, field string) int {
	if node == nil {
		return 0
	}
	switch v := node[field].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case string:
		return parseInt(v)
	default:
		return 0
	}
}

// int64Field is intField for timestamps that may exceed an int32.
func int64Field(node map[string]any, field string) int64 {
	if node == nil {
		return 0
	}
	switch v := node[field].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case string:
		return int64(parseInt(v))
	default:
		return 0
	}
}

// boolField reads a boolean, tolerating the 0/1 and "true" encodings.
func boolField(node map[string]any, field string) bool {
	if node == nil {
		return false
	}
	switch v := node[field].(type) {
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1", "yes":
			return true
		}
		return false
	default:
		return false
	}
}

// parseFloat is a small numeric parser so the package stays dependency-free of
// strconv's error plumbing at every call site.
//
// It deliberately accepts only what the upstream emits: an optional sign,
// digits, one decimal point and an optional exponent. Anything else is 0, which
// is the same answer a missing field gets.
func parseFloat(s string) (float64, bool) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, false
	}
	var (
		value   float64
		digits  int
		sign    = 1.0
		seenDot bool
		scale   = 1.0
	)
	i := 0
	if trimmed[0] == '+' || trimmed[0] == '-' {
		if trimmed[0] == '-' {
			sign = -1
		}
		i = 1
	}
	for ; i < len(trimmed); i++ {
		ch := trimmed[i]
		switch {
		case ch >= '0' && ch <= '9':
			digits++
			if seenDot {
				scale /= 10
				value += float64(ch-'0') * scale
			} else {
				value = value*10 + float64(ch-'0')
			}
		case ch == '.':
			if seenDot {
				return 0, false
			}
			seenDot = true
		default:
			return 0, false
		}
	}
	if digits == 0 {
		return 0, false
	}
	return sign * value, true
}

// parseInt reads an integer prefix from a string, returning 0 when there is
// none. A fractional part is dropped, matching intField's float behaviour.
func parseInt(s string) int {
	value, ok := parseFloat(strings.TrimSpace(s))
	if !ok {
		return 0
	}
	return int(value)
}

// trimFloat renders a float without a trailing ".0" so an id encoded as a
// number reads back the way the panel expects.
func trimFloat(v float64) string {
	if v == float64(int64(v)) {
		return formatInt(int64(v))
	}
	return formatFloat(v)
}

// formatInt renders an int64 in decimal without importing strconv.
func formatInt(v int64) string {
	if v == 0 {
		return "0"
	}
	negative := v < 0
	var buf [24]byte
	i := len(buf)
	u := uint64(v)
	if negative {
		// Guard the int64 minimum, whose negation overflows.
		u = uint64(-v)
	}
	for u > 0 {
		i--
		buf[i] = byte('0' + u%10)
		u /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// formatFloat renders a float with three decimals, which is the precision the
// gateway's own JSON surfaces use.
func formatFloat(v float64) string {
	whole := int64(v)
	frac := v - float64(whole)
	if frac < 0 {
		frac = -frac
	}
	scaled := int64(frac*1000 + 0.5)
	return formatInt(whole) + "." + formatInt(scaled)
}

// firstNonEmpty returns the first non-blank string.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// trimCodes normalises a list of task codes, dropping blanks and duplicates.
//
// Duplicates matter: the upstream rejects an accept batch that names the same
// task twice, which would fail the whole batch for the sake of one repeated
// entry from a caller that concatenated two sources.
func trimCodes(codes []string) []string {
	seen := make(map[string]struct{}, len(codes))
	out := make([]string, 0, len(codes))
	for _, code := range codes {
		trimmed := strings.TrimSpace(code)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}
