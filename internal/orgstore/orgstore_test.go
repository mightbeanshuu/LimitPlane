package orgstore

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

// tmpFile returns a path inside a fresh temp dir. The file does not exist yet,
// which is exactly the "first boot" case New has to survive.
func tmpFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), ".orgs.json")
}

func TestSlug(t *testing.T) {
	// The slug rule is a compatibility contract, not a preference: ids already
	// written into .orgs.json by the Node service must keep resolving.
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"simple", "Anshu Labs", "org_anshu-labs"},
		{"apostrophe", "newuser's org", "org_newuser-s-org"},
		{"already lower", "acme", "org_acme"},
		{"leading junk", "  Acme  ", "org_acme"},
		{"trailing junk", "acme!!!", "org_acme"},
		{"run collapses", "a   ---   b", "org_a-b"},
		{"digits kept", "Team 42", "org_team-42"},
		{"unicode folds to dash", "Café Zürich", "org_caf-z-rich"},
		{"all junk", "!!!", "org_"},
		{"empty", "", "org_"},
		{"underscores are junk", "my_org_name", "org_my-org-name"},
		{"mixed case", "MiXeD CaSe", "org_mixed-case"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := slug(tc.in); got != tc.want {
				t.Fatalf("slug(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// The exported path must agree with the helper.
			s := New("")
			org := s.CreateOrg(tc.in, "owner@example.com")
			if org == nil {
				t.Fatal("CreateOrg returned nil on a fresh store")
			}
			if org.ID != tc.want {
				t.Fatalf("CreateOrg id = %q, want %q", org.ID, tc.want)
			}
		})
	}
}

func TestCreateOrg(t *testing.T) {
	s := New("")

	org := s.CreateOrg("Anshu Labs", "demo@limitplane.dev")
	if org == nil {
		t.Fatal("first CreateOrg returned nil")
	}
	if org.Name != "Anshu Labs" {
		t.Fatalf("name = %q", org.Name)
	}
	if org.Members["demo@limitplane.dev"] != RoleOwner {
		t.Fatalf("creator should be owner, got %q", org.Members["demo@limitplane.dev"])
	}
	if org.Sites == nil {
		t.Fatal("Sites should be an empty slice, not nil (it marshals to null)")
	}

	// A name that slugs onto an existing id is refused, even when the display
	// names differ — the id is the uniqueness key.
	if dup := s.CreateOrg("anshu labs", "someone@else.com"); dup != nil {
		t.Fatalf("duplicate slug should return nil, got %+v", dup)
	}
	if got := s.RoleIn("org_anshu-labs", "someone@else.com"); got != "" {
		t.Fatalf("refused CreateOrg must not touch the existing org, role = %q", got)
	}
}

func TestReturnedOrgIsACopy(t *testing.T) {
	// The point of the whole port: a caller must never hold a handle into the
	// live directory, because it would be mutating it without the lock.
	s := New("")
	org := s.CreateOrg("Acme", "a@x.com")
	org.Members["intruder@x.com"] = RoleOwner
	org.Sites = append(org.Sites, "evil.example")
	org.Name = "Not Acme"

	if got := s.RoleIn("org_acme", "intruder@x.com"); got != "" {
		t.Fatalf("mutating the returned Org leaked into the store: role = %q", got)
	}
	if s.OrgOf("evil.example") != nil {
		t.Fatal("mutating the returned Org's Sites leaked into the store")
	}
	if sum := s.Summary(); sum[0].Name != "Acme" {
		t.Fatalf("mutating the returned Org's Name leaked into the store: %q", sum[0].Name)
	}

	// Same for the summary rows and the OrgsFor copies.
	sum := s.Summary()
	sum[0].Members["intruder2@x.com"] = RoleAdmin
	if got := s.RoleIn("org_acme", "intruder2@x.com"); got != "" {
		t.Fatalf("Summary handed out live state: role = %q", got)
	}
	mine := s.OrgsFor("a@x.com")
	mine[0].Members["intruder3@x.com"] = RoleAdmin
	if got := s.RoleIn("org_acme", "intruder3@x.com"); got != "" {
		t.Fatalf("OrgsFor handed out live state: role = %q", got)
	}
}

