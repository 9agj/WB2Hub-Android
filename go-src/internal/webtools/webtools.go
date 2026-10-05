// Package webtools runs web_search and web_fetch on behalf of the gateway.
//
// Ported from wb_webtools.py. A client (Codex App and friends) declares
// web_search as a Responses server-side tool, but the upstream has no search
// service behind it: v1.5.3 measured it, and handing web_search /
// web_search_preview / web_fetch to the chat endpoint produced exactly the same
// answer as sending no tools at all — zero tool calls. There is nothing to
// forward to, so the gateway runs the tools itself.
//
// v1.5.0..1.5.2 did this and was reverted (issue #43). All three defects are
// fixed here:
//
//  1. Only args["query"] was read. A model that sent the `queries` array got
//     back "you did not ask a question", retried, and burned the whole round
//     budget. -> QueryArgs accepts query / queries / q and joins multiple
//     queries into one search.
//  2. Deduplication only looked at the function-shaped tools already expanded
//     for chat, so the client's original server-side declaration survived and
//     the upstream saw two tools named web_search. -> InstallToolDefs drops
//     every same-named entry first, then installs exactly one definition of
//     ours.
//  3. When the rounds ran out, a synthetic wrap-up response (status=completed,
//     output=[]) passed the failure off as a clean ending, so the client saw a
//     half-finished answer. -> Nothing is synthesised here: the caller
//     withdraws the tools on the last round and lets the model close in prose.
//
// The search backend is DuckDuckGo's HTML endpoint, which needs no API key.
// Every failure comes back as a readable sentence for the model; nothing is
// ever fabricated. Standard library only.
package webtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Names the gateway answers to and installs.
const (
	WebSearchName = "web_search"
	WebFetchName  = "web_fetch"
)

// Declared types a client may use for the same tool. Both list forms are
// matched against an entry's `type` or its `name`, because some clients wrap a
// server-side declaration into function shape.
var (
	SearchDeclTypes = []string{"web_search", "web_search_preview", "web_search_preview_2025_03_11"}
	FetchDeclTypes  = []string{"web_fetch"}
)

// Limits. MAX_RESULTS caps how much of one search page is believed,
// MAXFetchChars is the per-call reading window, HTTPTimeout is the budget for
// one request to the outside world, and defaultRounds is how many web-tool
// rounds the gateway will run for one client turn before handing the turn back.
const (
	MaxResults    = 10
	MAXFetchChars = 100000
	HTTPTimeout   = 20 * time.Second
	maxWebRounds  = 8
	defaultRounds = 3

	// searchEndpoint is DuckDuckGo's keyless HTML result page.
	searchEndpoint = "https://html.duckduckgo.com/html/"

	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
)

// client is shared so connections are reused across searches and fetches.
var client = &http.Client{Timeout: HTTPTimeout}

// Compiled once: these patterns run over every result block and every page.
var (
	reSpace         = regexp.MustCompile(`[ \t\f\v]+`)
	reNewline       = regexp.MustCompile(`\s*\n\s*`)
	reBlankLines    = regexp.MustCompile(`\n{3,}`)
	reTag           = regexp.MustCompile(`(?s)<[^>]+>`)
	reResultBlock   = regexp.MustCompile(`(?is)<div[^>]+class="[^"]*result__body[^"]*"`)
	reResultLink    = regexp.MustCompile(`(?is)<a[^>]+class="[^"]*result__a[^"]*"[^>]*href="([^"]+)"[^>]*>(.*?)</a>`)
	reResultSnippet = regexp.MustCompile(`(?is)class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</a>`)
	rePrivateHost   = regexp.MustCompile(`^(127\.|10\.|192\.168\.|169\.254\.|0\.)`)
	rePrivate172    = regexp.MustCompile(`^172\.(1[6-9]|2\d|3[01])\.`)
	reSourceLine    = regexp.MustCompile(`(?m)^\d+\.\s*(.+?)\s*\n\s*(https?://\S+)\s*$`)
)

