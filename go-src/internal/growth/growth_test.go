package growth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"wb2hub/internal/auth"
	"wb2hub/internal/config"
	"wb2hub/internal/upstream"
)

// nightAt builds a local wall-clock time on the fixed day these tests use, so
// the boundary assertions are values rather than something the suite waits for.
func nightAt(hour, min int) time.Time {
	return time.Date(2026, time.September, 15, hour, min, 0, 0, time.Local)
}

func TestInNightWindowBoundaries(t *testing.T) {
	cases := []struct {
		hour, min int
		want      bool
	}{
		{22, 59, false},
		{23, 0, true},
		{23, 59, true},
		{0, 0, true},
		{7, 59, true},
		{8, 0, false},
		{12, 0, false},
	}
	for _, c := range cases {
		got := InNightWindow(nightAt(c.hour, c.min))
		if got != c.want {
			t.Errorf("InNightWindow(%02d:%02d) = %v, want %v", c.hour, c.min, got, c.want)
		}
	}
}

func TestShouldSkipDesktopOnlyTasks(t *testing.T) {
	// Midday: the night window is irrelevant, so anything that skips here is
	// skipping for the desktop-only reason.
	noon := nightAt(12, 0)
	for code, action := range DesktopOnlyTasks {
		skip, reason := ShouldSkip(code, noon)
		if !skip {
			t.Errorf("ShouldSkip(%q) = false, want true", code)
		}
		if reason == "" {
			t.Errorf("ShouldSkip(%q) returned an empty reason", code)
		}
		if !contains(reason, action) {
			t.Errorf("ShouldSkip(%q) reason %q does not name the action %q", code, reason, action)
		}
	}
	// The four codes are exactly the ones the notes document; a stray fifth
	// entry would silently stop reporting a forgeable task.
	if len(DesktopOnlyTasks) != 4 {
		t.Errorf("DesktopOnlyTasks has %d entries, want 4", len(DesktopOnlyTasks))
	}
	for _, code := range []string{"RichMeow_Chat", "Library_read", "Buddy_App", "Buddy_App_QQ"} {
		if _, ok := DesktopOnlyTasks[code]; !ok {
			t.Errorf("DesktopOnlyTasks is missing %q", code)
		}
	}
}

func TestShouldSkipNightTaskOutsideWindow(t *testing.T) {
	daytime := nightAt(12, 0)
	skip, reason := ShouldSkip("black_cat", daytime)
	if !skip {
		t.Fatal("ShouldSkip(black_cat) at midday = false, want true")
	}
	if !contains(reason, "23:00-08:00") {
		t.Errorf("reason %q does not state the window", reason)
	}

	night := nightAt(1, 0)
	if skip, reason := ShouldSkip("black_cat", night); skip {
		t.Errorf("ShouldSkip(black_cat) at 01:00 = true (%q), want false", reason)
	}
}

func TestShouldSkipForgeableTaskIsNotSkipped(t *testing.T) {
	night := nightAt(1, 0)
	for _, code := range []string{"create_canvas", "chat_5", "expert_5", "Expert_team_use_3"} {
		if skip, reason := ShouldSkip(code, night); skip {
			t.Errorf("ShouldSkip(%q) = true (%q), want false", code, reason)
		}
	}
}

func TestShouldSkipUnforgeableTask(t *testing.T) {
	skip, reason := ShouldSkip("Expert_Philanthropy", nightAt(12, 0))
	if !skip {
		t.Fatal("ShouldSkip(Expert_Philanthropy) = false, want true")
	}
	if !contains(reason, "真实捐款动作") {
		t.Errorf("reason %q does not explain the real action", reason)
	}
}

func TestNightTaskCodesTable(t *testing.T) {
	if _, ok := NightTaskCodes["black_cat"]; !ok {
		t.Fatal("NightTaskCodes is missing black_cat")
	}
	if len(NightTaskCodes) != 1 {
		t.Errorf("NightTaskCodes has %d entries, want 1", len(NightTaskCodes))
	}
}