func TestUsers(t *testing.T) {
	s := New("")

	u := s.CreateUser("a@x.com", "hunter2", "")
	if u.PlatformRole != "user" {
		t.Fatalf("empty platformRole should default to %q, got %q", "user", u.PlatformRole)
	}
	if len(u.Salt) != 16 {
		t.Fatalf("salt should be 8 bytes hex = 16 chars, got %d (%q)", len(u.Salt), u.Salt)
	}
	if len(u.Hash) != 64 {
		t.Fatalf("hash should be sha256 hex = 64 chars, got %d", len(u.Hash))
	}

	// Re-creating an existing user returns it UNCHANGED. The admin "invite"
	// flow calls this with an address that may already exist; silently resetting
	// the password there would be a takeover primitive.
	again := s.CreateUser("a@x.com", "a-different-password", "admin")
	if again != u {
		t.Fatalf("existing user changed: %+v -> %+v", u, again)
	}
	if !s.VerifyUser("a@x.com", "hunter2") {
		t.Fatal("original password stopped working after a duplicate CreateUser")
	}

	if s.CreateUser("staff@limitplane.dev", "pw", "admin").PlatformRole != "admin" {
		t.Fatal("explicit platformRole was not kept")
	}

	// Two users with the same password must not share a hash: that is the salt
	// doing its job.
	p := s.CreateUser("b@x.com", "hunter2", "")
	q := s.CreateUser("c@x.com", "hunter2", "")
	if p.Hash == q.Hash {
		t.Fatal("identical passwords produced identical hashes — salt is not being used")
	}

	if !s.HasUser("a@x.com") || s.HasUser("nobody@x.com") {
		t.Fatal("HasUser is wrong")
	}

	snap := s.Users()
	if len(snap) != 4 {
		t.Fatalf("Users() = %d entries, want 4", len(snap))
	}
	delete(snap, "a@x.com") // snapshot: must not affect the store
	if !s.HasUser("a@x.com") {
		t.Fatal("Users() returned live state")
	}

	s.DeleteUser("a@x.com")
	if s.HasUser("a@x.com") {
		t.Fatal("DeleteUser did not delete")
	}
	s.DeleteUser("a@x.com") // deleting twice is a no-op, not a panic
}

func TestVerifyUser(t *testing.T) {
	s := New("")
	s.CreateUser("real@x.com", "correct horse", "")

	cases := []struct {
		name     string
		email    string
		password string
		want     bool
	}{
		{"right password", "real@x.com", "correct horse", true},
		{"wrong password", "real@x.com", "correct horsey", false},
		{"empty password", "real@x.com", "", false},
		{"unknown email", "ghost@x.com", "correct horse", false},
		{"empty email", "", "correct horse", false},
		{"case-sensitive email", "REAL@x.com", "correct horse", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.VerifyUser(tc.email, tc.password); got != tc.want {
				t.Fatalf("VerifyUser(%q, %q) = %v, want %v", tc.email, tc.password, got, tc.want)
			}
		})
	}
}

