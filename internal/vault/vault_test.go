package vault_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"

	"github.com/warren-wupeng/agentd/internal/testutil"
	"github.com/warren-wupeng/agentd/internal/vault"
)

var ctx = context.Background()

func TestMain(m *testing.M) { testutil.Main(m) }

func testKey(t *testing.T) []byte {
	t.Helper()
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	return raw
}

func newVault(t *testing.T, key []byte) *vault.Vault {
	t.Helper()
	testutil.NewStore(t) // migrate + truncate (incl. vault_secrets)
	v, err := vault.New(ctx, testutil.DatabaseURL(t), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v.Close() })
	return v
}

func TestSecretRoundtripAndWriteOnlyListing(t *testing.T) {
	v := newVault(t, testKey(t))
	if err := v.PutSecret(ctx, "github", "ghp_supersecret"); err != nil {
		t.Fatal(err)
	}
	got, err := v.GetSecret(ctx, "github")
	if err != nil || got != "ghp_supersecret" {
		t.Fatalf("roundtrip: %q %v", got, err)
	}

	// replace
	if err := v.PutSecret(ctx, "github", "ghp_rotated"); err != nil {
		t.Fatal(err)
	}
	if got, _ := v.GetSecret(ctx, "github"); got != "ghp_rotated" {
		t.Fatalf("replace failed: %q", got)
	}

	metas, err := v.ListSecrets(ctx)
	if err != nil || len(metas) != 1 || metas[0].Name != "github" {
		t.Fatalf("list: %v %+v", err, metas)
	}
	blob, _ := json.Marshal(metas)
	if strings.Contains(string(blob), "ghp_rotated") || strings.Contains(string(blob), "ghp_supersecret") {
		t.Fatal("value leaked through listing")
	}

	if err := v.DeleteSecret(ctx, "github"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.GetSecret(ctx, "github"); err == nil {
		t.Fatal("get after delete must fail")
	}
	if err := v.DeleteSecret(ctx, "github"); err == nil {
		t.Fatal("double delete must 404")
	}
}

// Wrong master key must not decrypt — and the error must not leak the value.
func TestWrongMasterKeyFailsClosed(t *testing.T) {
	v1 := newVault(t, testKey(t))
	if err := v1.PutSecret(ctx, "svc", "value-under-key1"); err != nil {
		t.Fatal(err)
	}
	v2, err := vault.New(ctx, testutil.DatabaseURL(t), testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = v2.Close() }()
	_, err = v2.GetSecret(ctx, "svc")
	if err == nil {
		t.Fatal("decrypt with wrong key must fail")
	}
	if strings.Contains(err.Error(), "value-under-key1") {
		t.Fatal("decryption error leaked the plaintext")
	}
}

func TestSessionTokenDerivation(t *testing.T) {
	key := testKey(t)
	v := newVault(t, key)
	tok := v.SessionToken("sess-1")
	if len(tok) != 64 {
		t.Fatalf("token shape: %d hex chars", len(tok))
	}
	if !v.VerifySessionToken("sess-1", tok) {
		t.Fatal("valid token rejected")
	}
	if v.VerifySessionToken("sess-2", tok) {
		t.Fatal("token accepted for ANOTHER session")
	}
	if v.VerifySessionToken("sess-1", strings.Repeat("0", 64)) {
		t.Fatal("garbage token accepted")
	}
	if !v.VerifySessionToken("sess-1", " "+tok+" ") {
		t.Fatal("whitespace-padded valid token rejected") // documented: trimmed
	}
	// different master key → different token
	v2, _ := vault.New(ctx, testutil.DatabaseURL(t), testKey(t))
	defer func() { _ = v2.Close() }()
	if v2.VerifySessionToken("sess-1", tok) {
		t.Fatal("token from another master key accepted")
	}
}
