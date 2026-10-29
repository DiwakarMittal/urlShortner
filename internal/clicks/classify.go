package clicks

import (
	"net/url"
	"strings"
)

const (
	maxReferrerLen = 100       // referrer hosts are attacker-controlled: cap them
	otherReferrer  = "(other)" // where overflow referrers are folded
)

// ReferrerHost reduces a Referer header to just the host ("google.com").
// The full URL can contain private paths and tokens, so it is never kept.
// Anything that isn't an http(s) URL with a sane host counts as direct ("").
func ReferrerHost(referer string) string {
	u, err := url.Parse(strings.TrimSpace(referer))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if host == "" || len(host) > maxReferrerLen {
		return ""
	}
	return host
}

var (
	botMarkers    = []string{"bot", "crawler", "spider", "slurp", "curl/", "wget/", "python-requests", "go-http-client", "hey/", "headless"}
	mobileMarkers = []string{"mobi", "android", "iphone", "ipad"}
)

// Device reduces a User-Agent to a coarse class: bot, mobile, desktop or
// other. The raw string is never stored (it is unique enough to track a person).
func Device(userAgent string) string {
	ua := strings.ToLower(userAgent)
	switch {
	case ua == "":
		return "other"
	case containsAny(ua, botMarkers):
		return "bot"
	case containsAny(ua, mobileMarkers):
		return "mobile"
	case strings.Contains(ua, "mozilla"):
		return "desktop"
	default:
		return "other"
	}
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