// Source is one citation recovered from a formatted search result.
type Source struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// SearchToolDef is the definition installed in place of a client's web_search
// declaration. The description is the one the model is steered by, so it names
// web_fetch as the follow-up for reading a page in full.
func SearchToolDef() map[string]any {
	return map[string]any{
		"type": "function",
		"name": WebSearchName,
		"description": "Searches the web for real-time information and returns ranked results " +
			"with titles, URLs and snippets. Use it for current events, documentation " +
			"lookup, or anything beyond your knowledge cutoff. To read a page in full, " +
			"call web_fetch on its URL afterwards.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "The search query (at least 2 characters).",
				},
				"numResults": map[string]any{
					"type":        "number",
					"description": "How many results to return (1-10, default 5).",
				},
			},
			"required": []any{"query"},
		},
	}
}

// FetchToolDef is the definition installed in place of a client's web_fetch
// declaration.
func FetchToolDef() map[string]any {
	return map[string]any{
		"type":        "function",
		"name":        WebFetchName,
		"description": "Fetches a URL and returns its readable text. Use startIndex to page through a long page.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "Absolute http:// or https:// URL to fetch.",
				},
				"startIndex": map[string]any{
					"type":        "number",
					"description": "Character offset to continue reading a long page.",
				},
			},
			"required": []any{"url"},
		},
	}
}

// ClientWantsWeb reports which web tools the client declared, whether it
// declared them server-side ({"type": "web_search"}) or in function shape.
func ClientWantsWeb(tools []any) (search, fetch bool) {
	search = declaresAny(tools, append(append([]string{}, SearchDeclTypes...), WebSearchName))
	fetch = declaresAny(tools, append(append([]string{}, FetchDeclTypes...), WebFetchName))
	return search, fetch
}

// declaresAny reports whether any entry declares one of the web tools under
// any of types.
func declaresAny(tools []any, types []string) bool {
	for _, raw := range tools {
		entry, ok := asMap(raw)
		if !ok {
			continue
		}
		if identifiesWebTool(entry, types) {
			return true
		}
	}
	return false
}

// InstallToolDefs replaces the client's web-tool declarations with ours.
//
// Every entry that names a web tool is removed first — the server-side
// {"type": "web_search"}, the older preview types, a function the client
// brought itself, and the definition it learned from us on an earlier round —
// and exactly one definition of ours is left behind; otherwise the upstream
// sees two tools called web_search and the model calls the wrong one. Returns
// nil when neither tool is wanted, so the caller can treat "empty" and "nothing
// to install" the same way.
func InstallToolDefs(chatTools []any, wantSearch, wantFetch bool) []any {
	if !wantSearch && !wantFetch {
		return nil
	}

	kept := make([]any, 0, len(chatTools)+2)
	for _, raw := range chatTools {
		entry, ok := asMap(raw)
		if !ok {
			// Not a map means not something we can identify, so it survives.
			kept = append(kept, raw)
			continue
		}
		if identifiesWebTool(entry, SearchDeclTypes) || identifiesWebTool(entry, FetchDeclTypes) {
			continue
		}
		kept = append(kept, raw)
	}

	if wantSearch {
		kept = append(kept, SearchToolDef())
	}
	if wantFetch {
		kept = append(kept, FetchToolDef())
	}
	return kept
}

// IsInternalTool reports whether name is one of the tools this gateway runs
// itself, as opposed to something the upstream actually serves.
func IsInternalTool(name string) bool {
	trimmed := strings.TrimSpace(name)
	return trimmed == WebSearchName || trimmed == WebFetchName
}