func TestOnDiskPasswordFormatIsUnchanged(t *testing.T) {
	// The hash format is sha256(salt + password), hex. This is a fixed vector,
	// not a round-trip through our own hasher, so a "harmless" change to the
	// scheme cannot pass this test while locking every existing account out.
	file := tmpFile(t)
	const raw = `{
	  "orgs": {},
	  "users": {
	    "legacy@limitplane.dev": {
	      "salt": "0011223344556677",
	      "hash": "a568d088ecff015cdd74dc71f63b9d862a7ed2b2d806ac419219438d2a265885",
	      "platformRole": "user"
	    }
	  }
	}`
	if err := os.WriteFile(file, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	s := New(file)
	if !s.VerifyUser("legacy@limitplane.dev", "hunter2") {
		t.Fatal("an account written by the Node service no longer verifies")
	}
	if s.VerifyUser("legacy@limitplane.dev", "hunter3") {
		t.Fatal("wrong password verified")
	}
}

func TestMembers(t *testing.T) {
	s := New("")
	s.CreateOrg("Acme", "owner@x.com")

	cases := []struct {
		name  string
		orgID string
		email string
		role  string
		ok    bool
	}{
		{"admin", "org_acme", "admin@x.com", RoleAdmin, true},
		{"viewer", "org_acme", "viewer@x.com", RoleViewer, true},
		{"second owner", "org_acme", "owner2@x.com", RoleOwner, true},
		{"unknown org", "org_nope", "a@x.com", RoleAdmin, false},
		{"bogus role", "org_acme", "b@x.com", "superuser", false},
		{"empty role", "org_acme", "b@x.com", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			org, ok := s.AddMember(tc.orgID, tc.email, tc.role)
			if ok != tc.ok {
				t.Fatalf("AddMember ok = %v, want %v", ok, tc.ok)
			}
			if !tc.ok {
				if org != nil {
					t.Fatal("failed AddMember must return a nil org")
				}
				if got := s.RoleIn(tc.orgID, tc.email); got != "" {
					t.Fatalf("failed AddMember still granted role %q", got)
				}
				return
			}
			if org.Members[tc.email] != tc.role {
				t.Fatalf("returned org missing the new member")
			}
			if got := s.RoleIn(tc.orgID, tc.email); got != tc.role {
				t.Fatalf("RoleIn = %q, want %q", got, tc.role)
			}
		})
	}

	// A repeat AddMember changes the role rather than duplicating the member.
	s.AddMember("org_acme", "viewer@x.com", RoleAdmin)
	if got := s.RoleIn("org_acme", "viewer@x.com"); got != RoleAdmin {
		t.Fatalf("role was not upgraded, got %q", got)
	}

	if got := s.RoleIn("org_nope", "owner@x.com"); got != "" {
		t.Fatalf("RoleIn on an unknown org = %q, want \"\"", got)
	}
	if got := s.RoleIn("org_acme", "stranger@x.com"); got != "" {
		t.Fatalf("RoleIn for a non-member = %q, want \"\"", got)
	}
}

func TestRemoveMemberProtectsTheLastOwner(t *testing.T) {
	s := New("")
	s.CreateOrg("Acme", "owner@x.com")
	s.AddMember("org_acme", "viewer@x.com", RoleViewer)

	if s.RemoveMember("org_nope", "owner@x.com") {
		t.Fatal("unknown org should be false")
	}
	if s.RemoveMember("org_acme", "stranger@x.com") {
		t.Fatal("non-member should be false")
	}
	// The whole rule: an org whose last owner is gone is an org nobody can
	// manage, so this is refused even though the caller is allowed to ask.
	if s.RemoveMember("org_acme", "owner@x.com") {
		t.Fatal("removing the last owner should be refused")
	}
	if got := s.RoleIn("org_acme", "owner@x.com"); got != RoleOwner {
		t.Fatalf("refused removal still dropped the owner, role = %q", got)
	}

	if !s.RemoveMember("org_acme", "viewer@x.com") {
		t.Fatal("removing a viewer should succeed")
	}

	// With a second owner in place, the first may leave.
	s.AddMember("org_acme", "owner2@x.com", RoleOwner)
	if !s.RemoveMember("org_acme", "owner@x.com") {
		t.Fatal("removing an owner while another owner exists should succeed")
	}
	// ...and now owner2 is the last one, so they are pinned in turn.
	if s.RemoveMember("org_acme", "owner2@x.com") {
		t.Fatal("the new last owner should now be protected")
	}
}

