package identity

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFingerprintFormatAndParse(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	fp := FingerprintFromPublic(pub)
	s := fp.String()
	if len(strings.Split(s, " ")) != 16 {
		t.Fatalf("fingerprint should be 16 groups of 4: %q", s)
	}
	parsed, err := ParseFingerprint(s)
	if err != nil {
		t.Fatalf("parse display form: %v", err)
	}
	if !parsed.Equal(fp) {
		t.Fatal("parsed fingerprint differs")
	}
	// Separators and case are flexible.
	loose, err := ParseFingerprint(strings.ToLower(strings.ReplaceAll(s, " ", ":")))
	if err != nil {
		t.Fatal(err)
	}
	if !loose.Equal(fp) {
		t.Fatal("loose parse differs")
	}
	if _, err := ParseFingerprint("abcd"); err == nil {
		t.Fatal("short fingerprint must be rejected")
	}
	if _, err := ParseFingerprint(strings.Repeat("z", 64)); err == nil {
		t.Fatal("non-hex fingerprint must be rejected")
	}
}

func TestNewSignVerify(t *testing.T) {
	id, err := New("alice")
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("hello")
	sig := id.Sign(msg)
	if !Verify(id.PublicKey, msg, sig) {
		t.Fatal("valid signature rejected")
	}
	if Verify(id.PublicKey, []byte("hello!"), sig) {
		t.Fatal("signature accepted for modified message")
	}
	if Verify(id.PublicKey, msg, sig[:len(sig)-1]) {
		t.Fatal("truncated signature accepted")
	}
}

func TestStorePlainRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "identity.json")
	store := NewStore(path, nil)

	if exists, _ := store.Exists(); exists {
		t.Fatal("store should start empty")
	}
	id, err := store.Create("alice")
	if err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no POSIX mode bits; identity protection there rests on the
	// per-user profile ACL, which paths.CheckPermissions treats as out of scope.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %04o, want 0600", info.Mode().Perm())
	}

	loaded, err := NewStore(path, nil).Load()
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Fingerprint().Equal(id.Fingerprint()) {
		t.Fatal("fingerprint changed across reload")
	}
	if loaded.DisplayName != "alice" {
		t.Fatalf("display name = %q", loaded.DisplayName)
	}
}

func TestCreateRefusesOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	store := NewStore(path, nil)
	if _, err := store.Create("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create("bob"); err == nil {
		t.Fatal("second Create must fail instead of rotating silently")
	}
}

func TestStoreRejectsLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits do not apply; the per-user profile ACL is the boundary")
	}
	path := filepath.Join(t.TempDir(), "identity.json")
	store := NewStore(path, nil)
	if _, err := store.Create("alice"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(path, nil).Load(); err == nil {
		t.Fatal("world-readable key file must be refused")
	} else if !strings.Contains(err.Error(), "insecure") {
		t.Fatalf("error should explain the permission problem: %v", err)
	}
}

func TestStoreEncryptedRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	pass := []byte("correct horse battery staple")
	store := NewStore(path, pass)
	id, err := store.Create("carol")
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var kf keyFile
	if err := json.Unmarshal(raw, &kf); err != nil {
		t.Fatal(err)
	}
	if kf.PrivateKey.Encoding != "encrypted" || kf.PrivateKey.Envelope == nil {
		t.Fatal("private key was not encrypted")
	}
	if strings.Contains(string(raw), kf.PrivateKey.Seed) && kf.PrivateKey.Seed != "" {
		t.Fatal("plaintext seed leaked into encrypted file")
	}

	loaded, err := NewStore(path, pass).Load()
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !loaded.Fingerprint().Equal(id.Fingerprint()) {
		t.Fatal("fingerprint mismatch after decrypt")
	}

	if _, err := NewStore(path, []byte("wrong passphrase here")).Load(); err == nil {
		t.Fatal("wrong passphrase must fail")
	}
	if _, err := NewStore(path, nil).Load(); err == nil {
		t.Fatal("missing passphrase must fail")
	}
}

