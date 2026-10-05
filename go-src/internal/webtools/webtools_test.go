package webtools

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestFetchRejectsPrivateAndLoopbackHosts(t *testing.T) {
	blocked := []string{
		"http://127.0.0.1/admin",
		"http://10.1.2.3/",
		"http://192.168.1.1/",
		"http://169.254.169.254/latest/meta-data/",
		"http://0.0.0.0/",
		"http://172.16.0.1/",
		"http://172.31.255.254/",
		"http://localhost:8080/",
		"http://service.localhost/",
		"http://intranet/",
		"file:///etc/passwd",
		"ftp://example.com/",
		"gopher://example.com/",
		"//example.com/",
		"http://user:secret@example.com/",
		"http://user@example.com/",
	}
	for _, raw := range blocked {
		got := Fetch(raw, 0)
		if !strings.HasPrefix(got, "Error: ") {
			t.Errorf("Fetch(%q) = %q, want an Error: refusal", raw, got)
		}
	}
}

func TestFetchAllowsPublicHost(t *testing.T) {
	// Only the guard is exercised: a public host must get past it, so the
	// failure (if any) is about the network, not the address.
	if _, problem := guardURL("https://example.com/page"); problem != "" {
		t.Fatalf("public URL refused: %s", problem)
	}
	if _, problem := guardURL("http://172.32.0.1/"); problem != "" {
		t.Fatalf("172.32/12 is public but was refused: %s", problem)
	}
}

func TestSourcesFromResultRoundTrips(t *testing.T) {
	formatted := "Search results for: go regexp\n\n" +
		"1. First hit\n   https://one.example/a\n   first snippet\n\n" +
		"2. Second hit\n   https://two.example/b?q=1\n   second snippet\n\n" +
		"3. First hit\n   https://one.example/a\n   duplicate\n\n" +
		"Cite the sources you used at the end of your answer."

	sources := SourcesFromResult(formatted)
	if len(sources) != 2 {
		t.Fatalf("got %d sources, want 2 (duplicate dropped): %+v", len(sources), sources)
	}
	if sources[0].Title != "First hit" || sources[0].URL != "https://one.example/a" {
		t.Errorf("first source = %+v", sources[0])
	}
	if sources[1].Title != "Second hit" || sources[1].URL != "https://two.example/b?q=1" {
		t.Errorf("second source = %+v", sources[1])
	}
}

func TestSourcesFromResultReadsRealSearchOutput(t *testing.T) {
	// Shapes the real formatter produces, without touching the network.
	got := SourcesFromResult("Search results for: x\n\n1. Title only\n   https://a.example/x.\n   snip\n")
	if len(got) != 1 {
		t.Fatalf("got %d sources, want 1: %+v", len(got), got)
	}
	if got[0].URL != "https://a.example/x" {
		t.Errorf("trailing punctuation was kept: %q", got[0].URL)
	}

	if got := SourcesFromResult("No results found for: x"); len(got) != 0 {
		t.Errorf("empty result produced sources: %+v", got)
	}
}

func TestClientWantsWeb(t *testing.T) {
	cases := []struct {
		name       string
		tools      []any
		wantSearch bool
		wantFetch  bool
	}{
		{
			name:       "server-shaped",
			tools:      []any{map[string]any{"type": "web_search"}},
			wantSearch: true,
		},
		{
			name:       "server-shaped preview",
			tools:      []any{map[string]any{"type": "web_search_preview_2025_03_11"}},
			wantSearch: true,
		},
		{
			name:      "server-shaped fetch",
			tools:     []any{map[string]any{"type": "web_fetch"}},
			wantFetch: true,
		},
		{
			name:       "function-shaped",
			tools:      []any{map[string]any{"type": "function", "name": "web_search"}},
			wantSearch: true,
		},
		{
			name:      "chat-shaped nested function",
			tools:     []any{map[string]any{"type": "function", "function": map[string]any{"name": "Web_Fetch"}}},
			wantFetch: true,
		},
		{
			name:       "whitespace and case",
			tools:      []any{map[string]any{"type": "  WEB_SEARCH "}},
			wantSearch: true,
		},
		{
			name:  "unrelated tools",
			tools: []any{map[string]any{"type": "function", "name": "get_weather"}, "not-a-map", nil},
		},
		{
			name:       "both",
			tools:      []any{map[string]any{"type": "web_fetch"}, map[string]any{"type": "web_search"}},
			wantSearch: true,
			wantFetch:  true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			search, fetch := ClientWantsWeb(tc.tools)
			if search != tc.wantSearch || fetch != tc.wantFetch {
				t.Errorf("ClientWantsWeb = (%v, %v), want (%v, %v)",
					search, fetch, tc.wantSearch, tc.wantFetch)
			}
		})
	}
}

