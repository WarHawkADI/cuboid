package auth

import "testing"

func members() *Membership {
	m := NewMembership()
	m.company = map[int32]int32{1: 7, 2: 7, 3: 8}
	m.byCompany = map[int32][]int32{7: {1, 2}, 8: {3}}
	return m
}

func TestStoreKeySeesOnlyItsStore(t *testing.T) {
	m := members()
	s := Scope{Kind: Store, ID: 1}
	if got, ok := m.Resolve(s, false, 1); !ok || len(got) != 1 || got[0] != 1 {
		t.Errorf("own store: %v, %v", got, ok)
	}
	for _, id := range []int32{2, 3, 999} {
		if _, ok := m.Resolve(s, false, id); ok {
			t.Errorf("store 1's key opened store %d", id)
		}
	}
	if _, ok := m.Resolve(s, true, 7); ok {
		t.Error("a store key opened its company")
	}
}

func TestCompanyKeySeesItsStoresOnly(t *testing.T) {
	m := members()
	s := Scope{Kind: Company, ID: 7}
	if got, ok := m.Resolve(s, true, 7); !ok || len(got) != 2 {
		t.Errorf("own company: %v, %v", got, ok)
	}
	if _, ok := m.Resolve(s, false, 2); !ok {
		t.Error("a company key should open its own store")
	}
	if _, ok := m.Resolve(s, false, 3); ok {
		t.Error("company 7's key opened another company's store")
	}
	if _, ok := m.Resolve(s, true, 8); ok {
		t.Error("company 7's key opened company 8")
	}
}

func TestOperatorStillNeedsARealID(t *testing.T) {
	m := members()
	s := Scope{Kind: Operator}
	if _, ok := m.Resolve(s, false, 3); !ok {
		t.Error("operator should open any store")
	}
	if _, ok := m.Resolve(s, false, 999); ok {
		t.Error("a missing store should be not found, even for the operator")
	}
}

func TestParseIDAcceptsOnlyPositiveIntegers(t *testing.T) {
	for _, s := range []string{"0", "-1", "abc", "1e3", "1 OR 1=1", "99999999999", ""} {
		if _, ok := ParseID(s); ok {
			t.Errorf("%q was accepted", s)
		}
	}
	if id, ok := ParseID("42"); !ok || id != 42 {
		t.Errorf("42: %d, %v", id, ok)
	}
}

func TestAuthenticate(t *testing.T) {
	k := NewKeys("pepper")
	token := Token(Store, 42, DemoSecret("seed", Store, 42))
	pfx, ok := prefixOf(token)
	if !ok {
		t.Fatalf("token %q has no prefix", token)
	}
	k.byPfx[pfx] = keyRec{hash: k.hash(token), scope: Scope{Kind: Store, ID: 42, Prefix: pfx}}
	if s, ok := k.Authenticate(token); !ok || s.ID != 42 {
		t.Errorf("valid key refused: %v, %v", s, ok)
	}
	if _, ok := k.Authenticate(token[:len(token)-1] + "x"); ok {
		t.Error("a key with the right prefix and wrong secret was accepted")
	}
	if _, ok := k.Authenticate(""); ok {
		t.Error("an empty key was accepted")
	}
	if other := NewKeys("different pepper"); string(other.hash(token)) == string(k.hash(token)) {
		t.Error("the hash ignores the pepper")
	}
}
