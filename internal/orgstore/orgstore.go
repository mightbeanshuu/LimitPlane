// Package orgstore is the platform's directory: organizations, the people in
// them, and the sites they own.
//
// The shape is the one Vercel made familiar:
//
//	PLATFORM (LimitPlane staff = superadmin, sees everything)
//	  └── Organization  ("anshu-labs")
//	        ├── members: email -> role   (owner / admin / viewer)
//	        └── sites:   tenantIds       (each connected site belongs to ONE org)
//
// One JSON file on disk, loaded at boot, rewritten on every change — the same
// honest persistence as the tenant store. A real deployment swaps this for a
// database; every method below maps 1:1 to a table you would create there.
//
// # Why this package looks different from the Node original
//
// Node served every request on a single thread, so a plain object was a safe
// database by accident: two handlers could never be halfway through a write at
// the same moment. Go serves each request on its own goroutine, so every map
// and slice here lives behind a mutex — and nothing internal is ever handed out
// by reference. Callers get copies. A caller that mutated an *Org it was given
// would be editing the live directory from another goroutine with no lock held,
// which is a data race the Node version could not have. The copying below is
// not politeness; it is the port.
//
// Writes are atomic: marshal, write a temp file in the same directory, then
// os.Rename over the real one. Rename within a directory is atomic, so a crash
// mid-write leaves the previous good file intact rather than a truncated one.
// Losing the last change is survivable; losing the whole directory is not.
//
// # About the passwords
//
// Passwords are stored as sha256(salt + password) with a per-user 8-byte salt,
// hex encoded. That is DEMO GRADE and deliberately preserved: this is the exact
// format already sitting in .orgs.json, and the port has to keep verifying
// accounts created by the Node service. A real deployment uses scrypt, bcrypt
// or argon2id instead, whose entire purpose is to be SLOW and memory-hard, so
// an attacker who steals the file cannot try billions of guesses per second the
// way a single bare sha256 invites. The salt still earns its keep — no rainbow
// tables, and two users with the same password get different hashes — but the
// work factor, the part that actually buys time after a breach, is missing.
// Comparison is still constant-time, because leaking "how many bytes matched"
// through timing is free to fix and there is no reason to pay for it.
package orgstore

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Roles a member may hold inside an org. Anything else is rejected by AddMember.
const (
	RoleOwner  = "owner"
	RoleAdmin  = "admin"
	RoleViewer = "viewer"
)

// Org is one organization: a name, the people who can act on it, and the sites
// it owns. Every connected site belongs to exactly one org.
type Org struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Members map[string]string `json:"members"` // email -> "owner"|"admin"|"viewer"
	Sites   []string          `json:"sites"`   // tenantIds
}

// clone returns a deep copy. Every method that hands an Org to a caller goes
// through this, so no caller can reach into the live directory.
func (o *Org) clone() Org {
	c := Org{
		ID:      o.ID,
		Name:    o.Name,
		Members: make(map[string]string, len(o.Members)),
		// Always allocate, never leave nil: a nil slice marshals to JSON null
		// and the dashboard would have to special-case "no sites yet".
		Sites: make([]string, len(o.Sites)),
	}
	for k, v := range o.Members {
		c.Members[k] = v
	}
	copy(c.Sites, o.Sites)
	return c
}

// User is one login. The password itself is never stored — see the package doc
// for why this hashing scheme is demo grade and why it stays anyway.
type User struct {
	Salt         string `json:"salt"`
	Hash         string `json:"hash"`
	PlatformRole string `json:"platformRole"`
}

// OrgSummary is one row of the superadmin's platform overview: an org plus the
// two counts the table shows, precomputed so the UI does no arithmetic.
type OrgSummary struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Members     map[string]string `json:"members"`
	Sites       []string          `json:"sites"`
	MemberCount int               `json:"memberCount"`
	SiteCount   int               `json:"siteCount"`
}

// Store is the directory. Everything is guarded by one mutex: the operations
// are short, they are not on the hot request path (the limiter never touches
// this), and one lock is far easier to prove correct than several.
type Store struct {
	mu    sync.RWMutex
	file  string
	orgs  map[string]*Org
	users map[string]User
}

