package identity

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/scrypt"

	"github.com/s4chdev/voxen/internal/fsutil"
	"github.com/s4chdev/voxen/internal/paths"
)

// File schema constants.
const (
	fileVersion   = 1
	algorithmName = "ed25519"
	kdfName       = "scrypt"
	cipherName    = "xchacha20poly1305"
)

// PassphraseEnv is the environment variable consulted when loading or saving a
// passphrase-protected identity. voxen never prompts on stdin inside the daemon:
// a daemon has no terminal, and a hidden prompt is not auditable.
const PassphraseEnv = "VOXEN_KEY_PASSPHRASE"

// MinPassphraseLength is the minimum accepted passphrase length. Key file
// encryption is a defence-in-depth layer on top of 0600 permissions.
const MinPassphraseLength = 12

// scrypt parameters. N=2^15 keeps unlock latency around 100ms on current
// hardware while remaining portable; these are recorded in the file so future
// parameters can be introduced without breaking old files.
const (
	scryptN      = 1 << 15
	scryptR      = 8
	scryptP      = 1
	scryptKeyLen = 32
)

type keyFile struct {
	Version     int             `json:"version"`
	Algorithm   string          `json:"algorithm"`
	PublicKey   string          `json:"public_key"`
	Fingerprint string          `json:"fingerprint"`
	DisplayName string          `json:"display_name,omitempty"`
	CreatedAt   string          `json:"created_at"`
	PrivateKey  privateKeyBlock `json:"private_key"`
}

type privateKeyBlock struct {
	// Encoding is "plain" or "encrypted". Plain keys rely on filesystem
	// permissions (0600) for protection.
	Encoding string       `json:"encoding"`
	Seed     string       `json:"seed,omitempty"`
	Envelope *keyEnvelope `json:"envelope,omitempty"`
}