// QueryArgs pulls the query string out of tool arguments.
//
// The old version read only args["query"], so a model that sent the `queries`
// array was told it had not asked a question — issue #43 retried that way until
// the rounds ran out. query / queries / q are all accepted, and a list is
// joined with " or " into one search.
func QueryArgs(args map[string]any) string {
	raw, ok := firstPresent(args, "query", "queries", "q")
	if !ok || raw == nil {
		return ""
	}
	if items, ok := raw.([]any); ok {
		parts := make([]string, 0, len(items))
		for _, item := range items {
			if text := elementString(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, " or ")
	}
	return stringValue(raw)
}

// URLArg pulls the URL out of tool arguments, taking the first usable entry
// when the model sent a list.
func URLArg(args map[string]any) string {
	raw, ok := firstPresent(args, "url", "urls")
	if !ok || raw == nil {
		return ""
	}
	if items, ok := raw.([]any); ok {
		for _, item := range items {
			if text := elementString(item); text != "" {
				return text
			}
		}
		return ""
	}
	return stringValue(raw)
}

// Search runs one query against DuckDuckGo's HTML page and returns the block
// fed to the model. numResults is clamped to [1, MaxResults] and defaults to 5
// when unparseable. Any failure comes back as an "Error: ..." sentence.
func Search(query string, numResults int) string {
	query = strings.TrimSpace(query)
	if len([]rune(query)) < 2 {
		return fmt.Sprintf("Error: web_search needs a query of at least 2 characters; "+
			"got %q. Pass it as {\"query\": \"...\"}.", query)
	}
	if numResults <= 0 {
		numResults = 5
	}
	if numResults > MaxResults {
		numResults = MaxResults
	}

	page, failure := httpGet(searchEndpoint + "?" + url.Values{"q": {query}}.Encode())
	if failure != "" {
		return failure
	}

	// The page is sliced at each result container; the first slice is the
	// head of the document, every later one starts inside a result.
	blocks := reResultBlock.Split(page, -1)
	results := make([]Source, 0, numResults)
	snippets := make([]string, 0, numResults)
	for _, block := range blocks[1:] {
		link := reResultLink.FindStringSubmatch(block)
		if link == nil {
			continue
		}
		href := ddgTarget(link[1])
		title := StripTags(link[2])
		snippet := ""
		if m := reResultSnippet.FindStringSubmatch(block); m != nil {
			snippet = StripTags(m[1])
		}
		if href == "" || title == "" {
			continue
		}
		results = append(results, Source{Title: title, URL: href})
		snippets = append(snippets, snippet)
		if len(results) >= numResults {
			break
		}
	}

	if len(results) == 0 {
		return fmt.Sprintf("No results found for: %s\n\nTry a broader or differently worded query.", query)
	}

	lines := make([]string, 0, len(results))
	for i, result := range results {
		lines = append(lines, fmt.Sprintf("%d. %s\n   %s\n   %s",
			i+1, result.Title, result.URL, snippets[i]))
	}
	return fmt.Sprintf("Search results for: %s\n\n%s\n\nCite the sources you used at the end "+
		"of your answer.", query, strings.Join(lines, "\n\n"))
}

// Fetch retrieves one page and returns its readable text plus the window it
// covers. rawURL is guarded first; a rejected URL, an HTTP error and a read
// failure are all reported as "Error: ..." sentences.
func Fetch(rawURL string, startIndex int) string {
	checked, problem := guardURL(rawURL)
	if problem != "" {
		return "Error: " + problem
	}
	page, failure := httpGet(checked)
	if failure != "" {
		return strings.Replace(failure, "the search backend", checked, 1)
	}

	text := StripTags(page)
	start := startIndex
	if start < 0 {
		start = 0
	}
	if start >= len(text) {
		return fmt.Sprintf("Error: startIndex %d is past the end of the page (%d characters total).",
			start, len(text))
	}

	end := start + MAXFetchChars
	if end > len(text) {
		end = len(text)
	}
	head := fmt.Sprintf("URL: %s\nCharacters: %d-%d of %d", checked, start, end, len(text))
	tail := ""
	if end < len(text) {
		tail = fmt.Sprintf("\n\n[Truncated. Call web_fetch again with startIndex=%d to continue.]", end)
	}
	return head + "\n\n" + text[start:end] + tail
}

// SourcesFromResult reads the (title, URL) pairs back out of a formatted
// search result.
//
// The model is fed text, but the client needs structured data to draw citation
// cards, so the pairs are parsed back out of our own format instead of being
// kept in state somewhere.
func SourcesFromResult(result string) []Source {
	out := make([]Source, 0, MaxResults)
	for _, m := range reSourceLine.FindAllStringSubmatch(result, -1) {
		link := strings.TrimRight(strings.TrimSpace(m[2]), ".,;:!?")
		title := strings.TrimSpace(m[1])
		if link == "" || containsSource(out, link) {
			continue
		}
		if title == "" {
			title = link
		}
		out = append(out, Source{Title: title, URL: link})
	}
	return out
}

// Execute runs one internal web tool. It never panics and always returns a
// string that is safe to feed back to the model: argsRaw is accepted as a JSON
// object string or as a decoded map, and anything else is treated as "no
// arguments".
func Execute(name string, argsRaw any) string {
	name = strings.TrimSpace(name)
	args := decodeArgs(argsRaw)

	defer func() {
		// A panic here would take down the request handler mid-stream, so the
		// worst case is reported the same way any other failure is.
		_ = recover()
	}()

	switch name {
	case WebSearchName:
		return Search(QueryArgs(args), intArg(args, "numResults", 5))
	case WebFetchName:
		return Fetch(URLArg(args), intArg(args, "startIndex", 0))
	}
	return fmt.Sprintf("Error: %s is not a tool this gateway runs.", name)
}

// MaxRounds is how many web-tool rounds the gateway will run for one client
// turn. WB_MAX_WEB_ROUNDS overrides it for a quick dial-down; anything above
// the hard cap of 8 is clamped, and an unset or unusable value means 3.
func MaxRounds() int {
	raw := strings.TrimSpace(os.Getenv("WB_MAX_WEB_ROUNDS"))
	if raw == "" {
		return defaultRounds
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultRounds
	}
	if n > maxWebRounds {
		return maxWebRounds
	}
	return n
}

// StripTags turns a fragment of HTML into readable text: script/style/noscript
// bodies are dropped, breaks and block-level closers become newlines, inline
// whitespace and blank runs collapse, entities are unescaped and non-breaking
// spaces become plain ones.
func StripTags(text string) string {
	text = dropElement(text, "script")
	text = dropElement(text, "style")
	text = dropElement(text, "noscript")
	text = dropTag(text, "br", "\n")
	text = dropBlockClose(text, "\n")
	text = reTag.ReplaceAllString(text, " ")
	text = html.UnescapeString(text)
	text = strings.ReplaceAll(text, "\u00a0", " ")
	text = reSpace.ReplaceAllString(text, " ")
	text = reNewline.ReplaceAllString(text, "\n")
	text = reBlankLines.ReplaceAllString(text, "\n\n")
	return strings.TrimSpace(text)
}

// ddgTarget unwraps DuckDuckGo's /l/?uddg= redirect; a href that is not one is
// returned as-is, only unescaped and protocol-completed.
func ddgTarget(href string) string {
	href = strings.TrimSpace(html.UnescapeString(href))
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	parsed, err := url.Parse(href)
	if err != nil {
		return href
	}
	if strings.Contains(parsed.Hostname(), "duckduckgo.com") && strings.HasPrefix(parsed.Path, "/l/") {
		if target := parsed.Query().Get("uddg"); target != "" {
			if unescaped, err := url.QueryUnescape(target); err == nil {
				return unescaped
			}
			return target
		}
	}
	return href
}

// guardURL limits fetching to ordinary public http(s) URLs. It returns the URL
// to use and a human-readable reason when it refuses.
func guardURL(rawURL string) (string, string) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", "Invalid URL."
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", "Only http:// or https:// URLs are supported."
	}
	if parsed.User != nil {
		if _, hasPassword := parsed.User.Password(); parsed.User.Username() != "" || hasPassword {
			return "", "Credentials in the URL are not allowed."
		}
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" || !strings.Contains(host, ".") || strings.HasSuffix(host, ".localhost") {
		return "", "Private, loopback or single-label hosts are not allowed."
	}
	if rePrivateHost.MatchString(host) || rePrivate172.MatchString(host) {
		return "", "Private network addresses are not allowed."
	}
	return rawURL, ""
}

