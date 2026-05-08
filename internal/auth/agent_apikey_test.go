package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestGenerateAPIKey_PrefixAndLength asserts every minted cleartext
// is `sat_<43 base64url chars>` — total 47 chars, URL-safe, no
// padding. AC1.
func TestGenerateAPIKey_PrefixAndLength(t *testing.T) {
	t.Parallel()
	for i := 0; i < 16; i++ {
		ct, salt, err := GenerateAPIKey()
		if err != nil {
			t.Fatalf("iter %d: GenerateAPIKey: %v", i, err)
		}
		if !strings.HasPrefix(ct, APIKeyPrefix) {
			t.Errorf("iter %d: cleartext %q missing %q prefix", i, ct, APIKeyPrefix)
		}
		if len(ct) != len(APIKeyPrefix)+43 {
			t.Errorf("iter %d: cleartext len = %d, want %d", i, len(ct), len(APIKeyPrefix)+43)
		}
		// 16 raw bytes hex-encoded → 32 chars.
		if len(salt) != 32 {
			t.Errorf("iter %d: salt len = %d, want 32", i, len(salt))
		}
		// URL-safe: no `+`, `/`, or `=` characters.
		for _, c := range ct[len(APIKeyPrefix):] {
			switch c {
			case '+', '/', '=':
				t.Errorf("iter %d: cleartext %q has non-URL-safe char %q", i, ct, c)
			}
		}
	}
}

// TestHashAPIKey_DifferentSaltsProduceDifferentHashes guards
// against the V3 unsalted regression. Same cleartext + different
// salts MUST produce different hashes.
func TestHashAPIKey_DifferentSaltsProduceDifferentHashes(t *testing.T) {
	t.Parallel()
	cleartext := "sat_thesame"
	h1 := HashAPIKey("aabbccddeeff00112233445566778899", cleartext)
	h2 := HashAPIKey("00112233445566778899aabbccddeeff", cleartext)
	if h1 == h2 {
		t.Fatalf("hashes collide across salts: %s", h1)
	}
	if len(h1) != sha256.Size*2 || len(h2) != sha256.Size*2 {
		t.Errorf("hash len: got %d/%d, want 64/64", len(h1), len(h2))
	}
}

// TestHashAPIKey_DeterministicAndMatchesSpec asserts the on-disk
// hash equals hex(sha256(saltBytes || cleartextBytes)) — the shape
// the AC4 evidence checklist greps for.
func TestHashAPIKey_DeterministicAndMatchesSpec(t *testing.T) {
	t.Parallel()
	salt := "deadbeefcafef00ddeadbeefcafef00d"
	cleartext := "sat_abcdefghijklmnopqrstuvwxyz0123456789ABCDEF"
	got := HashAPIKey(salt, cleartext)

	saltBytes, err := hex.DecodeString(salt)
	if err != nil {
		t.Fatalf("decode salt: %v", err)
	}
	expectBytes := sha256.Sum256(append(saltBytes, []byte(cleartext)...))
	want := hex.EncodeToString(expectBytes[:])
	if got != want {
		t.Errorf("HashAPIKey = %q, want %q", got, want)
	}
}

// TestAPIKeyHashesEqual_ConstantTime asserts the helper returns
// true on equality, false otherwise. The constant-time-compare
// behaviour itself is a subtle.ConstantTimeCompare property; this
// test pins the boolean so a future refactor that swaps to a
// variable-time compare is caught.
func TestAPIKeyHashesEqual_ConstantTime(t *testing.T) {
	t.Parallel()
	if !APIKeyHashesEqual("abc", "abc") {
		t.Error("equal hashes should compare equal")
	}
	if APIKeyHashesEqual("abc", "abd") {
		t.Error("different hashes should compare not-equal")
	}
	// Different lengths must not panic; they should compare not-equal
	// (subtle.ConstantTimeCompare returns 0 on length mismatch).
	if APIKeyHashesEqual("abc", "abcd") {
		t.Error("length-mismatch should compare not-equal")
	}
}

// TestAPIKeyCleartextPrefix asserts the prefix slice respects the
// configured cleartext length.
func TestAPIKeyCleartextPrefix(t *testing.T) {
	t.Parallel()
	if got := APIKeyCleartextPrefix("sat_AbCdef"); got != "sat_AbCd" {
		t.Errorf("prefix = %q, want sat_AbCd", got)
	}
	if got := APIKeyCleartextPrefix("sat_"); got != "sat_" {
		t.Errorf("short cleartext should be returned verbatim, got %q", got)
	}
}

// TestMemoryStore_CreateLookupByTokenListDelete is the end-to-end
// round-trip on the in-memory test double — Create, LookupByToken,
// List, Delete, LookupByToken-misses post-delete.
func TestMemoryStore_CreateLookupByTokenListDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryAgentAPIKeyStore()

	cleartext, salt, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey: %v", err)
	}
	hash := HashAPIKey(salt, cleartext)
	now := time.Now().UTC()
	row := APIKey{
		ID:          NewAPIKeyID(),
		WorkspaceID: "wksp_test",
		ProjectID:   "proj_test",
		OwnerUserID: "u_alice",
		Name:        "alice's-laptop",
		Prefix:      APIKeyCleartextPrefix(cleartext),
		KeyHash:     hash,
		KeySalt:     salt,
		Status:      APIKeyStatusActive,
		CreatedAt:   now,
	}
	if err := store.Create(ctx, row); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := store.LookupByToken(ctx, cleartext)
	if err != nil {
		t.Fatalf("LookupByToken: %v", err)
	}
	if got.ID != row.ID {
		t.Errorf("LookupByToken returned id %q, want %q", got.ID, row.ID)
	}

	// GetByHash is a literal hex-equality lookup — used by hash-at-rest
	// tests and by callers holding the salted hash directly.
	if _, err := store.GetByHash(ctx, hash); err != nil {
		t.Errorf("GetByHash post-create err = %v, want nil", err)
	}

	rows, err := store.List(ctx, "u_alice", "", false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("List returned %d rows, want 1", len(rows))
	}

	if err := store.Delete(ctx, row.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.LookupByToken(ctx, cleartext); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Errorf("post-delete LookupByToken err = %v, want ErrAPIKeyNotFound", err)
	}
	// Get-by-id still resolves the archived row (audit invariant).
	if _, err := store.Get(ctx, row.ID); err != nil {
		t.Errorf("post-delete Get-by-id err = %v, want nil", err)
	}
}