type keyEnvelope struct {
	KDF        string `json:"kdf"`
	Cipher     string `json:"cipher"`
	N          int    `json:"n"`
	R          int    `json:"r"`
	P          int    `json:"p"`
	Salt       string `json:"salt"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

// Store manages the on-disk identity at a fixed path.
type Store struct {
	path string
	// passphrase is read from the environment at Load/Save time, never from
	// command line flags (which are visible in the process table).
	passphrase []byte
}

// NewStore returns a store for the identity file at path. An empty passphrase
// disables envelope encryption.
func NewStore(path string, passphrase []byte) *Store {
	return &Store{path: path, passphrase: passphrase}
}

// StoreFromEnv builds a Store using VOXEN_KEY_PASSPHRASE, if set.
func StoreFromEnv(path string) *Store {
	return NewStore(path, []byte(os.Getenv(PassphraseEnv)))
}

// Path returns the identity file path.
func (s *Store) Path() string { return s.path }

// Exists reports whether a key file is present.
func (s *Store) Exists() (bool, error) {
	_, err := os.Stat(s.path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// Load reads and validates the identity. It rejects key files that are readable
// by other users on Unix, a corrupt schema, a public/private mismatch, or a
// fingerprint that does not match the stored public key.
func (s *Store) Load() (*Identity, error) {
	info, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoIdentity
	}
	if err != nil {
		return nil, fmt.Errorf("identity: stat %s: %w", s.path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%w: %s is a directory", ErrCorruptIdentity, s.path)
	}
	if err := paths.CheckPermissions(s.path, 0o600); err != nil {
		return nil, fmt.Errorf("identity: refusing insecure key file: %w", err)
	}
	if info.Size() > 64*1024 {
		return nil, fmt.Errorf("%w: key file is unexpectedly large", ErrCorruptIdentity)
	}

	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("identity: read %s: %w", s.path, err)
	}

	var kf keyFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&kf); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptIdentity, err)
	}

	if kf.Version != fileVersion {
		return nil, fmt.Errorf("%w: unsupported key file version %d", ErrCorruptIdentity, kf.Version)
	}
	if kf.Algorithm != algorithmName {
		return nil, fmt.Errorf("%w: unsupported algorithm %q", ErrCorruptIdentity, kf.Algorithm)
	}

	pub, err := base64.StdEncoding.DecodeString(kf.PublicKey)
	if err != nil || len(pub) != PublicKeySize {
		return nil, fmt.Errorf("%w: invalid public key encoding", ErrCorruptIdentity)
	}

	seed, err := s.decodeSeed(&kf)
	if err != nil {
		return nil, err
	}

	id, err := FromSeed(kf.DisplayName, seed, kf.CreatedAt)
	if err != nil {
		return nil, err
	}
	// The public key in the file must be derived from the private seed.
	if !FingerprintFromPublic(pub).Equal(id.Fingerprint()) {
		return nil, fmt.Errorf("%w: public key does not match private key", ErrCorruptIdentity)
	}
	if kf.Fingerprint != "" {
		want, err := ParseFingerprint(kf.Fingerprint)
		if err != nil || !want.Equal(id.Fingerprint()) {
			return nil, fmt.Errorf("%w: stored fingerprint does not match key", ErrCorruptIdentity)
		}
	}
	return id, nil
}

func (s *Store) decodeSeed(kf *keyFile) ([]byte, error) {
	switch kf.PrivateKey.Encoding {
	case "plain":
		if kf.PrivateKey.Envelope != nil {
			return nil, fmt.Errorf("%w: plain key file contains an envelope", ErrCorruptIdentity)
		}
		seed, err := base64.StdEncoding.DecodeString(kf.PrivateKey.Seed)
		if err != nil || len(seed) != SeedSize {
			return nil, fmt.Errorf("%w: invalid private key encoding", ErrCorruptIdentity)
		}
		return seed, nil

	case "encrypted":
		env := kf.PrivateKey.Envelope
		if env == nil {
			return nil, fmt.Errorf("%w: encrypted key file has no envelope", ErrCorruptIdentity)
		}
		if env.KDF != kdfName || env.Cipher != cipherName {
			return nil, fmt.Errorf("%w: unsupported envelope %q/%q", ErrCorruptIdentity, env.KDF, env.Cipher)
		}
		if len(s.passphrase) == 0 {
			return nil, fmt.Errorf("identity: key is passphrase protected; set %s", PassphraseEnv)
		}
		if len(s.passphrase) < MinPassphraseLength {
			return nil, fmt.Errorf("identity: passphrase must be at least %d characters", MinPassphraseLength)
		}
		salt, err := base64.StdEncoding.DecodeString(env.Salt)
		if err != nil || len(salt) < 16 {
			return nil, fmt.Errorf("%w: invalid salt", ErrCorruptIdentity)
		}
		nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
		if err != nil || len(nonce) != chacha20poly1305.NonceSizeX {
			return nil, fmt.Errorf("%w: invalid nonce", ErrCorruptIdentity)
		}
		ct, err := base64.StdEncoding.DecodeString(env.Ciphertext)
		if err != nil || len(ct) < chacha20poly1305.Overhead {
			return nil, fmt.Errorf("%w: invalid ciphertext", ErrCorruptIdentity)
		}
		key, err := scrypt.Key(s.passphrase, salt, env.N, env.R, env.P, scryptKeyLen)
		if err != nil {
			return nil, fmt.Errorf("identity: derive key: %w", err)
		}
		aead, err := chacha20poly1305.NewX(key)
		if err != nil {
			return nil, fmt.Errorf("identity: init cipher: %w", err)
		}
		seed, err := aead.Open(nil, nonce, ct, s.envelopeAAD(kf))
		if err != nil {
			return nil, fmt.Errorf("identity: cannot decrypt key file (wrong passphrase or corrupt file)")
		}
		if len(seed) != SeedSize {
			return nil, fmt.Errorf("%w: decrypted seed has wrong length", ErrCorruptIdentity)
		}
		return seed, nil

	default:
		return nil, fmt.Errorf("%w: unknown private key encoding %q", ErrCorruptIdentity, kf.PrivateKey.Encoding)
	}
}

// envelopeAAD binds the encrypted seed to the public identity material so an
// attacker with write access to the key file cannot swap keys between files.
func (s *Store) envelopeAAD(kf *keyFile) []byte {
	return []byte("voxen-identity-v1|" + kf.PublicKey + "|" + kf.Fingerprint + "|" + kf.CreatedAt)
}

// Save writes the identity with 0600 permissions. When a passphrase is
// configured the seed is sealed with XChaCha20-Poly1305.
func (s *Store) Save(id *Identity) error {
	if id == nil || len(id.privateKey) != PrivateKeySize {
		return errors.New("identity: refusing to store invalid identity")
	}
	if len(s.passphrase) > 0 && len(s.passphrase) < MinPassphraseLength {
		return fmt.Errorf("identity: passphrase must be at least %d characters", MinPassphraseLength)
	}

	createdAt := id.createdAt
	if createdAt == "" {
		createdAt = time.Now().UTC().Format(time.RFC3339)
	}
	kf := keyFile{
		Version:     fileVersion,
		Algorithm:   algorithmName,
		PublicKey:   id.PublicKeyEncoded(),
		Fingerprint: id.Fingerprint().String(),
		DisplayName: id.DisplayName,
		CreatedAt:   createdAt,
	}

	seed := id.Seed()
	if len(s.passphrase) == 0 {
		kf.PrivateKey = privateKeyBlock{
			Encoding: "plain",
			Seed:     base64.StdEncoding.EncodeToString(seed),
		}
	} else {
		salt := make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return fmt.Errorf("identity: generate salt: %w", err)
		}
		nonce := make([]byte, chacha20poly1305.NonceSizeX)
		if _, err := rand.Read(nonce); err != nil {
			return fmt.Errorf("identity: generate nonce: %w", err)
		}
		key, err := scrypt.Key(s.passphrase, salt, scryptN, scryptR, scryptP, scryptKeyLen)
		if err != nil {
			return fmt.Errorf("identity: derive key: %w", err)
		}
		aead, err := chacha20poly1305.NewX(key)
		if err != nil {
			return fmt.Errorf("identity: init cipher: %w", err)
		}
		ct := aead.Seal(nil, nonce, seed, s.envelopeAAD(&kf))
		kf.PrivateKey = privateKeyBlock{
			Encoding: "encrypted",
			Envelope: &keyEnvelope{
				KDF:        kdfName,
				Cipher:     cipherName,
				N:          scryptN,
				R:          scryptR,
				P:          scryptP,
				Salt:       base64.StdEncoding.EncodeToString(salt),
				Nonce:      base64.StdEncoding.EncodeToString(nonce),
				Ciphertext: base64.StdEncoding.EncodeToString(ct),
			},
		}
	}

	raw, err := json.MarshalIndent(kf, "", "  ")
	if err != nil {
		return fmt.Errorf("identity: encode: %w", err)
	}
	raw = append(raw, '\n')
	if err := fsutil.WriteFileAtomic(s.path, raw, 0o600); err != nil {
		return err
	}
	return nil
}

// Create generates and stores a new identity, refusing to overwrite an existing
// one. Rotating an identity requires an explicit Remove, which is intentional:
// fingerprint continuity is the trust anchor for every peer.
func (s *Store) Create(displayName string) (*Identity, error) {
	exists, err := s.Exists()
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, fmt.Errorf("identity: %s already exists; refusing to overwrite", s.path)
	}
	id, err := New(displayName)
	if err != nil {
		return nil, err
	}
	if err := s.Save(id); err != nil {
		return nil, err
	}
	return id, nil
}

// Remove deletes the key file. It is used by "voxen init --force" only after the
// caller has confirmed the rotation intent and recorded the old fingerprint.
func (s *Store) Remove() error {
	err := os.Remove(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Dir returns the directory holding the key file.
func (s *Store) Dir() string { return filepath.Dir(s.path) }