func TestTaskTableLookups(t *testing.T) {
	cases := []struct {
		code   string
		name   string
		reward int
	}{
		{"create_canvas", "创建设计任务", 300},
		{"template_5", "模板创建任务", 200},
		{"expert_5", "使用专家助手", 200},
		{"Expert_team_use_3", "使用专家团队", 150},
		{"skill_1", "体验技能", 100},
		{"automation_1", "创建自动化任务", 100},
		{"playbook_prompt", "灵感案例使用", 100},
		{"Expert_lighthouse", "轻量云专家使用", 100},
		{"Buddy_App", "进入 Buddy 应用", 100},
		{"Buddy_App_QQ", "企鹅教师助手", 100},
		{"Hp_Appearance", "应用主题外观", 100},
		{"chat_5", "发起 5 次对话", 100},
		{"Model_chat_GLM5.2", "体验 GLM-5.2", 100},
		{"black_cat", "夜猫子任务 (23:00-08:00)", 100},
		{"RichMeow_Chat", "桌面对话事件链", 100},
		{"Library_read", "浏览资料库", 100},
		{"first_buddy", "领养首只猫猫", 0},
		{"Expert_Philanthropy", "公益爱心捐赠", 0},
	}
	if len(cases) != len(taskTable) {
		t.Errorf("the table has %d entries but the test covers %d", len(taskTable), len(cases))
	}
	for _, c := range cases {
		if got := TaskName(c.code); got != c.name {
			t.Errorf("TaskName(%q) = %q, want %q", c.code, got, c.name)
		}
		if got := TaskReward(c.code); got != c.reward {
			t.Errorf("TaskReward(%q) = %d, want %d", c.code, got, c.reward)
		}
		if got := TaskName(c.code); got == "" {
			t.Errorf("TaskName(%q) is empty", c.code)
		}
	}

	// Unknown codes must be zero values, never a panic or a guess.
	if got := TaskName("no_such_task"); got != "" {
		t.Errorf("TaskName(unknown) = %q, want %q", got, "")
	}
	if got := TaskReward("no_such_task"); got != 0 {
		t.Errorf("TaskReward(unknown) = %d, want 0", got)
	}
	if got := TaskTarget("no_such_task"); got != 0 {
		t.Errorf("TaskTarget(unknown) = %d, want 0", got)
	}

	// The codes the note table documents carry their target and kind.
	if got := TaskTarget("black_cat"); got != 3 {
		t.Errorf("TaskTarget(black_cat) = %d, want 3", got)
	}
	if got := TaskTarget("Expert_team_use_3"); got != 3 {
		t.Errorf("TaskTarget(Expert_team_use_3) = %d, want 3", got)
	}
	if got := TaskKind("expert_5"); got != "expert" {
		t.Errorf("TaskKind(expert_5) = %q, want %q", got, "expert")
	}
	if got := TaskKind("Expert_team_use_3"); got != "team" {
		t.Errorf("TaskKind(Expert_team_use_3) = %q, want %q", got, "team")
	}
	if got := TaskKind("black_cat"); got != "cat" {
		t.Errorf("TaskKind(black_cat) = %q, want %q", got, "cat")
	}
}

func TestIsUnforgeable(t *testing.T) {
	ok, reason := IsUnforgeable("Expert_Philanthropy")
	if !ok || reason == "" {
		t.Errorf("IsUnforgeable(Expert_Philanthropy) = (%v, %q), want (true, non-empty)", ok, reason)
	}
	if ok, _ := IsUnforgeable("chat_5"); ok {
		t.Error("IsUnforgeable(chat_5) = true, want false")
	}
}

func TestIDPoolsAreIntact(t *testing.T) {
	if len(ExpertIDPool) != 18 {
		t.Errorf("ExpertIDPool has %d entries, want 18", len(ExpertIDPool))
	}
	if len(TeamIDPool) != 8 {
		t.Errorf("TeamIDPool has %d entries, want 8", len(TeamIDPool))
	}
	// The ids are the substance of the pool: the upstream deduplicates by
	// (eventCode, id), so a repeated id silently stalls a task.
	seen := map[string]bool{}
	for _, entry := range ExpertIDPool {
		if entry.ID == "" || entry.Name == "" {
			t.Errorf("expert entry %+v is incomplete", entry)
		}
		if seen[entry.ID] {
			t.Errorf("expert id %q appears twice", entry.ID)
		}
		seen[entry.ID] = true
	}
	if ExpertIDPool[0].ID != "ex_PZw8Gu81HfN4" || ExpertIDPool[0].Name != "运维工程师" {
		t.Errorf("ExpertIDPool[0] = %+v, want the 运维工程师 entry", ExpertIDPool[0])
	}
	if expert, ok := expertByName("腾讯轻量云专家"); !ok || expert != "ex_2cvvUZQhDyeJ" {
		t.Errorf("the lighthouse expert id = %q (found=%v), want ex_2cvvUZQhDyeJ", expert, ok)
	}
	seenTeam := map[string]bool{}
	for _, entry := range TeamIDPool {
		if entry.ID == "" || entry.Name == "" {
			t.Errorf("team entry %+v is incomplete", entry)
		}
		if seenTeam[entry.ID] {
			t.Errorf("team id %q appears twice", entry.ID)
		}
		seenTeam[entry.ID] = true
	}
	if TeamIDPool[0].ID != "CloudOpsTeam" || TeamIDPool[0].Name != "运维专家团队" {
		t.Errorf("TeamIDPool[0] = %+v, want the 运维专家团队 entry", TeamIDPool[0])
	}
}