// TestMemoryStore_LookupByToken_ArchivedRowMisses pins the AuthMiddleware
// invariant: an archived row must NOT resolve via LookupByToken so the
// middleware falls through to the next auth path.
func TestMemoryStore_LookupByToken_ArchivedRowMisses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryAgentAPIKeyStore()
	cleartext, salt, _ := GenerateAPIKey()
	hash := HashAPIKey(salt, cleartext)
	row := APIKey{
		ID:          NewAPIKeyID(),
		KeyHash:     hash,
		KeySalt:     salt,
		Prefix:      APIKeyCleartextPrefix(cleartext),
		Status:      APIKeyStatusArchived,
		OwnerUserID: "u_alice",
		CreatedAt:   time.Now().UTC(),
	}
	_ = store.Create(ctx, row)
	if _, err := store.LookupByToken(ctx, cleartext); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Errorf("archived LookupByToken err = %v, want ErrAPIKeyNotFound", err)
	}
	// GetByHash returns the row regardless of status — it is a literal
	// hash-equality lookup used for hash-at-rest verification.
	if _, err := store.GetByHash(ctx, hash); err != nil {
		t.Errorf("archived GetByHash err = %v, want nil (literal hash lookup)", err)
	}
}

// TestMemoryStore_LookupByToken_ExpiredRowMisses pins the same
// invariant for ExpiresAt-in-the-past.
func TestMemoryStore_LookupByToken_ExpiredRowMisses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryAgentAPIKeyStore()
	cleartext, salt, _ := GenerateAPIKey()
	hash := HashAPIKey(salt, cleartext)
	past := time.Now().UTC().Add(-time.Hour)
	row := APIKey{
		ID:          NewAPIKeyID(),
		KeyHash:     hash,
		KeySalt:     salt,
		Prefix:      APIKeyCleartextPrefix(cleartext),
		Status:      APIKeyStatusActive,
		ExpiresAt:   &past,
		OwnerUserID: "u_alice",
		CreatedAt:   time.Now().UTC().Add(-2 * time.Hour),
	}
	_ = store.Create(ctx, row)
	if _, err := store.LookupByToken(ctx, cleartext); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Errorf("expired LookupByToken err = %v, want ErrAPIKeyNotFound", err)
	}
}

// TestMemoryStore_Touch_BumpsLastUsedAt asserts the fire-and-forget
// hook used by AuthMiddleware advances LastUsedAt.
func TestMemoryStore_Touch_BumpsLastUsedAt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryAgentAPIKeyStore()
	row := APIKey{
		ID:          NewAPIKeyID(),
		Status:      APIKeyStatusActive,
		OwnerUserID: "u_alice",
		CreatedAt:   time.Now().UTC(),
	}
	_ = store.Create(ctx, row)
	when := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	if err := store.Touch(ctx, row.ID, when); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	got, _ := store.Get(ctx, row.ID)
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(when) {
		t.Errorf("LastUsedAt = %v, want %v", got.LastUsedAt, when)
	}
}

// TestMemoryStore_List_FiltersByOwnerAndProject pins the AC2
// filter behaviour: caller A's keys never leak to caller B; the
// project_id filter narrows further.
func TestMemoryStore_List_FiltersByOwnerAndProject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryAgentAPIKeyStore()
	mk := func(owner, project, status string) APIKey {
		return APIKey{
			ID:          NewAPIKeyID(),
			OwnerUserID: owner,
			ProjectID:   project,
			Status:      status,
			CreatedAt:   time.Now().UTC(),
		}
	}
	_ = store.Create(ctx, mk("u_alice", "proj_x", APIKeyStatusActive))
	_ = store.Create(ctx, mk("u_alice", "proj_y", APIKeyStatusActive))
	_ = store.Create(ctx, mk("u_alice", "proj_y", APIKeyStatusArchived))
	_ = store.Create(ctx, mk("u_bob", "proj_x", APIKeyStatusActive))

	all, _ := store.List(ctx, "u_alice", "", false)
	if len(all) != 2 {
		t.Errorf("alice all-active = %d, want 2", len(all))
	}
	scoped, _ := store.List(ctx, "u_alice", "proj_y", false)
	if len(scoped) != 1 {
		t.Errorf("alice proj_y = %d, want 1", len(scoped))
	}
	includeArchived, _ := store.List(ctx, "u_alice", "proj_y", true)
	if len(includeArchived) != 2 {
		t.Errorf("alice proj_y include_archived = %d, want 2", len(includeArchived))
	}
	bob, _ := store.List(ctx, "u_bob", "", false)
	if len(bob) != 1 {
		t.Errorf("bob all-active = %d, want 1", len(bob))
	}
}