// world is the on-disk shape, kept identical to the Node version's so an
// existing .orgs.json loads unchanged.
type world struct {
	Orgs  map[string]*Org `json:"orgs"`
	Users map[string]User `json:"users"`
}

// New opens the directory backed by file, loading it if it is there and
// starting an empty world if it is not.
//
// A missing or unreadable file is not an error: first boot has no file, and the
// Node version treated both the same way. Since every mutator persists
// immediately, the first write creates it. Pass "" for a purely in-memory store
// (useful in tests).
func New(file string) *Store {
	s := &Store{
		file:  file,
		orgs:  map[string]*Org{},
		users: map[string]User{},
	}
	if file == "" {
		return s
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return s // first boot — empty world
	}
	var w world
	if err := json.Unmarshal(raw, &w); err != nil {
		return s // corrupt file: start clean rather than refuse to boot
	}
	for id, o := range w.Orgs {
		if o == nil {
			continue
		}
		if o.Members == nil {
			o.Members = map[string]string{}
		}
		if o.Sites == nil {
			o.Sites = []string{}
		}
		if o.ID == "" {
			o.ID = id // tolerate a hand-edited file that omitted the redundant id
		}
		s.orgs[id] = o
	}
	for email, u := range w.Users {
		s.users[email] = u
	}
	return s
}

// Save writes the directory to disk atomically. Mutators call this for you; it
// is exported for callers that change something through a lower-level path (the
// admin user-delete flow does) and for anyone who wants the error.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

// saveLocked marshals and writes while the caller still holds the write lock.
// Both halves stay inside the lock on purpose: marshalling under the lock is
// the only way to get a consistent snapshot, and writing under it stops two
// savers from landing out of order and leaving the older state on disk.
func (s *Store) saveLocked() error {
	if s.file == "" {
		return nil
	}
	// Indent to match the Node version so the file stays human-diffable.
	raw, err := json.MarshalIndent(world{Orgs: s.orgs, Users: s.users}, "", "  ")
	if err != nil {
		return fmt.Errorf("orgstore: marshal: %w", err)
	}
	if err := atomicWriteFile(s.file, raw); err != nil {
		return fmt.Errorf("orgstore: save %s: %w", s.file, err)
	}
	return nil
}

// atomicWriteFile writes data to path via a temp file in the SAME directory
// followed by a rename. Same directory matters: rename is only atomic within a
// filesystem, and a temp dir may well be on another one.
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	// Flush the bytes to the device before publishing the name. Without this a
	// power cut can leave a renamed-but-empty file, which is the exact failure
	// the temp-file dance was meant to prevent.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp: %w", err)
	}
	return nil
}

// ---- users ------------------------------------------------------------------

// hashPassword is the on-disk password format: sha256(salt + password), hex.
// See the package doc for why this is demo grade and why it is kept.
func hashPassword(salt, password string) string {
	sum := sha256.Sum256([]byte(salt + password))
	return hex.EncodeToString(sum[:])
}

