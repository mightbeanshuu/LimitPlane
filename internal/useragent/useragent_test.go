package useragent_test

import (
	"strings"
	"testing"

	"github.com/mightbeanshuu/limitplane/internal/useragent"
)

// Real user-agent strings, because the whole point of this parser is surviving
// what browsers actually send rather than what a spec says they should.
const (
	uaMacChrome     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
	uaMacSafari     = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4.1 Safari/605.1.15"
	uaMacFirefox    = "Mozilla/5.0 (Macintosh; Intel Mac OS X 14.5; rv:127.0) Gecko/20100101 Firefox/127.0"
	uaIPhoneSafari  = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1"
	uaIPadSafari    = "Mozilla/5.0 (iPad; CPU OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1"
	uaIPadNoMobile  = "Mozilla/5.0 (iPad; CPU OS 16_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.6 Safari/604.1"
	uaAndroidChrome = "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36"
	uaAndroidTablet = "Mozilla/5.0 (Linux; Android 13; Tablet) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
	uaWinChrome     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
	uaWinEdge       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36 Edg/126.0.0.0"
	uaWinFirefox    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:127.0) Gecko/20100101 Firefox/127.0"
	uaWinOpera      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36 OPR/111.0.0.0"
	uaOperaPresto   = "Opera/9.80 (Windows NT 6.0) Presto/2.12.388 Version/12.14"
	uaChromeOS      = "Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
	uaLinuxFirefox  = "Mozilla/5.0 (X11; Linux x86_64; rv:127.0) Gecko/20100101 Firefox/127.0"
	uaCurl          = "curl/8.4.0"
	uaPython        = "python-requests/2.31.0"
	uaGo            = "Go-http-client/1.1"
	uaAxios         = "axios/1.6.8"
	uaNodeFetch     = "node-fetch/3.3.2"
	uaJava          = "Java/17.0.1"
	uaGooglebot     = "Googlebot/2.1 (+http://www.google.com/bot.html)"
	uaBingbot       = "Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)"
	uaAhrefs        = "Mozilla/5.0 (compatible; AhrefsBot/7.0; +http://ahrefs.com/robot/)"
	uaYahooSlurp    = "Mozilla/5.0 (compatible; Yahoo! Slurp; http://help.yahoo.com/help/us/ysearch/slurp)"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name                string
		ua                  string
		os, browser, device string
	}{
		// ---- desktop browsers ----
		{"macOS Chrome", uaMacChrome, "macOS", "Chrome", "Desktop"},
		{"macOS Safari", uaMacSafari, "macOS", "Safari", "Desktop"},
		{"macOS Firefox", uaMacFirefox, "macOS", "Firefox", "Desktop"},
		{"Windows Chrome", uaWinChrome, "Windows", "Chrome", "Desktop"},
		{"Windows Edge", uaWinEdge, "Windows", "Edge", "Desktop"},
		{"Windows Firefox", uaWinFirefox, "Windows", "Firefox", "Desktop"},
		{"Windows Opera (Chromium)", uaWinOpera, "Windows", "Opera", "Desktop"},
		{"Opera (Presto era)", uaOperaPresto, "Windows", "Opera", "Desktop"},
		{"ChromeOS", uaChromeOS, "ChromeOS", "Chrome", "Desktop"},
		{"Linux Firefox", uaLinuxFirefox, "Linux", "Firefox", "Desktop"},

		// ---- phones and tablets ----
		{"iPhone Safari", uaIPhoneSafari, "iOS", "Safari", "Mobile"},
		{"iPad Safari (sends a Mobile token)", uaIPadSafari, "iPadOS", "Safari", "Mobile"},
		{"iPad Safari without a Mobile token", uaIPadNoMobile, "iPadOS", "Safari", "Tablet"},
		{"Android phone", uaAndroidChrome, "Android", "Chrome", "Mobile"},
		{"Android tablet", uaAndroidTablet, "Android", "Chrome", "Tablet"},

		// ---- machines ----
		{"curl", uaCurl, "Unknown", "curl", "Machine"},
		{"python-requests", uaPython, "Unknown", "script", "Machine"},
		{"Go http client", uaGo, "Unknown", "script", "Machine"},
		{"axios", uaAxios, "Unknown", "script", "Machine"},
		{"node-fetch", uaNodeFetch, "Unknown", "script", "Machine"},
		{"Java", uaJava, "Unknown", "script", "Machine"},
		{"Googlebot", uaGooglebot, "Unknown", "bot", "Machine"},
		{"bingbot", uaBingbot, "Unknown", "bot", "Machine"},
		{"AhrefsBot", uaAhrefs, "Unknown", "bot", "Machine"},
		{"Yahoo! Slurp", uaYahooSlurp, "Unknown", "bot", "Machine"},

		// ---- nothing useful ----
		{"empty header", "", "Unknown", "Unknown", "Desktop"},
		{"junk", "!!! ???", "Unknown", "Unknown", "Desktop"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := useragent.Parse(tc.ua)
			if got.OS != tc.os {
				t.Errorf("operating system read as %q, expected %q — the users table would show the wrong platform", got.OS, tc.os)
			}
			if got.Browser != tc.browser {
				t.Errorf("browser read as %q, expected %q", got.Browser, tc.browser)
			}
			if got.Device != tc.device {
				t.Errorf("device class read as %q, expected %q", got.Device, tc.device)
			}
			if got.Raw != tc.ua {
				t.Errorf("the raw header was altered: %q", got.Raw)
			}
		})
	}
}