func TestSites(t *testing.T) {
	s := New("")
	s.CreateOrg("Acme", "owner@x.com")
	s.CreateOrg("Beta", "b@x.com")

	if _, ok := s.AddSite("org_nope", "site.example"); ok {
		t.Fatal("AddSite on an unknown org should be false")
	}

	org, ok := s.AddSite("org_acme", "site.example")
	if !ok || len(org.Sites) != 1 {
		t.Fatalf("AddSite failed: ok=%v sites=%v", ok, org)
	}
	// Idempotent: connecting the same site twice is a retry, not a second site.
	org, _ = s.AddSite("org_acme", "site.example")
	if len(org.Sites) != 1 {
		t.Fatalf("AddSite is not idempotent: %v", org.Sites)
	}
	s.AddSite("org_acme", "second.example")

	if got := s.OrgOf("site.example"); got == nil || got.ID != "org_acme" {
		t.Fatalf("OrgOf = %+v, want org_acme", got)
	}
	if got := s.OrgOf("unclaimed.example"); got != nil {
		t.Fatalf("OrgOf on an unclaimed site = %+v, want nil", got)
	}

	if got := s.RemoveSite("nowhere.example"); got != "" {
		t.Fatalf("RemoveSite for an unknown site = %q, want \"\"", got)
	}
	if got := s.RemoveSite("site.example"); got != "org_acme" {
		t.Fatalf("RemoveSite = %q, want org_acme", got)
	}
	if s.OrgOf("site.example") != nil {
		t.Fatal("site still attached after RemoveSite")
	}
	// The sibling site is untouched — the splice must remove one element, not
	// corrupt the rest of the slice.
	if got := s.OrgOf("second.example"); got == nil || got.ID != "org_acme" {
		t.Fatalf("sibling site lost after RemoveSite: %+v", got)
	}
}

func TestOrgsForAndVisibleTenantIDs(t *testing.T) {
	s := New("")
	s.CreateOrg("Acme", "owner@x.com")
	s.CreateOrg("Beta", "owner@x.com")
	s.CreateOrg("Gamma", "other@x.com")
	s.AddMember("org_beta", "viewer@x.com", RoleViewer)
	s.AddSite("org_acme", "a1.example")
	s.AddSite("org_acme", "a2.example")
	s.AddSite("org_beta", "b1.example")
	s.AddSite("org_gamma", "g1.example")

	orgs := s.OrgsFor("owner@x.com")
	if len(orgs) != 2 {
		t.Fatalf("OrgsFor = %d orgs, want 2", len(orgs))
	}
	// Deterministic order: the server takes OrgsFor(...)[0] as a default org,
	// and Go randomizes map iteration, so this must be sorted by id.
	if orgs[0].ID != "org_acme" || orgs[1].ID != "org_beta" {
		t.Fatalf("OrgsFor is not in id order: %v, %v", orgs[0].ID, orgs[1].ID)
	}
	if len(s.OrgsFor("nobody@x.com")) != 0 {
		t.Fatal("OrgsFor for a stranger should be empty")
	}

	cases := []struct {
		name  string
		email string
		want  []string
	}{
		{"member of two orgs", "owner@x.com", []string{"a1.example", "a2.example", "b1.example"}},
		{"viewer sees the org's sites", "viewer@x.com", []string{"b1.example"}},
		{"single org", "other@x.com", []string{"g1.example"}},
		{"stranger sees nothing", "nobody@x.com", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set := s.VisibleTenantIDs(tc.email)
			got := make([]string, 0, len(set))
			for id := range set {
				got = append(got, id)
			}
			sort.Strings(got)
			want := tc.want
			if want == nil {
				want = []string{}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("VisibleTenantIDs(%q) = %v, want %v", tc.email, got, want)
			}
		})
	}
}

