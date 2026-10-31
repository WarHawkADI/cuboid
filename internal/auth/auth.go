// Package auth decides who may read which stores. A request's access comes
// only from its API key: the store or company id in the path is a selector
// within the key's scope, and anything outside it is "not found".
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Kind is the kind of scope a key grants.
type Kind string

const (
	Store    Kind = "store"
	Company  Kind = "company"
	Operator Kind = "operator"
)

// Scope is what an authenticated key may read.
type Scope struct {
	Kind   Kind
	ID     int32  // store or company id; 0 for operator
	Prefix string // the key's public prefix, for audit and logs
}

// Keys authenticates bearer tokens against hashed keys.
type Keys struct {
	pepper []byte
	mu     sync.RWMutex
	byPfx  map[string]keyRec
}

type keyRec struct {
	hash  []byte
	scope Scope
}

// NewKeys returns an authenticator using pepper for HMAC.
func NewKeys(pepper string) *Keys {
	return &Keys{pepper: []byte(pepper), byPfx: map[string]keyRec{}}
}

const secretLen = 32

// prefixOf returns the stored prefix: the scope part plus six characters of
// the secret, enough to identify a key in logs without revealing it.
func prefixOf(token string) (string, bool) {
	i := strings.LastIndexByte(token, '_')
	if i < 0 || len(token)-i-1 < 20 {
		return "", false
	}
	return token[:i+7], true
}

func (k *Keys) hash(token string) []byte {
	m := hmac.New(sha256.New, k.pepper)
	m.Write([]byte(token))
	return m.Sum(nil)
}

// Authenticate returns the scope of a token. Comparison is constant time.
func (k *Keys) Authenticate(token string) (Scope, bool) {
	pfx, ok := prefixOf(token)
	if !ok {
		return Scope{}, false
	}
	k.mu.RLock()
	rec, ok := k.byPfx[pfx]
	k.mu.RUnlock()
	h := k.hash(token)
	if !ok {
		subtle.ConstantTimeCompare(h, h) // keep timing similar for unknown prefixes
		return Scope{}, false
	}
	if subtle.ConstantTimeCompare(h, rec.hash) != 1 {
		return Scope{}, false
	}
	return rec.scope, true
}

// Load reads every unrevoked key from the aggregate store.
func (k *Keys) Load(ctx context.Context, pool *pgxpool.Pool) error {
	rows, err := pool.Query(ctx, "SELECT prefix, hash, scope, coalesce(scope_id, 0) FROM api_keys WHERE revoked_at IS NULL")
	if err != nil {
		return fmt.Errorf("load API keys: %w", err)
	}
	defer rows.Close()
	m := map[string]keyRec{}
	for rows.Next() {
		var pfx, scope string
		var hash []byte
		var id int32
		if err := rows.Scan(&pfx, &hash, &scope, &id); err != nil {
			return err
		}
		m[pfx] = keyRec{hash: hash, scope: Scope{Kind: Kind(scope), ID: id, Prefix: pfx}}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	k.mu.Lock()
	k.byPfx = m
	k.mu.Unlock()
	return nil
}

// Token builds a key string for a scope from a secret.
func Token(kind Kind, id int32, secret []byte) string {
	s := hex.EncodeToString(secret) // hex: no "_" inside the secret, so the prefix parses unambiguously
	switch kind {
	case Store:
		return fmt.Sprintf("cub_store_%d_%s", id, s)
	case Company:
		return fmt.Sprintf("cub_co_%d_%s", id, s)
	}
	return "cub_op_" + s
}

// NewSecret returns a random 32-byte secret.
func NewSecret() []byte {
	b := make([]byte, secretLen)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the OS random source failing is not recoverable
	}
	return b
}

// DemoSecret derives a reproducible secret, so the README's demo keys work
// verbatim on every machine. Only used when CUBOID_DEMO_MODE is on.
func DemoSecret(seed string, kind Kind, id int32) []byte {
	m := hmac.New(sha256.New, []byte(seed))
	fmt.Fprintf(m, "%s:%d", kind, id)
	return m.Sum(nil)
}