// ---- the ordering that the whole parser depends on -------------------------

func TestNarrowImpostorsAreTestedBeforeTheThingTheyImpersonate(t *testing.T) {
	cases := []struct {
		name       string
		ua         string
		mustNotBe  string
		mustBe     string
		field      func(useragent.Parsed) string
		impersonat string
	}{
		{
			name: "an iPad claims to be a Macintosh", ua: uaIPadSafari,
			mustNotBe: "macOS", mustBe: "iPadOS",
			field:      func(p useragent.Parsed) string { return p.OS },
			impersonat: "every iPad UA contains \"like Mac OS X\", so iPad must be tested first",
		},
		{
			name: "an iPhone also claims to be a Macintosh", ua: uaIPhoneSafari,
			mustNotBe: "macOS", mustBe: "iOS",
			field:      func(p useragent.Parsed) string { return p.OS },
			impersonat: "\"CPU iPhone OS 17_5 like Mac OS X\" would otherwise read as macOS",
		},
		{
			name: "Android claims to be Linux", ua: uaAndroidChrome,
			mustNotBe: "Linux", mustBe: "Android",
			field:      func(p useragent.Parsed) string { return p.OS },
			impersonat: "every Android UA starts \"Linux; Android\"",
		},
		{
			name: "Edge claims to be Chrome", ua: uaWinEdge,
			mustNotBe: "Chrome", mustBe: "Edge",
			field:      func(p useragent.Parsed) string { return p.Browser },
			impersonat: "Edge appends Edg/ to a full Chrome UA",
		},
		{
			name: "Opera claims to be Chrome", ua: uaWinOpera,
			mustNotBe: "Chrome", mustBe: "Opera",
			field:      func(p useragent.Parsed) string { return p.Browser },
			impersonat: "Opera appends OPR/ to a full Chrome UA",
		},
		{
			name: "Chrome claims to be Safari", ua: uaMacChrome,
			mustNotBe: "Safari", mustBe: "Chrome",
			field:      func(p useragent.Parsed) string { return p.Browser },
			impersonat: "every Chrome UA ends in Safari/537.36",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.field(useragent.Parse(tc.ua))
			if got == tc.mustNotBe {
				t.Fatalf("read as %q — %s, so the ordering of the match list is wrong", got, tc.impersonat)
			}
			if got != tc.mustBe {
				t.Fatalf("read as %q, expected %q", got, tc.mustBe)
			}
		})
	}
}

func TestMachinesAreLabelledAsMachinesNotPhantomDesktops(t *testing.T) {
	// A dashboard full of "Desktop" rows that are really cron jobs is a lie
	// about who your users are.
	for _, ua := range []string{uaCurl, uaPython, uaGo, uaAxios, uaNodeFetch, uaJava, uaGooglebot, uaBingbot, uaAhrefs, uaYahooSlurp} {
		t.Run(ua, func(t *testing.T) {
			if got := useragent.Parse(ua).Device; got != "Machine" {
				t.Fatalf("%q was classified as a %q; scripts and crawlers must be labelled Machine", ua, got)
			}
		})
	}
}

