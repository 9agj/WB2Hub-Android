package server

// Localised labels for the panel.
//
// The Android UI renders Chinese, so the API hands back display-ready names
// rather than making the client carry a translation table it would have to keep
// in sync with the server. Identifiers stay ASCII: only the human-facing label
// is translated.
var (
	// realmNames maps a realm id to its display name.
	realmNames = map[string]string{
		"intl":     "国际版",
		"cn":       "国内版",
		"codearts": "华为 CodeArts",
	}

	// coolKindNames maps a cooldown reason to its display name.
	coolKindNames = map[string]string{
		"rate_limit":   "限流中",
		"credits":      "额度用尽",
		"error":        "连续报错",
		"session_dead": "会话失效",
		"manual":       "手动停用",
		"":             "正常",
	}

	// ipTypeNames maps an exit address kind to its display name.
	ipTypeNames = map[string]string{
		"residential": "住宅",
		"datacenter":  "机房",
		"":            "未知",
	}
)

// realmLabel returns the display name for a realm id.
func realmLabel(id string) string {
	if name, ok := realmNames[id]; ok {
		return name
	}
	return id
}

// coolLabel returns the display name for a cooldown reason.
func coolLabel(kind string) string {
	if name, ok := coolKindNames[kind]; ok {
		return name
	}
	return kind
}

// ipTypeLabel returns the display name for an exit kind.
func ipTypeLabel(kind string) string {
	if name, ok := ipTypeNames[kind]; ok {
		return name
	}
	return kind
}