func TestInstallToolDefsReplacesEveryDeclaration(t *testing.T) {
	chatTools := []any{
		map[string]any{"type": "web_search"},
		map[string]any{"type": "web_search_preview"},
		map[string]any{"type": "function", "name": "web_search"},
		map[string]any{"type": "function", "function": map[string]any{"name": "web_fetch"}},
		map[string]any{"type": "function", "name": "get_weather"},
		nil,
	}
	installed := InstallToolDefs(chatTools, true, false)
	if len(installed) != 3 {
		t.Fatalf("got %d tools, want 3: %+v", len(installed), installed)
	}

	searchCount, weatherSeen := 0, false
	for _, raw := range installed {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if entry["name"] == WebSearchName {
			searchCount++
		}
		if entry["name"] == "get_weather" {
			weatherSeen = true
		}
		if entry["name"] == WebFetchName {
			t.Errorf("web_fetch installed although it was not wanted")
		}
	}
	if searchCount != 1 {
		t.Errorf("installed %d web_search defs, want exactly 1", searchCount)
	}
	if !weatherSeen {
		t.Errorf("unrelated tool was dropped")
	}
}

func TestInstallToolDefsNothingWanted(t *testing.T) {
	installed := InstallToolDefs([]any{map[string]any{"type": "web_search"}}, false, false)
	if len(installed) != 0 {
		t.Fatalf("got %d tools, want an empty result: %+v", len(installed), installed)
	}
}

func TestExecuteNeverReturnsNonStringOrPanics(t *testing.T) {
	cases := []struct {
		name    string
		tool    string
		argsRaw any
	}{
		{"broken json", WebSearchName, "{not json"},
		{"json array", WebSearchName, "[1,2,3]"},
		{"json null", WebSearchName, "null"},
		{"json scalar", WebSearchName, "42"},
		{"nil args", WebSearchName, nil},
		{"wrong value types", WebSearchName, map[string]any{"query": map[string]any{"a": 1}}},
		{"fetch with number", WebFetchName, map[string]any{"url": 12}},
		{"fetch with list", WebFetchName, map[string]any{"urls": []any{123, nil}}},
		{"fetch bad json", WebFetchName, "<xml/>"},
		{"unknown tool", "definitely_not_a_tool", "{}"},
		{"empty name", "", nil},
		{"chan args", WebSearchName, make(chan int)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Execute(tc.tool, tc.argsRaw)
			if got == "" {
				t.Fatalf("Execute returned an empty string")
			}
			if strings.Contains(got, "panic") {
				t.Fatalf("Execute leaked a panic: %q", got)
			}
		})
	}
}

func TestExecuteRoutesArguments(t *testing.T) {
	// An unusable URL must be refused before any request goes out, which also
	// proves the arguments reached Fetch.
	got := Execute(WebFetchName, `{"url": "http://127.0.0.1:9/"}`)
	if !strings.Contains(got, "Private network addresses are not allowed.") {
		t.Errorf("Execute did not route the url argument: %q", got)
	}
	if got := Execute(WebSearchName, `{"queries": []}`); !strings.Contains(got, "at least 2 characters") {
		t.Errorf("empty queries list was not reported: %q", got)
	}
}

func TestQueryAndURLArg(t *testing.T) {
	if got := QueryArgs(map[string]any{"query": "a"}); got != "a" {
		t.Errorf("query: %q", got)
	}
	if got := QueryArgs(map[string]any{"queries": []any{"a", "", nil, " b "}}); got != "a or b" {
		t.Errorf("queries list: %q", got)
	}
	if got := QueryArgs(map[string]any{"q": "solo"}); got != "solo" {
		t.Errorf("q: %q", got)
	}
	if got := QueryArgs(map[string]any{"query": nil, "queries": []any{"fallback"}}); got != "fallback" {
		t.Errorf("null query did not fall through: %q", got)
	}
	if got := QueryArgs(nil); got != "" {
		t.Errorf("nil args: %q", got)
	}

	if got := URLArg(map[string]any{"url": " https://a.example "}); got != "https://a.example" {
		t.Errorf("url: %q", got)
	}
	if got := URLArg(map[string]any{"urls": []any{"", "https://b.example"}}); got != "https://b.example" {
		t.Errorf("urls list: %q", got)
	}
	if got := URLArg(map[string]any{"urls": []any{"", nil}}); got != "" {
		t.Errorf("urls with no usable entry: %q", got)
	}
}

