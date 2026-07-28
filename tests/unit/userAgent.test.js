import { test } from "node:test";
import assert from "node:assert/strict";
import { parseUA } from "../../src/gateway/userAgent.js";

test("real device + OS + browser from common user agents", () => {
  const mac = parseUA("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36");
  assert.deepEqual([mac.os, mac.browser, mac.device], ["macOS", "Chrome", "Desktop"]);

  const iphone = parseUA("Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1 Version/17.5 Mobile/15E148 Safari/604.1");
  assert.deepEqual([iphone.os, iphone.browser, iphone.device], ["iOS", "Safari", "Mobile"]);

  const win = parseUA("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/126.0 Safari/537.36 Edg/126.0");
  assert.deepEqual([win.os, win.browser], ["Windows", "Edge"]); // Edge before Chrome
});

test("machines are labeled as machines, not phantom desktops", () => {
  assert.equal(parseUA("curl/8.4.0").device, "Machine");
  assert.equal(parseUA("python-requests/2.31").browser, "script");
  assert.equal(parseUA("Googlebot/2.1 (+http://www.google.com/bot.html)").device, "Machine");
});

test("empty or junk UA never throws", () => {
  assert.equal(parseUA("").os, "Unknown");
  assert.equal(parseUA(undefined).browser, "Unknown");
});