func TestStoreRejectsShortPassphrase(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "identity.json"), []byte("short"))
	if _, err := store.Create("dave"); err == nil {
		t.Fatal("short passphrase must be rejected")
	}
}

func TestStoreDetectsTamperedKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	id, err := NewStore(path, nil).Create("erin")
	if err != nil {
		t.Fatal(err)
	}
	other, err := New("attacker")
	if err != nil {
		t.Fatal(err)
	}

	// Swap in a different public key while keeping the original private seed.
	raw, _ := os.ReadFile(path)
	var kf keyFile
	_ = json.Unmarshal(raw, &kf)
	kf.PublicKey = other.PublicKeyEncoded()
	kf.Fingerprint = other.Fingerprint().String()
	out, _ := json.MarshalIndent(kf, "", "  ")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := NewStore(path, nil).Load(); err == nil {
		t.Fatal("mismatched public/private key pair must be rejected")
	}
	_ = id
}

func TestStoreNoIdentity(t *testing.T) {
	_, err := NewStore(filepath.Join(t.TempDir(), "missing.json"), nil).Load()
	if !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("expected ErrNoIdentity, got %v", err)
	}
}

func TestTrustStoreLifecycle(t *testing.T) {
	dir := t.TempDir()
	ts, err := OpenTrust(dir)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(nil)

	entry, err := ts.Observe(pub, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Level != TrustUnverified {
		t.Fatalf("observed key level = %q, want unverified", entry.Level)
	}
	if ts.IsVerified(entry.Fingerprint) {
		t.Fatal("observed key must not be verified")
	}

	ver, err := ts.VerifyKey(pub, "bob", "compared in person")
	if err != nil {
		t.Fatal(err)
	}
	if ver.Level != TrustVerified || ver.VerifiedAt == nil {
		t.Fatal("verify did not promote the key")
	}

	// Reload from disk to prove the decision is durable.
	reloaded, err := OpenTrust(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.IsVerified(entry.Fingerprint) {
		t.Fatal("verification was not persisted")
	}

	if err := reloaded.Revoke(entry.Fingerprint); err != nil {
		t.Fatal(err)
	}
	third, err := OpenTrust(dir)
	if err != nil {
		t.Fatal(err)
	}
	if third.IsVerified(entry.Fingerprint) || third.Level(entry.Fingerprint) != TrustRevoked {
		t.Fatal("revocation was not persisted")
	}

	info, err := os.Stat(third.Path())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("trust store mode = %04o, want 0600", info.Mode().Perm())
	}
}

func TestTrustStoreRejectsLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits do not apply; the per-user profile ACL is the boundary")
	}
	dir := t.TempDir()
	ts, err := OpenTrust(dir)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(nil)
	if _, err := ts.VerifyKey(pub, "bob", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ts.Path(), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenTrust(dir); err == nil {
		t.Fatal("insecure trust store must be refused")
	}
}

func TestTrustStoreCorruptIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, trustFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenTrust(dir); err == nil {
		t.Fatal("corrupt trust store must be an error, not an empty store")
	}
}

