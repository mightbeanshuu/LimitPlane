import { test } from "node:test";
import assert from "node:assert/strict";
import { sign, verify, createAuth } from "../../src/auth/jwt.js";

const SECRET = "test-secret";
const T0 = Date.UTC(2026, 5, 10);

test("round trip: what you sign is what you get back", () => {
  const token = sign({ sub: "anshu", role: "admin" }, SECRET, { now: () => T0 });
  const r = verify(token, SECRET, { now: () => T0 });
  assert.equal(r.valid, true);
  assert.equal(r.payload.sub, "anshu");
  assert.equal(r.payload.role, "admin");
});

test("tampering with the payload breaks the signature", () => {
  const token = sign({ sub: "viewer-user", role: "viewer" }, SECRET, { now: () => T0 });
  const [h, body, sig] = token.split(".");
  // forge: viewer promotes themselves to admin in the payload...
  const forged = Buffer.from(JSON.stringify({ sub: "viewer-user", role: "admin", exp: 9999999999 })).toString("base64url");
  const r = verify(`${h}.${forged}.${sig}`, SECRET, { now: () => T0 });
  assert.equal(r.valid, false); // ...and the HMAC says no
  assert.equal(r.why, "bad_signature");
});

test("wrong secret, malformed junk, and expiry are all rejected", () => {
  const token = sign({ sub: "x" }, SECRET, { expiresInSec: 100, now: () => T0 });
  assert.equal(verify(token, "other-secret", { now: () => T0 }).valid, false);
  assert.equal(verify("not-a-jwt", SECRET).valid, false);
  assert.equal(verify(undefined, SECRET).valid, false);

  const later = () => T0 + 101_000; // one second past exp
  const r = verify(token, SECRET, { now: later });
  assert.equal(r.valid, false);
  assert.equal(r.why, "expired");
});

test("createAuth: login issues role tokens, guard enforces them", () => {
  const auth = createAuth({
    secret: SECRET,
    users: {
      "admin@x.dev": { password: "a-pass", role: "admin" },
      "view@x.dev": { password: "v-pass", role: "viewer" },
    },
    now: () => T0,
  });

  assert.equal(auth.login("admin@x.dev", "WRONG"), null); // bad password
  assert.equal(auth.login("ghost@x.dev", "a-pass"), null); // unknown user

  const admin = auth.login("admin@x.dev", "a-pass");
  const viewer = auth.login("view@x.dev", "v-pass");
  const reqWith = (t) => ({ headers: { authorization: `Bearer ${t}` } });

  // viewer can read, cannot pass an admin-only gate
  assert.equal(auth.guard(reqWith(viewer.token), ["admin", "viewer"]).role, "viewer");
  assert.equal(auth.guard(reqWith(viewer.token), ["admin"]), null);
  // admin passes both
  assert.equal(auth.guard(reqWith(admin.token), ["admin"]).role, "admin");
  // no header at all
  assert.equal(auth.guard({ headers: {} }, ["admin", "viewer"]), null);
});