// CreateUser adds a login and returns it. An email that already exists is
// returned UNCHANGED — this doubles as the "invite" path, where the admin
// re-submits an address that may or may not already have an account, and it
// must never silently reset a password.
//
// platformRole "" means the ordinary "user" role; "admin" is platform staff.
func (s *Store) CreateUser(email, password, platformRole string) User {
	if platformRole == "" {
		platformRole = "user"
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if u, ok := s.users[email]; ok {
		return u
	}
	// crypto/rand.Read is documented never to fail; it panics internally if the
	// OS entropy source is broken, which is not a condition this store could do
	// anything sensible about anyway.
	var b [8]byte
	_, _ = rand.Read(b[:])
	salt := hex.EncodeToString(b[:])

	u := User{Salt: salt, Hash: hashPassword(salt, password), PlatformRole: platformRole}
	s.users[email] = u
	_ = s.saveLocked() // best effort: the caller has no error to return it in
	return u
}

// VerifyUser reports whether the email and password match a stored login.
//
// An unknown email and a wrong password both return false with nothing else
// said. Telling them apart would hand an attacker a free account-enumeration
// oracle: "this address exists, keep guessing".
func (s *Store) VerifyUser(email, password string) bool {
	s.mu.RLock()
	u, ok := s.users[email]
	s.mu.RUnlock()
	if !ok {
		return false
	}
	got := hashPassword(u.Salt, password)
	// Constant-time: never let the comparison's duration reveal how many
	// leading bytes were right.
	return subtle.ConstantTimeCompare([]byte(got), []byte(u.Hash)) == 1
}

// HasUser reports whether an account exists for this email.
func (s *Store) HasUser(email string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.users[email]
	return ok
}

// DeleteUser removes a login. It does NOT touch org memberships — the caller
// walks OrgsFor + RemoveMember first, because the last-owner rule lives there
// and deleting the account cannot be allowed to route around it.
func (s *Store) DeleteUser(email string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[email]; !ok {
		return // nothing to do, and nothing to rewrite
	}
	delete(s.users, email)
	_ = s.saveLocked()
}

// Users returns a snapshot copy of every login, keyed by email. The caller may
// do whatever it likes with the map; the directory is unaffected.
func (s *Store) Users() map[string]User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]User, len(s.users))
	for k, v := range s.users {
		out[k] = v
	}
	return out
}

// ---- orgs -------------------------------------------------------------------

// slug turns a display name into the org id: "org_" + the lowercased name with
// every run of characters outside [a-z0-9] folded to a single "-", then the
// leading and trailing "-" trimmed.
//
//	"Anshu Labs"     -> "org_anshu-labs"
//	"newuser's org"  -> "org_newuser-s-org"
//
// The id doubles as the uniqueness key, which is why CreateOrg refuses names
// that collide after slugging: two orgs with one id would be one org.
func slug(name string) string {
	lower := strings.ToLower(name)
	var b strings.Builder
	b.Grow(len(lower))
	dash := false
	for i := 0; i < len(lower); i++ {
		c := lower[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
			dash = false
			continue
		}
		if !dash { // collapse a run of junk into one "-"
			b.WriteByte('-')
			dash = true
		}
	}
	return "org_" + strings.Trim(b.String(), "-")
}

// CreateOrg makes an org owned by ownerEmail and returns a copy of it, or nil
// if the name already slugs to an existing id.
func (s *Store) CreateOrg(name, ownerEmail string) *Org {
	id := slug(name)

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.orgs[id]; exists {
		return nil // names must be unique enough to slug
	}
	o := &Org{
		ID:      id,
		Name:    name,
		Members: map[string]string{ownerEmail: RoleOwner},
		Sites:   []string{},
	}
	s.orgs[id] = o
	_ = s.saveLocked()

	c := o.clone()
	return &c
}

// validRole gates AddMember. Roles are a closed set; a typo must not create a
// third kind of member that no permission check knows how to interpret.
func validRole(role string) bool {
	return role == RoleOwner || role == RoleAdmin || role == RoleViewer
}