// expertByName is a test helper that looks an expert up by display name.
func expertByName(name string) (string, bool) {
	for _, entry := range ExpertIDPool {
		if entry.Name == name {
			return entry.ID, true
		}
	}
	return "", false
}

func TestTrimCodesDropsBlanksAndDuplicates(t *testing.T) {
	got := trimCodes([]string{" chat_5 ", "", "chat_5", "skill_1", "   "})
	want := []string{"chat_5", "skill_1"}
	if len(got) != len(want) {
		t.Fatalf("trimCodes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("trimCodes = %v, want %v", got, want)
		}
	}
	if out := trimCodes(nil); len(out) != 0 {
		t.Errorf("trimCodes(nil) = %v, want empty", out)
	}
}

func TestEscapePathPercentEncodes(t *testing.T) {
	if got := escapePath("black_cat"); got != "black_cat" {
		t.Errorf("escapePath(black_cat) = %q, want unchanged", got)
	}
	if got := escapePath("../etc/passwd"); contains(got, "/") {
		t.Errorf("escapePath encoded a path separator as %q", got)
	}
	if got := escapePath(""); got == "" {
		t.Error("escapePath(\"\") returned an empty segment")
	}
	// The dash, underscore and dot, which real task codes use, must survive.
	if got := escapePath("Expert_team_use_3"); got != "Expert_team_use_3" {
		t.Errorf("escapePath mangled a real code: %q", got)
	}
	if got := escapePath("Model_chat_GLM5.2"); got != "Model_chat_GLM5.2" {
		t.Errorf("escapePath mangled the dotted code: %q", got)
	}
}

func TestEnvelopeError(t *testing.T) {
	if err := envelopeError(nil); err == nil {
		t.Error("envelopeError(nil) = nil, want an error")
	}
	if err := envelopeError(map[string]any{}); err != nil {
		t.Errorf("envelopeError({}) = %v, want nil (a missing code is code 0)", err)
	}
	if err := envelopeError(map[string]any{"code": float64(0)}); err != nil {
		t.Errorf("envelopeError(code=0) = %v, want nil", err)
	}
	err := envelopeError(map[string]any{"code": float64(400), "msg": "task not completed"})
	if err == nil {
		t.Fatal("envelopeError(code=400) = nil, want an error")
	}
	if !contains(err.Error(), "task not completed") {
		t.Errorf("error %q loses the upstream wording", err)
	}
}

func TestNumericHelpersTolerateUpstreamEncodings(t *testing.T) {
	node := map[string]any{
		"as_float":  float64(12),
		"as_string": "34",
		"as_bool":   true,
		"float_str": "56.9",
		"junk":      "not a number",
		"nil":       nil,
	}
	if got := intField(node, "as_float"); got != 12 {
		t.Errorf("intField(float) = %d, want 12", got)
	}
	if got := intField(node, "as_string"); got != 34 {
		t.Errorf("intField(string) = %d, want 34", got)
	}
	if got := intField(node, "float_str"); got != 56 {
		t.Errorf("intField(quoted float) = %d, want 56", got)
	}
	if got := intField(node, "junk"); got != 0 {
		t.Errorf("intField(junk) = %d, want 0", got)
	}
	if got := intField(node, "nil"); got != 0 {
		t.Errorf("intField(nil) = %d, want 0", got)
	}
	if got := intField(node, "missing"); got != 0 {
		t.Errorf("intField(missing) = %d, want 0", got)
	}
	if got := boolField(node, "as_bool"); !got {
		t.Error("boolField(true) = false, want true")
	}
	if got := boolField(map[string]any{"x": "1"}, "x"); !got {
		t.Error("boolField(\"1\") = false, want true")
	}
	if got := boolField(node, "missing"); got {
		t.Error("boolField(missing) = true, want false")
	}
	if got := stringField(map[string]any{"n": float64(7)}, "n"); got != "7" {
		t.Errorf("stringField(7) = %q, want \"7\"", got)
	}
	if got := int64Field(map[string]any{"big": float64(1e12)}, "big"); got != 1e12 {
		t.Errorf("int64Field(1e12) = %d, want 1e12", got)
	}
}

