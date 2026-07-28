import { test } from "node:test";
import assert from "node:assert/strict";
import { OrgStore } from "../../src/gateway/orgStore.js";

test("users: passwords are salted-hashed and verified, never stored plain", () => {
  const store = new OrgStore();
  store.createUser("a@x.dev", "secret1");
  assert.equal(store.data.users["a@x.dev"].hash.includes("secret1"), false);
  assert.ok(store.verifyUser("a@x.dev", "secret1"));
  assert.equal(store.verifyUser("a@x.dev", "wrong"), null);
  assert.equal(store.verifyUser("ghost@x.dev", "secret1"), null);
});

test("org lifecycle: create, add members, last owner is protected", () => {
  const store = new OrgStore();
  const org = store.createOrg("Anshu Labs", "anshu@x.dev");
  assert.equal(org.id, "org_anshu-labs");
  assert.equal(org.members["anshu@x.dev"], "owner");

  store.addMember(org.id, "friend@x.dev", "viewer");
  assert.equal(store.roleIn(org.id, "friend@x.dev"), "viewer");
  assert.equal(store.addMember(org.id, "x@x.dev", "god"), null); // no invented roles

  assert.equal(store.removeMember(org.id, "anshu@x.dev"), false); // last owner stays
  assert.equal(store.removeMember(org.id, "friend@x.dev"), true);
});

test("sites belong to exactly one org and visibility follows membership", () => {
  const store = new OrgStore();
  const a = store.createOrg("Alpha", "a@x.dev");
  const b = store.createOrg("Beta", "b@x.dev");
  store.addSite(a.id, "visualise.vercel.app");
  store.addSite(b.id, "other.site");

  assert.equal(store.orgOf("visualise.vercel.app").id, a.id);
  assert.deepEqual([...store.visibleTenantIds("a@x.dev")], ["visualise.vercel.app"]);
  assert.deepEqual([...store.visibleTenantIds("b@x.dev")], ["other.site"]);

  assert.equal(store.removeSite("visualise.vercel.app"), a.id);
  assert.equal(store.orgOf("visualise.vercel.app"), null);
});

test("summary gives the superadmin one row per org", () => {
  const store = new OrgStore();
  const o = store.createOrg("Gamma", "g@x.dev");
  store.addMember(o.id, "h@x.dev", "admin");
  store.addSite(o.id, "site1");
  const row = store.summary()[0];
  assert.equal(row.memberCount, 2);
  assert.equal(row.siteCount, 1);
});
