// JWT + role-based access, from scratch.
//
// A JWT is just three base64url strings glued with dots:
//   header.payload.signature
// header  = {"alg":"HS256","typ":"JWT"}       (how it's signed)
// payload = {"sub":"...","role":"admin","exp":123}  (the claims)
// signature = HMAC-SHA256(header + "." + payload, secret)
//
// The server hands one out at login and then TRUSTS ITS OWN SIGNATURE
// instead of a session database: if the signature checks out, the claims
// inside (who you are, what ROLE you have, when it expires) must be exactly
// what we signed — any tampering breaks the HMAC. That's the whole trick,
// and it's why this file has zero dependencies: it's one HMAC, same as the
// Stripe webhook check.
//
// Roles here: "admin" can do everything (billing simulate, all admin data);
// "viewer" is read-only (dashboard data, no mutations).

import { createHmac, timingSafeEqual } from "node:crypto";

const b64url = (buf) => Buffer.from(buf).toString("base64url");
const fromB64url = (s) => Buffer.from(s, "base64url").toString();

export function sign(payload, secret, { expiresInSec = 7200, now = () => Date.now() } = {}) {
  const header = b64url(JSON.stringify({ alg: "HS256", typ: "JWT" }));
  const body = b64url(
    JSON.stringify({ ...payload, iat: Math.floor(now() / 1000), exp: Math.floor(now() / 1000) + expiresInSec })
  );
  const sig = createHmac("sha256", secret).update(`${header}.${body}`).digest("base64url");
  return `${header}.${body}.${sig}`;
}

export function verify(token, secret, { now = () => Date.now() } = {}) {
  const parts = String(token ?? "").split(".");
  if (parts.length !== 3) return { valid: false, why: "malformed" };

  const [header, body, sig] = parts;
  const expected = createHmac("sha256", secret).update(`${header}.${body}`).digest("base64url");
  const a = Buffer.from(expected);
  const b = Buffer.from(sig);
  if (a.length !== b.length || !timingSafeEqual(a, b)) {
    return { valid: false, why: "bad_signature" }; // tampered or wrong secret
  }

  let payload;
  try {
    payload = JSON.parse(fromB64url(body));
  } catch {
    return { valid: false, why: "malformed" };
  }
  if (payload.exp && payload.exp <= Math.floor(now() / 1000)) {
    return { valid: false, why: "expired" }; // a stolen old token is useless
  }
  return { valid: true, payload };
}

// createAuth — the login + gate pair a server actually uses.
// users: { "email": { password, role } }   (demo store; hash passwords in prod)
export function createAuth({ users, secret, expiresInSec = 7200, now = () => Date.now() } = {}) {
  if (!secret) throw new Error("createAuth needs a secret"); // no silent insecurity
  if (!users || Object.keys(users).length === 0) throw new Error("createAuth needs users");

  // Exchange credentials for a signed token. Same vague error for wrong
  // email vs wrong password — don't help attackers enumerate accounts.
  function login(email, password) {
    const user = users[email];
    if (!user || user.password !== password) return null;
    return {
      token: sign({ sub: email, role: user.role }, secret, { expiresInSec, now }),
      role: user.role,
      expiresInSec,
    };
  }

  // The gate: reads "Authorization: Bearer <jwt>", verifies, checks role.
  // Returns the claims when allowed, null when not — caller sends the 401/403.
  function guard(req, allowedRoles) {
    const header = req.headers?.authorization ?? "";
    const token = header.startsWith("Bearer ") ? header.slice(7) : null;
    const result = verify(token, secret, { now });
    if (!result.valid) return null;
    if (allowedRoles && !allowedRoles.includes(result.payload.role)) return null; // wrong role
    return result.payload;
  }

  return { login, guard };
}
