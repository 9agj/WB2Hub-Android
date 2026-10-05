// Package ipintel answers "where does this exit live, and what kind of address
// is it" — the geolocation half of the proxy-slot feature.
//
// Ported from wb_ipintel.py. The gateway only ever learns an exit's IP address
// (fetched through the proxy itself); the country and the address kind come
// from a public lookup. ip-api.com answers without a key and, with lang=zh-CN,
// returns the country name the panel shows.
//
// `hosting` is the signal behind the residential/datacenter split — an address
// announced by a hosting provider is treated as a machine room, everything else
// as residential. That is a heuristic, not a registry fact, so the announcing
// network is stored alongside it and shown to the operator.
//
// A lookup that fails is reported as unknown rather than raised: a proxy that
// carries traffic is still usable when the lookup is blocked or rate limited.
package ipintel

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Address kinds. Empty means "not known", never "assume something".
const (
	Residential = "residential"
	Datacenter  = "datacenter"
)

// TypeLabels maps a stored kind to the Chinese label the panel shows.
var TypeLabels = map[string]string{
	Residential: "住宅",
	Datacenter:  "机房",
}

// geoEndpoint requests `message` too, so a refusal ("private range", "reserved
// range", rate limit) is visible in the payload instead of surfacing as a bare
// failure.
const geoEndpoint = "http://ip-api.com/json/%s?lang=zh-CN" +
	"&fields=status,message,country,countryCode,isp,org,as,hosting,proxy,mobile"

// LookupTimeout is the default budget for one geolocation query.
const LookupTimeout = 8 * time.Second

// Intel is what a slot stores about its exit. Every field may be empty, which
// means "not known" and never "assume something".
type Intel struct {
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
	IPType      string `json:"ip_type"`
	ISP         string `json:"isp"`
	ASN         string `json:"asn"`
}

// TypeLabel returns the Chinese label for a stored kind, or "" when unknown.
func TypeLabel(ipType string) string {
	return TypeLabels[strings.ToLower(strings.TrimSpace(ipType))]
}

// Empty is the unknown-exit answer, with the same shape as a successful lookup.
func Empty() Intel { return Intel{} }

// apiReply mirrors the subset of the ip-api.com response we consume.
type apiReply struct {
	Status      string `json:"status"`
	Message     string `json:"message"`
	Country     string `json:"country"`
	CountryCode string `json:"countryCode"`
	ISP         string `json:"isp"`
	Org         string `json:"org"`
	AS          string `json:"as"`
	// Hosting is a pointer so a missing field is distinguishable from false.
	Hosting *bool `json:"hosting"`
}

// Classify turns one ip-api.com reply into the fields a slot stores.
//
// Anything unusable — a failed status, a missing body, a `hosting` flag that is
// neither true nor false — comes back empty rather than guessed.
func Classify(body []byte) Intel {
	var reply apiReply
	if err := json.Unmarshal(body, &reply); err != nil {
		return Empty()
	}
	if reply.Status != "success" {
		return Empty()
	}

	var ipType string
	if reply.Hosting != nil {
		if *reply.Hosting {
			ipType = Datacenter
		} else {
			ipType = Residential
		}
	}

	isp := reply.ISP
	if isp == "" {
		isp = reply.Org
	}
	return Intel{
		Country:     strings.TrimSpace(reply.Country),
		CountryCode: strings.ToUpper(strings.TrimSpace(reply.CountryCode)),
		IPType:      ipType,
		ISP:         strings.TrimSpace(isp),
		ASN:         strings.TrimSpace(reply.AS),
	}
}

// Lookup returns the country and address kind for one exit IP; empty fields
// when unknown.
//
// The request goes out directly rather than through the proxy being described:
// the lookup has to work even when that proxy only reaches the upstream, and it
// must not be answered by the exit we are asking about.
func Lookup(client *http.Client, ip string, timeout time.Duration) Intel {
	address := strings.TrimSpace(ip)
	if address == "" {
		return Empty()
	}
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	if timeout <= 0 {
		timeout = LookupTimeout
	}

	req, err := http.NewRequest(http.MethodGet,
		strings.Replace(geoEndpoint, "%s", url.QueryEscape(address), 1), nil)
	if err != nil {
		return Empty()
	}
	req.Header.Set("User-Agent", "wb-proxy")

	// The client may carry its own timeout; bound this call regardless.
	type result struct {
		body []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			done <- result{nil, err}
			return
		}
		defer resp.Body.Close()
		buf := make([]byte, 0, 2048)
		tmp := make([]byte, 1024)
		for {
			n, err := resp.Body.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
				if len(buf) > 64*1024 {
					break
				}
			}
			if err != nil {
				break
			}
		}
		done <- result{buf, nil}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			return Empty()
		}
		return Classify(r.body)
	case <-time.After(timeout):
		return Empty()
	}
}

// SlotName is the default slot name: the exit's country and kind, e.g.
// "美国 住宅".
//
// No country means no name. "住宅" on its own would not tell two slots apart, so
// the caller is better off falling back to the slot id.
func SlotName(country, ipType string) string {
	where := strings.TrimSpace(country)
	if where == "" {
		return ""
	}
	if kind := TypeLabel(ipType); kind != "" {
		return where + " " + kind
	}
	return where
}
