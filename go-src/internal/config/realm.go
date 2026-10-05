// Package config holds the gateway's realm (regional upstream) definitions.
//
// WorkBuddy ships as two independent services with different hosts, different
// User-Agents, and different feature sets: the international build talks to
// workbuddy.ai and has no daily check-in, while the China build talks to
// copilot.tencent.com / codebuddy.cn and does. Routing the wrong UA at the
// wrong host gets the request rejected upstream, so the pair is kept together
// here and never assembled piecemeal at a call site.
//
// Ported from REALM_CONFIGS in the Python reference (wb_accounts.py).
package config

import "strings"

// Realm identifiers, as stored on disk and in the panel.
const (
	RealmIntl = "intl"
	RealmCN   = "cn"
)

// Realm describes one regional upstream.
type Realm struct {
	ID   string
	Name string

	// ChatUpstream serves the model catalog and conversations.
	ChatUpstream string
	// BillingUpstream serves credits, check-in and usage metering. On the
	// international side it happens to equal ChatUpstream; on the China side it
	// does not, and check-in only exists there.
	BillingUpstream string

	Origin string
	Domain string

	// ChatUA and BillingUA differ per realm because upstream fingerprints the
	// client that is expected to be talking to it.
	ChatUA    string
	BillingUA string

	// InfoFilename is the credential file the desktop client writes.
	InfoFilename string
	CacheDirName string

	// HasCheckin gates the whole check-in / travel / growth subsystem.
	HasCheckin bool
}

// Realms is the registry, keyed by realm id.
var Realms = map[string]Realm{
	RealmIntl: {
		ID:              RealmIntl,
		Name:            "国际版 (Global)",
		ChatUpstream:    "https://www.workbuddy.ai",
		BillingUpstream: "https://www.workbuddy.ai",
		Origin:          "https://www.workbuddy.ai",
		Domain:          "www.workbuddy.ai",
		ChatUA:          "WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2",
		BillingUA:       "WorkBuddy/5.5.2",
		InfoFilename:    "workbuddy-desktop-ai.info",
		CacheDirName:    ".workbuddy-ai",
		HasCheckin:      false,
	},
	RealmCN: {
		ID:              RealmCN,
		Name:            "国内版 (China)",
		ChatUpstream:    "https://copilot.tencent.com",
		BillingUpstream: "https://www.codebuddy.cn",
		Origin:          "https://www.codebuddy.cn",
		Domain:          "copilot.tencent.com",
		ChatUA:          "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1",
		BillingUA:       "WorkBuddy/5.5.6",
		InfoFilename:    "workbuddy-desktop.info",
		CacheDirName:    ".workbuddy",
		HasCheckin:      true,
	},
}

// Upstream API paths. These are shared by both realms.
const (
	PathAuthState     = "/v2/plugin/auth/state"
	PathAuthToken     = "/v2/plugin/auth/token"
	PathLoginAccount  = "/v2/plugin/login/account"
	PathTokenRefresh  = "/v2/plugin/auth/token/refresh"
	PathDailyCheckin  = "/v2/billing/meter/daily-checkin"
	PathGetResource   = "/v2/billing/meter/get-user-resource"
	PathClaimComp     = "/v2/billing/meter/claim-compensation"
	PathClaimGift     = "/v2/billing/meter/claim-gift"
	PathIDETrial      = "/billing/ide/trial"
	PathChatComplet   = "/v1/chat/completions"
	PathChatCompletV2 = "/v2/chat/completions"
	PathEnterpriseMod = "/v2/enterprises/personal/models"
)

// Get returns the realm for id, falling back to the international build.
//
// Falling back rather than failing is deliberate: an account file written by an
// older version may carry no realm at all, and the international build is the
// one that has always existed.
func Get(id string) Realm {
	if r, ok := Realms[strings.ToLower(strings.TrimSpace(id))]; ok {
		return r
	}
	return Realms[RealmIntl]
}

// DetectFromDomain picks a realm from a host or issuer string.
func DetectFromDomain(domain string) string {
	d := strings.ToLower(domain)
	if strings.Contains(d, "copilot.tencent.com") || strings.Contains(d, "codebuddy.cn") {
		return RealmCN
	}
	return RealmIntl
}