// httpGet performs one request with the browser-ish headers DuckDuckGo's HTML
// page expects. The body is bounded rather than streamed whole; a failure is
// returned as an "Error: ..." sentence for the model.
func httpGet(rawURL string) (string, string) {
	ctx, cancel := context.WithTimeout(context.Background(), HTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Sprintf("Error: could not reach the search backend (%s).", errName(err))
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := client.Do(req)
	if err != nil {
		var status *httpStatusError
		if errors.As(err, &status) {
			return "", fmt.Sprintf("Error: the search backend answered HTTP %d.", status.Code)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return "", fmt.Sprintf("Error: the search backend did not answer within %ds.",
				int(HTTPTimeout.Seconds()))
		}
		return "", fmt.Sprintf("Error: could not reach the search backend (%s).", errName(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return "", fmt.Sprintf("Error: the search backend answered HTTP %d.", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return "", fmt.Sprintf("Error: could not reach the search backend (%s).", errName(err))
	}
	return decodeBody(body, resp.Header.Get("Content-Type")), ""
}

// maxBodyBytes bounds one response read. A 100k-character window is the most
// that is ever shown, so a page far larger than that is not worth holding.
const maxBodyBytes = 4 << 20

// httpStatusError is only produced by tests that substitute a client; the
// transport reports non-2xx through StatusCode instead.
type httpStatusError struct{ Code int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("http %d", e.Code) }

// decodeBody honours the charset advertised in Content-Type, then falls back to
// UTF-8 — the same order the Python version used, with invalid bytes replaced
// rather than aborting the read.
func decodeBody(body []byte, contentType string) string {
	charset := charsetOf(contentType)
	if charset != "" && !strings.EqualFold(charset, "utf-8") && !strings.EqualFold(charset, "utf8") {
		if decoded, ok := decodeCharset(body, charset); ok {
			return decoded
		}
	}
	return strings.ToValidUTF8(string(body), "\ufffd")
}

// charsetOf pulls the charset parameter out of a Content-Type header.
func charsetOf(contentType string) string {
	for _, part := range strings.Split(contentType, ";") {
		key, value, found := strings.Cut(part, "=")
		if !found || !strings.EqualFold(strings.TrimSpace(key), "charset") {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return ""
}

// decodeCharset handles the two legacy charsets still worth knowing about; it
// reports false so the caller can fall back to UTF-8.
func decodeCharset(body []byte, charset string) (string, bool) {
	switch strings.ToLower(charset) {
	case "latin-1", "latin1", "iso-8859-1", "iso8859-1":
		var sb strings.Builder
		sb.Grow(len(body))
		for _, b := range body {
			sb.WriteRune(rune(b))
		}
		return sb.String(), true
	case "ascii", "us-ascii":
		var sb strings.Builder
		sb.Grow(len(body))
		for _, b := range body {
			if b < 0x80 {
				sb.WriteByte(b)
			} else {
				sb.WriteRune('\ufffd')
			}
		}
		return sb.String(), true
	}
	return "", false
}

// decodeArgs accepts the arguments either as the raw JSON string the upstream
// sent or as a map that was already parsed. Anything else means no arguments.
func decodeArgs(argsRaw any) map[string]any {
	var decoded any
	switch raw := argsRaw.(type) {
	case nil:
		return map[string]any{}
	case string:
		if strings.TrimSpace(raw) == "" {
			return map[string]any{}
		}
		if json.Unmarshal([]byte(raw), &decoded) != nil {
			return map[string]any{}
		}
	case []byte:
		if json.Unmarshal(raw, &decoded) != nil {
			return map[string]any{}
		}
	default:
		decoded = raw
	}
	if entry, ok := asMap(decoded); ok {
		return entry
	}
	return map[string]any{}
}

// asMap narrows a decoded JSON value to an object.
func asMap(raw any) (map[string]any, bool) {
	if raw == nil {
		return nil, false
	}
	entry, ok := raw.(map[string]any)
	return entry, ok
}

// identifiesWebTool reports whether one tool entry names a web tool: its `type`
// or its `name` — a server-side declaration carries {"type": "web_search"} and
// carries no name at all, a client that wrapped it into function shape carries
// {"type": "function", "name": "web_search"}. Both fields are therefore tried on
// every entry, and the nested function name is used when the entry has none.
func identifiesWebTool(entry map[string]any, types []string) bool {
	if inList(fieldValue(entry, "type"), types) {
		return true
	}
	name := fieldValue(entry, "name")
	if name == "" {
		if nested, ok := asMap(entry["function"]); ok {
			name = fieldValue(nested, "name")
		}
	}
	return name != "" && inList(name, types)
}

// fieldValue is one entry's field, lowercased and trimmed the way the reference
// compared it, or "" when the field is absent or unusable.
func fieldValue(entry map[string]any, key string) string {
	if entry == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(stringValue(entry[key])))
}

// firstPresent returns the first of keys that the arguments actually carry,
// skipping JSON nulls so a model that sent {"query": null, "queries": [...]}
// still gets searched.
func firstPresent(args map[string]any, keys ...string) (any, bool) {
	for _, key := range keys {
		if value, ok := args[key]; ok && value != nil {
			return value, true
		}
	}
	return nil, false
}

// stringValue renders a decoded JSON value as the trimmed string the Python
// version's str(...).strip() produced.
func stringValue(raw any) string {
	switch value := raw.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(value)
	case json.Number:
		return strings.TrimSpace(value.String())
	case bool, float64, int:
		return strings.TrimSpace(fmt.Sprint(value))
	case map[string]any, []any:
		// str() of a container in Python is its repr; only its presence
		// matters here, so it renders as empty rather than as debugging noise.
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(raw))
}

// elementString renders one entry of a query or url list.
func elementString(raw any) string { return stringValue(raw) }

// inList reports whether value is one of items.
func inList(value string, items []string) bool {
	for _, item := range items {
		if value == item {
			return true
		}
	}
	return false
}

// containsSource reports whether url is already collected.
func containsSource(out []Source, url string) bool {
	for _, source := range out {
		if source.URL == url {
			return true
		}
	}
	return false
}

// dropElement removes whole elements (with their content) for a tag name.
//
// The scan is index-based on purpose: .*? between an opening and a closing tag
// is quadratic on pages with many script tags, and one such regexp can be
// pointed straight at a hostile page.
func dropElement(text, tag string) string {
	open := regexp.MustCompile(`(?is)<` + tag + `[^>]*>`)
	closeTag := regexp.MustCompile(`(?is)</` + tag + `\s*>`)
	var sb strings.Builder
	sb.Grow(len(text))
	at := 0
	for {
		start := open.FindStringIndex(text[at:])
		if start == nil {
			break
		}
		openEnd := at + start[1]
		closeAt := closeTag.FindStringIndex(text[openEnd:])
		if closeAt == nil {
			// Unterminated element: everything after it belongs to it.
			sb.WriteString(text[at : at+start[0]])
			return sb.String()
		}
		sb.WriteString(text[at : at+start[0]])
		sb.WriteByte(' ')
		at = openEnd + closeAt[1]
	}
	sb.WriteString(text[at:])
	return sb.String()
}

// dropTag replaces every tag named tag with replacement.
func dropTag(text, tag, replacement string) string {
	re := regexp.MustCompile(`(?is)<` + tag + `[^>]*>`)
	return re.ReplaceAllString(text, replacement)
}

// dropBlockClose turns the closing tag of every block-level element into a
// newline, so paragraphs, list items, table rows and headings stay separate
// once the tags themselves are gone.
func dropBlockClose(text, replacement string) string {
	re := regexp.MustCompile(`(?is)</(p|div|li|tr|h[1-6])>`)
	return re.ReplaceAllString(text, replacement)
}

// intArg reads an integer argument, falling back to def for a missing, null or
// unparseable value. JSON numbers arrive as float64, which is the case models
// hit by sending 5 rather than "5".
func intArg(args map[string]any, key string, def int) int {
	raw, ok := args[key]
	if !ok || raw == nil {
		return def
	}
	switch value := raw.(type) {
	case float64:
		return int(value)
	case float32:
		return int(value)
	case int:
		return value
	case int64:
		return int(value)
	case json.Number:
		if n, err := value.Int64(); err == nil {
			return int(n)
		}
		if f, err := value.Float64(); err == nil {
			return int(f)
		}
	case string:
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return def
		}
		if n, err := strconv.Atoi(trimmed); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(trimmed, 64); err == nil {
			return int(f)
		}
	}
	return def
}

// errName is the exception type name the Python version reported — here the
// Go error's kind, which is what a reader can act on.
func errName(err error) string {
	if err == nil {
		return "unknown"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		err = urlErr.Err
	}
	message := err.Error()
	if idx := strings.Index(message, ":"); idx > 0 {
		message = message[:idx]
	}
	return strings.TrimSpace(message)
}