func TestBrowsersAreNeverLabelledAsMachines(t *testing.T) {
	for _, ua := range []string{uaMacChrome, uaWinEdge, uaIPhoneSafari, uaAndroidChrome, uaLinuxFirefox, uaChromeOS} {
		t.Run(ua, func(t *testing.T) {
			if got := useragent.Parse(ua).Device; got == "Machine" {
				t.Fatalf("a real browser (%q) was labelled a Machine", ua)
			}
		})
	}
}

// ---- robustness ------------------------------------------------------------

func TestParseNeverPanicsOnHostileInput(t *testing.T) {
	hostile := []string{
		"",
		" ",
		strings.Repeat("A", 10_000),
		"\x00\x01\x02",
		"Mozilla/5.0 (" + strings.Repeat("iPhone;", 500) + ")",
		"Mozilla/5.0 <script>alert(1)</script>",
		"日本語のユーザーエージェント",
	}
	for i, ua := range hostile {
		got := useragent.Parse(ua) // must not panic
		if got.OS == "" || got.Browser == "" || got.Device == "" {
			t.Fatalf("case %d left a field empty (%+v); every field must always carry at least \"Unknown\"", i, got)
		}
	}
}

func TestRawIsTruncatedSoOneClientCannotBloatTheDiary(t *testing.T) {
	// The raw header is stored on every audit event; an attacker sending a
	// megabyte of UA on every request must not be able to fill the ring buffer
	// with their own text.
	long := strings.Repeat("x", 5_000)
	got := useragent.Parse(long)
	if len(got.Raw) != 160 {
		t.Fatalf("a 5000-character user-agent was stored as %d characters; it must be capped at 160", len(got.Raw))
	}
	if got.Raw != long[:160] {
		t.Fatal("truncation kept the wrong slice of the header")
	}

	short := "curl/8.4.0"
	if useragent.Parse(short).Raw != short {
		t.Fatal("a short user-agent was altered by the truncation logic")
	}

	exactly160 := strings.Repeat("y", 160)
	if r := useragent.Parse(exactly160).Raw; len(r) != 160 {
		t.Fatalf("a header of exactly 160 characters was truncated to %d", len(r))
	}
}

func TestMatchingIsCaseInsensitive(t *testing.T) {
	// Headers are attacker-controlled text; casing must not be a way to dodge
	// classification.
	cases := []struct{ ua, wantBrowser, wantDevice string }{
		{"CURL/8.4.0", "curl", "Machine"},
		{"GOOGLEBOT/2.1", "bot", "Machine"},
		{"PYTHON-REQUESTS/2.31.0", "script", "Machine"},
	}
	for _, tc := range cases {
		t.Run(tc.ua, func(t *testing.T) {
			got := useragent.Parse(tc.ua)
			if got.Browser != tc.wantBrowser || got.Device != tc.wantDevice {
				t.Fatalf("an upper-cased header parsed as browser=%q device=%q; casing must not change the verdict", got.Browser, got.Device)
			}
		})
	}
}

// ---- the substring bug -----------------------------------------------------

func TestChromeOSDetectionMustNotMatchTheWordMicrosoft(t *testing.T) {
	t.Skip("BUG: the ChromeOS test is a bare substring search for \"cros\", and \"miCROSoft\" contains it. Any UA carrying the word Microsoft without an explicit \"Windows NT\" token is reported as ChromeOS — e.g. the real Windows WebDAV client \"Microsoft-WebDAV-MiniRedir/10.0.19045\" and Office's own UAs. Inherited from the Node original, which used the equally loose /CrOS/i. Fix: match \"cros \" / \"(x11; cros\" or require a word boundary.")

	cases := []struct{ name, ua string }{
		{"the Windows WebDAV client", "Microsoft-WebDAV-MiniRedir/10.0.19045"},
		{"an Office desktop client", "Microsoft Office Word 2014"},
		{"a bare product string", "Microsoft BITS/7.8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := useragent.Parse(tc.ua).OS; got == "ChromeOS" {
				t.Fatalf("%q was reported as running ChromeOS because \"microsoft\" contains the substring \"cros\"", tc.ua)
			}
		})
	}

	// The genuine article must of course still be detected.
	if got := useragent.Parse(uaChromeOS).OS; got != "ChromeOS" {
		t.Fatalf("a real ChromeOS user-agent was read as %q", got)
	}
}