// Save stores a key's hash and returns its prefix.
func (k *Keys) Save(ctx context.Context, pool *pgxpool.Pool, token string, kind Kind, id int32) error {
	pfx, ok := prefixOf(token)
	if !ok {
		return fmt.Errorf("malformed key")
	}
	var sid any
	if kind != Operator {
		sid = id
	}
	_, err := pool.Exec(ctx, `INSERT INTO api_keys (prefix, hash, scope, scope_id) VALUES ($1, $2, $3, $4)
		ON CONFLICT (prefix) DO UPDATE SET hash = EXCLUDED.hash, scope = EXCLUDED.scope, scope_id = EXCLUDED.scope_id, revoked_at = NULL`,
		pfx, k.hash(token), string(kind), sid)
	return err
}

// Membership maps companies to their stores, kept from the mirrored tenants
// table (never read from the main database).
type Membership struct {
	mu        sync.RWMutex
	byCompany map[int32][]int32
	company   map[int32]int32
	names     map[int32]string
}

// NewMembership returns an empty membership map.
func NewMembership() *Membership {
	return &Membership{byCompany: map[int32][]int32{}, company: map[int32]int32{}, names: map[int32]string{}}
}

// Load reads membership from the tenants mirror with the writer role.
func (m *Membership) Load(ctx context.Context, pool *pgxpool.Pool, table, tenantCol, companyCol, nameCol string) error {
	q := fmt.Sprintf("SELECT %s, %s, coalesce(%s::text, '') FROM %s", tenantCol, companyCol, nameCol, table)
	rows, err := pool.Query(ctx, q)
	if err != nil {
		return fmt.Errorf("load company membership: %w", err)
	}
	defer rows.Close()
	byCo := map[int32][]int32{}
	co := map[int32]int32{}
	names := map[int32]string{}
	for rows.Next() {
		var t, c int32
		var name string
		if err := rows.Scan(&t, &c, &name); err != nil {
			return err
		}
		byCo[c] = append(byCo[c], t)
		co[t] = c
		names[t] = name
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for c := range byCo {
		sort.Slice(byCo[c], func(i, j int) bool { return byCo[c][i] < byCo[c][j] })
	}
	m.mu.Lock()
	m.byCompany, m.company, m.names = byCo, co, names
	m.mu.Unlock()
	return nil
}

// Stores returns a company's stores.
func (m *Membership) Stores(company int32) []int32 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]int32(nil), m.byCompany[company]...)
}

// CompanyOf returns a store's company.
func (m *Membership) CompanyOf(tenant int32) (int32, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.company[tenant]
	return c, ok
}

// Name returns a store's name.
func (m *Membership) Name(tenant int32) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.names[tenant]
}

// AllTenants returns every known store.
func (m *Membership) AllTenants() []int32 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]int32, 0, len(m.company))
	for t := range m.company {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// AllCompanies returns every company with stores.
func (m *Membership) AllCompanies() []int32 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]int32, 0, len(m.byCompany))
	for c := range m.byCompany {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Resolve returns the tenants a request may read when it selects a store
// (company=false) or company (company=true) by id. ok=false means the id is
// outside the key's scope or does not exist; the caller answers 404 either way.
func (m *Membership) Resolve(s Scope, company bool, id int32) ([]int32, bool) {
	if company {
		stores := m.Stores(id)
		if len(stores) == 0 {
			return nil, false
		}
		if s.Kind == Operator || (s.Kind == Company && s.ID == id) {
			return stores, true
		}
		return nil, false
	}
	c, exists := m.CompanyOf(id)
	if !exists {
		return nil, false
	}
	switch s.Kind {
	case Operator:
		return []int32{id}, true
	case Store:
		if s.ID == id {
			return []int32{id}, true
		}
	case Company:
		if s.ID == c {
			return []int32{id}, true
		}
	}
	return nil, false
}

// ParseID reads a path id. Anything that is not a positive integer is "not found".
func ParseID(s string) (int32, bool) {
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil || n <= 0 {
		return 0, false
	}
	return int32(n), true
}