func TestDataObjectAndListFrom(t *testing.T) {
	if got := dataObject(nil); len(got) != 0 {
		t.Errorf("dataObject(nil) = %v, want empty", got)
	}
	// "data" present but not an object: every field reads as zero.
	if got := dataObject(map[string]any{"data": "nope"}); len(got) != 0 {
		t.Errorf("dataObject(non-object) = %v, want empty", got)
	}
	// Envelope with no data at all: same answer, not an error.
	envelope := map[string]any{"code": float64(0)}
	if got := listFrom(envelope, "tasks"); got != nil {
		t.Errorf("listFrom(no data) = %v, want nil", got)
	}
	// A single object where a list was expected is still usable.
	wrapped := map[string]any{"data": map[string]any{"tasks": map[string]any{"task_code": "chat_5"}}}
	rows := listFrom(wrapped, "tasks")
	if len(rows) != 1 || stringField(rows[0], "task_code") != "chat_5" {
		t.Errorf("listFrom(wrapped object) = %v, want one row", rows)
	}
	// Non-object entries inside the array are dropped, not fatal.
	mixed := map[string]any{"data": map[string]any{"tasks": []any{"junk", map[string]any{"task_code": "x"}}}}
	if rows := listFrom(mixed, "tasks"); len(rows) != 1 {
		t.Errorf("listFrom(mixed array) = %v, want one row", rows)
	}
	// camelCase fallback.
	camelCase := map[string]any{"data": map[string]any{"makeupCards": []any{map[string]any{"id": "c1"}}}}
	if rows := listFrom(camelCase, "makeup_cards"); len(rows) != 1 {
		t.Errorf("listFrom(camelCase) = %v, want one row", rows)
	}
}