// AddMember grants email a role in the org, creating or overwriting the
// membership. It returns a copy of the org and true, or (nil, false) when the
// org is unknown or the role is not one of owner/admin/viewer.
func (s *Store) AddMember(orgID, email, role string) (*Org, bool) {
	if !validRole(role) {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orgs[orgID]
	if !ok {
		return nil, false
	}
	if o.Members == nil {
		o.Members = map[string]string{}
	}
	o.Members[email] = role
	_ = s.saveLocked()

	c := o.clone()
	return &c, true
}

// RemoveMember drops a membership. It returns false for an unknown org, a
// non-member, or the LAST remaining owner — an org nobody can manage is a dead
// org, and no API should be able to create one by accident.
func (s *Store) RemoveMember(orgID, email string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orgs[orgID]
	if !ok {
		return false
	}
	role, isMember := o.Members[email]
	if !isMember {
		return false
	}
	if role == RoleOwner {
		owners := 0
		for _, r := range o.Members {
			if r == RoleOwner {
				owners++
			}
		}
		if owners == 1 {
			return false
		}
	}
	delete(o.Members, email)
	_ = s.saveLocked()
	return true
}

// ---- sites ------------------------------------------------------------------

// AddSite attaches a tenant id to an org, idempotently: connecting the same
// site twice is a retry, not a second site. Returns a copy of the org, or
// (nil, false) if the org is unknown.
func (s *Store) AddSite(orgID, tenantID string) (*Org, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orgs[orgID]
	if !ok {
		return nil, false
	}
	found := false
	for _, id := range o.Sites {
		if id == tenantID {
			found = true
			break
		}
	}
	if !found {
		o.Sites = append(o.Sites, tenantID)
	}
	_ = s.saveLocked()

	c := o.clone()
	return &c, true
}

// RemoveSite detaches a tenant id from whichever org holds it and returns that
// org's id, or "" if no org did. Orgs are scanned in id order so the answer is
// stable — Go randomizes map iteration, and the Node version leaned on
// insertion order here without noticing.
func (s *Store) RemoveSite(tenantID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.sortedOrgIDsLocked() {
		o := s.orgs[id]
		for i, site := range o.Sites {
			if site == tenantID {
				o.Sites = append(o.Sites[:i], o.Sites[i+1:]...)
				_ = s.saveLocked()
				return o.ID
			}
		}
	}
	return ""
}

// ---- lookups the server asks for --------------------------------------------

// sortedOrgIDsLocked gives every org id in a deterministic order. Callers must
// already hold the lock.
//
// Go randomizes map iteration deliberately, so any caller that takes "the first
// org" would otherwise get a different answer per process — and the server does
// exactly that when picking a default org for a user. Sorting by id makes that
// choice reproducible.
func (s *Store) sortedOrgIDsLocked() []string {
	ids := make([]string, 0, len(s.orgs))
	for id := range s.orgs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// OrgOf returns a copy of the org that owns this site, or nil if the site is
// not claimed by anyone.
func (s *Store) OrgOf(tenantID string) *Org {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, id := range s.sortedOrgIDsLocked() {
		o := s.orgs[id]
		for _, site := range o.Sites {
			if site == tenantID {
				c := o.clone()
				return &c
			}
		}
	}
	return nil
}

// OrgsFor returns copies of every org this email is a member of, in id order.
func (s *Store) OrgsFor(email string) []Org {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []Org{}
	for _, id := range s.sortedOrgIDsLocked() {
		o := s.orgs[id]
		if _, member := o.Members[email]; member {
			out = append(out, o.clone())
		}
	}
	return out
}

// RoleIn returns this email's role in the org, or "" if the org is unknown or
// the email is not in it. "" is the natural zero here: no role at all.
func (s *Store) RoleIn(orgID, email string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.orgs[orgID]
	if !ok {
		return ""
	}
	return o.Members[email] // "" when absent
}

// VisibleTenantIDs is the answer to "which sites may this user see?" — the
// union of the sites of every org they belong to. It is a set because the
// caller only ever asks it membership questions, once per row it is filtering.
//
// The platform superadmin never calls this: staff bypass the org layer entirely.
func (s *Store) VisibleTenantIDs(email string) map[string]struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]struct{}{}
	for _, o := range s.orgs {
		if _, member := o.Members[email]; !member {
			continue
		}
		for _, site := range o.Sites {
			out[site] = struct{}{}
		}
	}
	return out
}

// Summary is the platform overview: one row per org for the superadmin table,
// in id order, with the counts already worked out.
func (s *Store) Summary() []OrgSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]OrgSummary, 0, len(s.orgs))
	for _, id := range s.sortedOrgIDsLocked() {
		c := s.orgs[id].clone()
		out = append(out, OrgSummary{
			ID:          c.ID,
			Name:        c.Name,
			Members:     c.Members,
			Sites:       c.Sites,
			MemberCount: len(c.Members),
			SiteCount:   len(c.Sites),
		})
	}
	return out
}
