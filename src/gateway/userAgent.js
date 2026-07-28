// userAgent — turn a User-Agent string into { device, os, browser }.
//
// The browser sends this header on every request for free (the beacon's
// fetch included). It's a messy, lie-prone string, but enough to label a
// client "macOS · Chrome" or "iOS · Safari" for the users table. Zero deps:
// just ordered regex, most-specific first (order matters — iPad claims to be
// Macintosh, Edge claims to be Chrome, so the narrow cases win).

export function parseUA(ua = "") {
  const s = String(ua);

  // ---- OS ----
  let os = "Unknown";
  if (/iPhone/i.test(s)) os = "iOS";
  else if (/iPad/i.test(s)) os = "iPadOS";
  else if (/Android/i.test(s)) os = "Android";
  else if (/Mac OS X|Macintosh/i.test(s)) os = "macOS";
  else if (/Windows NT/i.test(s)) os = "Windows";
  else if (/CrOS/i.test(s)) os = "ChromeOS";
  else if (/Linux/i.test(s)) os = "Linux";

  // ---- browser (order: narrow impostors before the thing they impersonate) ----
  let browser = "Unknown";
  if (/Edg\//i.test(s)) browser = "Edge";
  else if (/OPR\/|Opera/i.test(s)) browser = "Opera";
  else if (/Firefox\//i.test(s)) browser = "Firefox";
  else if (/Chrome\//i.test(s)) browser = "Chrome";
  else if (/Safari\//i.test(s)) browser = "Safari";
  else if (/curl\//i.test(s)) browser = "curl";
  else if (/node|axios|python-requests|Go-http|Java\//i.test(s)) browser = "script";
  else if (/bot|crawler|spider|slurp/i.test(s)) browser = "bot";

  // ---- device class ----
  let device = "Desktop";
  if (/Mobile|iPhone|Android.*Mobile/i.test(s)) device = "Mobile";
  else if (/iPad|Tablet/i.test(s)) device = "Tablet";
  else if (browser === "curl" || browser === "script" || browser === "bot") device = "Machine";

  return { device, os, browser, raw: s.slice(0, 160) };
}