func TestSummary(t *testing.T) {
	s := New("")
	if got := s.Summary(); len(got) != 0 {
		t.Fatalf("empty store summary = %v", got)
	}
	s.CreateOrg("Zeta", "z@x.com")
	s.CreateOrg("Acme", "owner@x.com")
	s.AddMember("org_acme", "viewer@x.com", RoleViewer)
	s.AddSite("org_acme", "a1.example")

	sum := s.Summary()
	if len(sum) != 2 {
		t.Fatalf("summary = %d rows, want 2", len(sum))
	}
	if sum[0].ID != "org_acme" || sum[1].ID != "org_zeta" {
		t.Fatalf("summary not sorted by id: %q, %q", sum[0].ID, sum[1].ID)
	}
	if sum[0].MemberCount != 2 || sum[0].SiteCount != 1 {
		t.Fatalf("counts wrong: members=%d sites=%d", sum[0].MemberCount, sum[0].SiteCount)
	}
	if sum[1].MemberCount != 1 || sum[1].SiteCount != 0 {
		t.Fatalf("counts wrong for the empty org: members=%d sites=%d", sum[1].MemberCount, sum[1].SiteCount)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	file := tmpFile(t)

	s := New(file)
	s.CreateUser("owner@x.com", "hunter2", "")
	s.CreateUser("staff@limitplane.dev", "pw", "admin")
	s.CreateOrg("Anshu Labs", "owner@x.com")
	s.AddMember("org_anshu-labs", "viewer@x.com", RoleViewer)
	s.AddSite("org_anshu-labs", "visualise.vercel.app")

	// Every mutator persists, so nothing extra is needed here — that is the
	// behaviour under test.
	reloaded := New(file)

	if !reloaded.VerifyUser("owner@x.com", "hunter2") {
		t.Fatal("password did not survive the round trip")
	}
	if reloaded.Users()["staff@limitplane.dev"].PlatformRole != "admin" {
		t.Fatal("platformRole did not survive the round trip")
	}
	if got := reloaded.RoleIn("org_anshu-labs", "viewer@x.com"); got != RoleViewer {
		t.Fatalf("membership did not survive: %q", got)
	}
	if got := reloaded.OrgOf("visualise.vercel.app"); got == nil || got.Name != "Anshu Labs" {
		t.Fatalf("site did not survive: %+v", got)
	}
	if !reflect.DeepEqual(reloaded.Summary(), s.Summary()) {
		t.Fatalf("summary changed across a reload:\n got %+v\nwant %+v", reloaded.Summary(), s.Summary())
	}

	// The file must be the same shape the Node service reads and writes.
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		Orgs  map[string]json.RawMessage `json:"orgs"`
		Users map[string]json.RawMessage `json:"users"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatalf("on-disk file is not the {orgs, users} shape: %v", err)
	}
	if len(w.Orgs) != 1 || len(w.Users) != 2 {
		t.Fatalf("on-disk counts wrong: %d orgs, %d users", len(w.Orgs), len(w.Users))
	}

	// A user deleted after the reload must stay deleted.
	reloaded.DeleteUser("owner@x.com")
	if New(file).HasUser("owner@x.com") {
		t.Fatal("DeleteUser did not persist")
	}
}

func TestNewToleratesMissingAndCorruptFiles(t *testing.T) {
	cases := []struct {
		name  string
		write string // "" means do not create the file at all
	}{
		{"missing file", ""},
		{"empty file", " "},
		{"not json", "this is not json"},
		{"json but wrong shape", `[1,2,3]`},
		{"null org entry", `{"orgs":{"org_x":null},"users":{}}`},
		{"org without members", `{"orgs":{"org_x":{"id":"org_x","name":"X"}},"users":{}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file := tmpFile(t)
			if tc.write != "" {
				if err := os.WriteFile(file, []byte(tc.write), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			s := New(file) // must not panic and must not refuse to boot
			// Whatever came in, the store is usable afterwards.
			if org := s.CreateOrg("Fresh", "a@x.com"); org == nil {
				t.Fatal("store unusable after loading this file")
			}
			if err := s.Save(); err != nil {
				t.Fatalf("Save after a bad load: %v", err)
			}
		})
	}

	// An org row missing its members map must still take members.
	file := tmpFile(t)
	if err := os.WriteFile(file, []byte(`{"orgs":{"org_x":{"id":"org_x","name":"X"}},"users":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(file)
	if _, ok := s.AddMember("org_x", "a@x.com", RoleOwner); !ok {
		t.Fatal("AddMember failed on an org loaded without a members map")
	}
	if s.Summary()[0].Sites == nil {
		t.Fatal("Sites should be normalised to an empty slice on load")
	}
}

func TestSaveErrorIsWrapped(t *testing.T) {
	// A path whose parent directory does not exist cannot be written; the error
	// must name the file rather than surfacing a bare syscall failure.
	s := New(filepath.Join(t.TempDir(), "no-such-dir", ".orgs.json"))
	err := s.Save()
	if err == nil {
		t.Fatal("Save to an unwritable path should fail")
	}
	if !strings.Contains(err.Error(), "orgstore: save") {
		t.Fatalf("error not wrapped with context: %v", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("wrapping lost the underlying error (%%w not used?): %v", err)
	}

	// The in-memory store keeps working even though it cannot persist: losing
	// the disk must not take the running gateway down.
	if org := s.CreateOrg("Acme", "a@x.com"); org == nil {
		t.Fatal("mutators should still work when the file cannot be written")
	}
}

func TestWritesAreAtomicAndLeaveNoLitter(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, ".orgs.json")

	s := New(file)
	for i := 0; i < 20; i++ {
		s.CreateUser("u@x.com", "pw", "")
		s.CreateOrg("Acme", "u@x.com")
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly the real file: every temp file was renamed over it or cleaned up.
	if len(entries) != 1 || entries[0].Name() != ".orgs.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("temp files left behind: %v", names)
	}
}

// TestConcurrentAccess is the reason this package exists in Go at all. Node
// could not race here; this can, so run it under -race.
func TestConcurrentAccess(t *testing.T) {
	file := tmpFile(t)
	s := New(file)
	s.CreateOrg("Shared", "owner@x.com")
	s.CreateUser("owner@x.com", "hunter2", "")

	const goroutines = 24
	const iterations = 40

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			email := "user" + string(rune('a'+g%26)) + "@x.com"
			site := "site" + string(rune('a'+g%26)) + ".example"
			for i := 0; i < iterations; i++ {
				switch i % 10 {
				case 0:
					s.CreateUser(email, "pw", "")
				case 1:
					s.VerifyUser(email, "pw")
				case 2:
					s.CreateOrg("Org "+email, email)
				case 3:
					s.AddMember("org_shared", email, RoleViewer)
				case 4:
					s.AddSite("org_shared", site)
				case 5:
					s.RemoveSite(site)
				case 6:
					s.RemoveMember("org_shared", email)
				case 7:
					s.OrgsFor(email)
					s.OrgOf(site)
					s.RoleIn("org_shared", email)
				case 8:
					s.VisibleTenantIDs(email)
					s.Users()
				case 9:
					s.Summary()
					_ = s.Save()
				}
			}
		}(g)
	}
	wg.Wait()

	// The invariant that must hold no matter how the interleaving fell out.
	if got := s.RoleIn("org_shared", "owner@x.com"); got != RoleOwner {
		t.Fatalf("the last owner was lost under concurrency: role = %q", got)
	}
	if !s.VerifyUser("owner@x.com", "hunter2") {
		t.Fatal("the seeded login was corrupted under concurrency")
	}
	// And the file is still parseable — no interleaved half-write survived.
	if len(New(file).Summary()) == 0 {
		t.Fatal("the persisted file did not survive concurrent writers")
	}
}
