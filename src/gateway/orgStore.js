// OrgStore — the platform's directory: organizations, their people, their sites.
//
// This is the Vercel-shaped hierarchy:
//
//   PLATFORM (LimitPlane staff = superadmin, sees everything)
//     └── Organization  ("anshu-labs")
//           ├── members: email -> role   (owner / admin / viewer)
//           └── sites:   tenantIds       (each connected site belongs to ONE org)
//
// One JSON file on disk, loaded at boot, written on every change — the same
// honest persistence as the tenant store. A real deployment swaps this for a
// database; every method below maps 1:1 to a table you'd create there.
//
// Passwords are stored as sha256(salt + password) — never plaintext. (Demo
// grade: real systems use scrypt/argon2 with per-user salts and work factors.)

import { createHash, randomBytes } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";

const hash = (salt, password) => createHash("sha256").update(salt + password).digest("hex");

export class OrgStore {
  constructor({ file } = {}) {
    this.file = file;
    this.data = { orgs: {}, users: {} };
    if (file) {
      try {
        this.data = JSON.parse(readFileSync(file, "utf8")); // reload saved world
      } catch {
        /* first boot — empty world */
      }
    }
  }

  save() {
    if (this.file) writeFileSync(this.file, JSON.stringify(this.data, null, 2));
  }

  // ---- users ------------------------------------------------------------------
  createUser(email, password, { platformRole = "user" } = {}) {
    if (this.data.users[email]) return this.data.users[email];
    const salt = randomBytes(8).toString("hex");
    this.data.users[email] = { salt, hash: hash(salt, password), platformRole };
    this.save();
    return this.data.users[email];
  }

  verifyUser(email, password) {
    const u = this.data.users[email];
    if (!u || u.hash !== hash(u.salt, password)) return null; // same silence for both failures
    return { email, platformRole: u.platformRole };
  }

  // ---- orgs -------------------------------------------------------------------
  createOrg(name, ownerEmail) {
    const id = "org_" + name.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "");
    if (this.data.orgs[id]) return null; // names must be unique enough to slug
    this.data.orgs[id] = { id, name, members: { [ownerEmail]: "owner" }, sites: [] };
    this.save();
    return this.data.orgs[id];
  }

  addMember(orgId, email, role) {
    const org = this.data.orgs[orgId];
    if (!org || !["owner", "admin", "viewer"].includes(role)) return null;
    org.members[email] = role;
    this.save();
    return org;
  }

  removeMember(orgId, email) {
    const org = this.data.orgs[orgId];
    if (!org || !(email in org.members)) return false;
    // Never delete the last owner — an org nobody can manage is a dead org.
    if (org.members[email] === "owner" && Object.values(org.members).filter((r) => r === "owner").length === 1) {
      return false;
    }
    delete org.members[email];
    this.save();
    return true;
  }

  // ---- sites ------------------------------------------------------------------
  addSite(orgId, tenantId) {
    const org = this.data.orgs[orgId];
    if (!org) return null;
    if (!org.sites.includes(tenantId)) org.sites.push(tenantId);
    this.save();
    return org;
  }

  removeSite(tenantId) {
    for (const org of Object.values(this.data.orgs)) {
      const i = org.sites.indexOf(tenantId);
      if (i !== -1) {
        org.sites.splice(i, 1);
        this.save();
        return org.id;
      }
    }
    return null;
  }

  // ---- lookups the server asks for --------------------------------------------
  orgOf(tenantId) {
    return Object.values(this.data.orgs).find((o) => o.sites.includes(tenantId)) ?? null;
  }

  orgsFor(email) {
    return Object.values(this.data.orgs).filter((o) => email in o.members);
  }

  roleIn(orgId, email) {
    return this.data.orgs[orgId]?.members[email] ?? null;
  }

  // Which tenantIds may this user see? (superadmin bypasses this entirely)
  visibleTenantIds(email) {
    return new Set(this.orgsFor(email).flatMap((o) => o.sites));
  }

  // The platform overview: one row per org for the superadmin table.
  summary() {
    return Object.values(this.data.orgs).map((o) => ({
      id: o.id,
      name: o.name,
      members: o.members,
      sites: o.sites,
      memberCount: Object.keys(o.members).length,
      siteCount: o.sites.length,
    }));
  }
}