func TestCamel(t *testing.T) {
	cases := map[string]string{
		"task_code":    "taskCode",
		"chances":      "chances",
		"max_days":     "maxDays",
		"jump_url":     "jumpUrl",
		"a_b_c":        "aBC",
		"trailing_":    "trailing",
		"makeup_cards": "makeupCards",
	}
	for in, want := range cases {
		if got := camel(in); got != want {
			t.Errorf("camel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTaskDone(t *testing.T) {
	if !(Task{Current: 3, Target: 3}).Done() {
		t.Error("Task{3,3}.Done() = false, want true")
	}
	if (Task{Current: 2, Target: 3}).Done() {
		t.Error("Task{2,3}.Done() = true, want false")
	}
	// A target of 0 is not "instantly done": it means the target was never
	// reported, and claiming then would earn a guaranteed refusal.
	if (Task{Current: 5, Target: 0}).Done() {
		t.Error("Task with no target reported as done")
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "  ", "x", "y"); got != "x" {
		t.Errorf("firstNonEmpty = %q, want \"x\"", got)
	}
	if got := firstNonEmpty("", " "); got != "" {
		t.Errorf("firstNonEmpty(blank) = %q, want empty", got)
	}
}

func TestFormatIntRoundTrip(t *testing.T) {
	cases := map[int64]string{
		0:    "0",
		7:    "7",
		-12:  "-12",
		1e12: "1000000000000",
	}
	for in, want := range cases {
		if got := formatInt(in); got != want {
			t.Errorf("formatInt(%d) = %q, want %q", in, got, want)
		}
	}
}

// contains is a small substring helper so the assertions stay readable.
func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// growthServer answers every growth request with one scripted body, so the
// Tasks parser can be exercised against the shapes upstream really sends.
func growthServer(t *testing.T, body string) (*httptest.Server, *auth.Account) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	original, ok := config.Realms[config.RealmCN]
	if !ok {
		t.Fatal("the CN realm disappeared from the registry")
	}
	patched := original
	patched.ChatUpstream = server.URL
	config.Realms[config.RealmCN] = patched
	t.Cleanup(func() { config.Realms[config.RealmCN] = original })

	return server, &auth.Account{UID: "uid-cn", Realm: config.RealmCN}
}

func TestTasksParsesAMissingDataSection(t *testing.T) {
	// Every one of these is a legitimate "nothing to report" reply. A parse
	// error here would look to the scheduler like an upstream outage.
	for _, body := range []string{
		`{"code":0}`,
		`{"code":0,"data":null}`,
		`{"code":0,"data":[]}`,
		`{"code":0,"data":{"tasks":[]}}`,
		`{"code":0,"data":{"tasks":null}}`,
	} {
		server, acct := growthServer(t, body)
		tasks, err := New(upstream.New()).Tasks(context.Background(), server.Client(), acct)
		if err != nil {
			t.Errorf("Tasks(%s) = %v, want no error", body, err)
		}
		if len(tasks) != 0 {
			t.Errorf("Tasks(%s) = %+v, want no tasks", body, tasks)
		}
	}
}

func TestTasksFillsInFieldsTheUpstreamOmits(t *testing.T) {
	body := `{"code":0,"data":{"tasks":[
		{"task_code":"chat_5","progress":{"current":2}},
		{"task_code":"black_cat"},
		{"task_code":"no_such_code"}
	]}}`
	server, acct := growthServer(t, body)
	tasks, err := New(upstream.New()).Tasks(context.Background(), server.Client(), acct)
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("Tasks returned %d tasks, want 3", len(tasks))
	}

	// A known code with a partial progress block: the target and reward come
	// from the local table, and the reported current is kept.
	if tasks[0].Name != "发起 5 次对话" || tasks[0].Target != 5 || tasks[0].Reward != 100 {
		t.Errorf("chat_5 = %+v, want the local name, target 5 and reward 100", tasks[0])
	}
	if tasks[0].Current != 2 {
		t.Errorf("chat_5 current = %d, want the reported 2", tasks[0].Current)
	}
	if tasks[0].Status != "not_accepted" {
		t.Errorf("chat_5 status = %q, want the not_accepted default", tasks[0].Status)
	}

	if tasks[1].Target != 3 || tasks[1].Reward != 100 {
		t.Errorf("black_cat = %+v, want target 3 and reward 100", tasks[1])
	}

	// An unknown code still gets a usable row: the code as the name and a
	// one-shot target, so the panel does not render 0/0.
	if tasks[2].Name != "no_such_code" || tasks[2].Target != 1 {
		t.Errorf("unknown code = %+v, want the code as name and target 1", tasks[2])
	}
}

func TestTasksDropsRowsWithNoCode(t *testing.T) {
	// A row with no task_code can never be accepted, reported against or
	// claimed, so it is dropped rather than handed downstream to be guarded
	// against by every consumer.
	body := `{"code":0,"data":{"tasks":[{"title":"no code"},{"task_code":"chat_5"}]}}`
	server, acct := growthServer(t, body)
	tasks, err := New(upstream.New()).Tasks(context.Background(), server.Client(), acct)
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Code != "chat_5" {
		t.Errorf("Tasks = %+v, want only the row with a code", tasks)
	}
}

func TestTasksReportsABusinessError(t *testing.T) {
	server, acct := growthServer(t, `{"code":403,"msg":"realm not enabled"}`)
	_, err := New(upstream.New()).Tasks(context.Background(), server.Client(), acct)
	if err == nil {
		t.Fatal("Tasks on a non-zero business code = nil, want an error")
	}
	if !contains(err.Error(), "realm not enabled") {
		t.Errorf("error %q loses the upstream wording", err)
	}
}

func TestTasksRefusesTheInternationalRealm(t *testing.T) {
	// The gate must fire before any request is built, so the URL the account
	// would have produced is never dereferenced.
	_, err := New(upstream.New()).Tasks(context.Background(), &http.Client{},
		&auth.Account{UID: "uid-intl", Realm: config.RealmIntl})
	if !errors.Is(err, ErrNotSupported) {
		t.Errorf("Tasks on the intl realm = %v, want ErrNotSupported", err)
	}
}

func TestEveryGatedMethodRefusesTheInternationalRealm(t *testing.T) {
	acct := &auth.Account{UID: "uid-intl", Realm: config.RealmIntl}
	client := New(upstream.New())
	ctx := context.Background()
	httpClient := &http.Client{Timeout: time.Second}

	calls := map[string]func() error{
		"Tasks":      func() error { _, err := client.Tasks(ctx, httpClient, acct); return err },
		"AcceptTask": func() error { return client.AcceptTask(ctx, httpClient, acct, "chat_5") },
		"AcceptTasks": func() error {
			return client.AcceptTasks(ctx, httpClient, acct, []string{"chat_5"})
		},
		"ClaimTask": func() error { _, err := client.ClaimTask(ctx, httpClient, acct, "chat_5"); return err },
		"Energy":    func() error { _, err := client.Energy(ctx, httpClient, acct); return err },
		"Streak":    func() error { _, err := client.Streak(ctx, httpClient, acct); return err },
		"Heatmap":   func() error { _, err := client.Heatmap(ctx, httpClient, acct); return err },
		"LotteryChances": func() error {
			_, err := client.LotteryChances(ctx, httpClient, acct)
			return err
		},
		"LotteryDraw": func() error { _, err := client.LotteryDraw(ctx, httpClient, acct); return err },
		"RedeemTier": func() error {
			_, err := client.RedeemTier(ctx, httpClient, acct, 1)
			return err
		},
		"MakeupCards": func() error { _, err := client.MakeupCards(ctx, httpClient, acct); return err },
		"UseMakeupCard": func() error {
			_, err := client.UseMakeupCard(ctx, httpClient, acct, "2026-09-01")
			return err
		},
		"BuddyInfo":  func() error { _, err := client.BuddyInfo(ctx, httpClient, acct); return err },
		"BuddyFirst": func() error { return client.BuddyFirst(ctx, httpClient, acct) },
		"BuddyAgreement": func() error {
			_, err := client.BuddyAgreement(ctx, httpClient, acct)
			return err
		},
		"TravelStatus": func() error { _, err := client.TravelStatus(ctx, httpClient, acct); return err },
		"TravelDepart": func() error { return client.TravelDepart(ctx, httpClient, acct, 1) },
		"TravelClaim":  func() error { _, err := client.TravelClaim(ctx, httpClient, acct); return err },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrNotSupported) {
			t.Errorf("%s on the intl realm = %v, want ErrNotSupported", name, err)
		}
	}
}