func TestMaxRounds(t *testing.T) {
	cases := map[string]int{"": 3, "0": 3, "-3": 3, "abc": 3, "1": 1, "6": 6, "8": 8, "9": 8, "100": 8, " 4 ": 4}
	for value, want := range cases {
		t.Setenv("WB_MAX_WEB_ROUNDS", value)
		if got := MaxRounds(); got != want {
			t.Errorf("WB_MAX_WEB_ROUNDS=%q -> %d, want %d", value, got, want)
		}
	}
	os.Unsetenv("WB_MAX_WEB_ROUNDS")
	if got := MaxRounds(); got != defaultRounds {
		t.Errorf("unset -> %d, want %d", got, defaultRounds)
	}
}

func TestRoundTripSources(t *testing.T) {
	// Build the same text the formatter emits and read the citations back.
	page := "1. Alpha\n   https://alpha.example/one\n   alpha snippet\n\n" +
		"2. Beta\n   https://beta.example/two\n   beta snippet"
	formatted := "Search results for: anything\n\n" + page +
		"\n\nCite the sources you used at the end of your answer."

	sources := SourcesFromResult(formatted)
	if len(sources) != 2 {
		t.Fatalf("got %d sources, want 2: %+v", len(sources), sources)
	}
	blob, err := json.Marshal(sources)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `[{"title":"Alpha","url":"https://alpha.example/one"},{"title":"Beta","url":"https://beta.example/two"}]`
	if string(blob) != want {
		t.Errorf("sources = %s, want %s", blob, want)
	}
}

func TestToolDefsShape(t *testing.T) {
	search := SearchToolDef()
	if search["type"] != "function" || search["name"] != WebSearchName {
		t.Errorf("search def identity: %+v", search)
	}
	params, ok := search["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("search def has no parameters object: %+v", search)
	}
	if required, ok := params["required"].([]any); !ok || len(required) != 1 || required[0] != "query" {
		t.Errorf("search required = %v", params["required"])
	}
	if _, err := json.Marshal(search); err != nil {
		t.Errorf("search def is not JSON-serialisable: %v", err)
	}

	fetch := FetchToolDef()
	if fetch["type"] != "function" || fetch["name"] != WebFetchName {
		t.Errorf("fetch def identity: %+v", fetch)
	}
	if _, err := json.Marshal(fetch); err != nil {
		t.Errorf("fetch def is not JSON-serialisable: %v", err)
	}
}

func TestIsInternalTool(t *testing.T) {
	for _, name := range []string{"web_search", " web_fetch ", WebSearchName} {
		if !IsInternalTool(name) {
			t.Errorf("IsInternalTool(%q) = false", name)
		}
	}
	for _, name := range []string{"", " get_weather", "web_search_preview", "WEB_SEARCH"} {
		if IsInternalTool(name) {
			t.Errorf("IsInternalTool(%q) = true", name)
		}
	}
}

func TestStripTags(t *testing.T) {
	in := `<html><head><style>body{color:red}</style><script>var x = 1;</script>` +
		`</head><body><h1>Title</h1><p>one&nbsp;two &amp; three</p><ul><li>a</li><li>b</li></ul>` +
		`<noscript>fallback</noscript><br><div>last</div></body></html>`
	got := StripTags(in)

	for _, unwanted := range []string{"color:red", "var x", "fallback", "<", ">"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("StripTags kept %q: %q", unwanted, got)
		}
	}
	for _, wanted := range []string{"Title", "one two & three", "a", "b", "last"} {
		if !strings.Contains(got, wanted) {
			t.Errorf("StripTags lost %q: %q", wanted, got)
		}
	}
	if strings.Contains(got, "\u00a0") {
		t.Errorf("non-breaking space survived: %q", got)
	}
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("blank runs were not collapsed: %q", got)
	}
}

func TestGuardURLRejectsNonHTTP(t *testing.T) {
	for _, raw := range []string{"", "example.com", "javascript:alert(1)", "data:text/html,x", "https://"} {
		if _, problem := guardURL(raw); problem == "" {
			t.Errorf("guardURL(%q) accepted the URL", raw)
		}
	}
}