// TestFingerprintPinThenBind covers the real-world flow: the peer reads their
// fingerprint, the user records it before any call, and the key presented later
// must hash to that value.
func TestFingerprintPinThenBind(t *testing.T) {
	dir := t.TempDir()
	ts, err := OpenTrust(dir)
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _ := ed25519.GenerateKey(nil)
	fp := FingerprintFromPublic(pub)

	entry, err := ts.VerifyFingerprint(fp, "bob", "read over the phone")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Level != TrustVerified || !entry.Pinned {
		t.Fatalf("pin should be verified and pinned: %+v", entry)
	}
	if entry.PublicKey != "" {
		t.Fatal("a fingerprint-only pin must not fabricate a public key")
	}
	if !ts.IsVerified(fp) {
		t.Fatal("pinned fingerprint should be verified")
	}

	// A different key with the same display name cannot take the pin over.
	otherPub, _, _ := ed25519.GenerateKey(nil)
	if _, err := ts.BindKey(otherPub); err == nil {
		t.Fatal("an unrelated key was bound to a pinned fingerprint")
	}

	// The real key binds and the pin is completed.
	bound, err := ts.BindKey(pub)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if bound.Pinned {
		t.Fatal("pin should be resolved after binding")
	}
	if bound.PublicKey == "" {
		t.Fatal("bound entry should carry the public key")
	}

	// The completion persists.
	reloaded, err := OpenTrust(dir)
	if err != nil {
		t.Fatal(err)
	}
	again, ok := reloaded.Lookup(fp)
	if !ok || again.Pinned || again.PublicKey == "" {
		t.Fatalf("binding was not persisted: %+v", again)
	}
	if !reloaded.IsVerified(fp) {
		t.Fatal("verified state lost on reload")
	}
}

func TestRevokedPinCannotBeBound(t *testing.T) {
	dir := t.TempDir()
	ts, _ := OpenTrust(dir)
	pub, _, _ := ed25519.GenerateKey(nil)
	fp := FingerprintFromPublic(pub)

	if _, err := ts.VerifyFingerprint(fp, "bob", ""); err != nil {
		t.Fatal(err)
	}
	if err := ts.Revoke(fp); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.BindKey(pub); err == nil {
		t.Fatal("revoked fingerprint was bound")
	}
	// Observing the key must not clear a revocation.
	if _, err := ts.Observe(pub, "bob"); err != nil {
		t.Fatal(err)
	}
	if ts.Level(fp) != TrustRevoked {
		t.Fatalf("observation downgraded a revocation to %q", ts.Level(fp))
	}
}

func TestObserveDoesNotDowngradeOrRebind(t *testing.T) {
	dir := t.TempDir()
	ts, _ := OpenTrust(dir)
	pub, _, _ := ed25519.GenerateKey(nil)
	fp := FingerprintFromPublic(pub)

	if _, err := ts.VerifyKey(pub, "bob", "in person"); err != nil {
		t.Fatal(err)
	}
	// A later observation with a different display name must not lose trust.
	entry, err := ts.Observe(pub, "bob-renamed")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Level != TrustVerified {
		t.Fatalf("observation downgraded trust to %q", entry.Level)
	}
	if !ts.IsVerified(fp) {
		t.Fatal("verified state lost after observation")
	}
}

func TestTrustStoreRejectsFingerprintKeyMismatch(t *testing.T) {
	dir := t.TempDir()
	ts, _ := OpenTrust(dir)
	pub, _, _ := ed25519.GenerateKey(nil)
	other, _, _ := ed25519.GenerateKey(nil)
	if _, err := ts.VerifyKey(pub, "bob", ""); err != nil {
		t.Fatal(err)
	}

	// Hand-edit the store so the key no longer matches the anchor fingerprint.
	raw, err := os.ReadFile(ts.Path())
	if err != nil {
		t.Fatal(err)
	}
	var tf trustFile
	if err := json.Unmarshal(raw, &tf); err != nil {
		t.Fatal(err)
	}
	if len(tf.Peers) != 1 {
		t.Fatalf("expected one peer, got %d", len(tf.Peers))
	}
	tf.Peers[0].PublicKey = base64.StdEncoding.EncodeToString(other)
	out, _ := json.MarshalIndent(tf, "", "  ")
	if err := os.WriteFile(ts.Path(), out, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenTrust(dir); err == nil {
		t.Fatal("a key that does not hash to its anchor fingerprint was accepted")
	}
}

func TestPrivateKeyIsNotLoggable(t *testing.T) {
	// Guard against a future change that adds a LogValue leaking key material.
	id, err := New("frank")
	if err != nil {
		t.Fatal(err)
	}
	var v any = id
	if _, ok := v.(interface{ LogValue() any }); ok {
		t.Fatal("Identity must not implement LogValue")
	}
}