func TestAcceptWithNothingToDo(t *testing.T) {
	client := New(upstream.New())
	acct := &auth.Account{UID: "uid-cn", Realm: config.RealmCN}

	// An empty batch is "nothing to accept", not a request carrying an empty
	// task_codes array that upstream would reject as malformed. A no-op is the
	// right answer, and it must not reach the network.
	if err := client.AcceptTasks(context.Background(), &http.Client{}, acct, nil); err != nil {
		t.Errorf("AcceptTasks(nil) = %v, want nil", err)
	}
	if err := client.AcceptTasks(context.Background(), &http.Client{}, acct, []string{"", "  "}); err != nil {
		t.Errorf("AcceptTasks(blanks) = %v, want nil", err)
	}

	// A single blank code, in contrast, is a caller bug: it would build
	// /tasks/accept with no codes at all.
	if err := client.AcceptTask(context.Background(), &http.Client{}, acct, "   "); err == nil {
		t.Error("AcceptTask(blank) = nil, want an error")
	}
}

func TestRedeemTierRefusesANonPositiveTier(t *testing.T) {
	client := New(upstream.New())
	for _, tier := range []int{0, -1} {
		_, err := client.RedeemTier(context.Background(), &http.Client{},
			&auth.Account{UID: "uid-cn", Realm: config.RealmCN}, tier)
		if err == nil {
			t.Errorf("RedeemTier(%d) = nil, want an error", tier)
		}
	}
}

func TestClaimTaskRefusesAnEmptyCode(t *testing.T) {
	client := New(upstream.New())
	_, err := client.ClaimTask(context.Background(), &http.Client{},
		&auth.Account{UID: "uid-cn", Realm: config.RealmCN}, "")
	if err == nil {
		t.Error("ClaimTask(\"\") = nil, want an error")
	}
}

func TestNilAccountIsGated(t *testing.T) {
	client := New(upstream.New())
	if _, err := client.Energy(context.Background(), &http.Client{}, nil); !errors.Is(err, ErrNotSupported) {
		t.Errorf("Energy(nil account) = %v, want ErrNotSupported", err)
	}
}

func TestNewFallsBackToADefaultTransport(t *testing.T) {
	if c := New(nil); c == nil || c.Up == nil {
		t.Error("New(nil) did not build a usable client")
	}
}
