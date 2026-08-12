// Package useragent turns a User-Agent string into {device, os, browser}.
//
// The browser sends this header on every request for free (the beacon's fetch
// included). It is a messy, lie-prone string, but enough to label a client
// "macOS · Chrome" or "iOS · Safari" in the users table.
//
// Ordered matching, most specific first — the order is the whole trick, because
// an iPad claims to be a Macintosh and Edge claims to be Chrome, so the narrow
// impostor must be tested before the thing it impersonates.
package useragent

import "strings"

type Parsed struct {
	Device  string `json:"device"`
	OS      string `json:"os"`
	Browser string `json:"browser"`
	Raw     string `json:"raw"`
}

func contains(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

func Parse(ua string) Parsed {
	s := strings.ToLower(ua)

	// ---- OS ----
	os := "Unknown"
	switch {
	case contains(s, "iphone"):
		os = "iOS"
	case contains(s, "ipad"):
		os = "iPadOS"
	case contains(s, "android"):
		os = "Android"
	case contains(s, "mac os x", "macintosh"):
		os = "macOS"
	case contains(s, "windows nt"):
		os = "Windows"
	// "cros" as a bare substring also matches "microsoft" — real UA strings such
	// as Microsoft-WebDAV-MiniRedir were being reported as ChromeOS. Chrome OS
	// always spells it "CrOS " followed by the architecture, so require the
	// token boundary.
	case contains(s, "cros "), strings.HasSuffix(s, "cros"), contains(s, "cros;"), contains(s, "cros)"):
		os = "ChromeOS"
	case contains(s, "linux"):
		os = "Linux"
	}

	// ---- browser (narrow impostors before the thing they impersonate) ----
	browser := "Unknown"
	switch {
	case contains(s, "edg/"):
		browser = "Edge"
	case contains(s, "opr/", "opera"):
		browser = "Opera"
	case contains(s, "firefox/"):
		browser = "Firefox"
	case contains(s, "chrome/"):
		browser = "Chrome"
	case contains(s, "safari/"):
		browser = "Safari"
	case contains(s, "curl/"):
		browser = "curl"
	case contains(s, "node", "axios", "python-requests", "go-http", "java/"):
		browser = "script"
	case contains(s, "bot", "crawler", "spider", "slurp"):
		browser = "bot"
	}

	// ---- device class ----
	device := "Desktop"
	switch {
	case contains(s, "mobile", "iphone"):
		device = "Mobile"
	case contains(s, "ipad", "tablet"):
		device = "Tablet"
	case browser == "curl" || browser == "script" || browser == "bot":
		device = "Machine"
	}

	raw := ua
	if len(raw) > 160 {
		raw = raw[:160]
	}
	return Parsed{Device: device, OS: os, Browser: browser, Raw: raw}
}
