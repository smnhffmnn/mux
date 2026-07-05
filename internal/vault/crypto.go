package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"

	"golang.org/x/crypto/argon2"
)

const (
	keySize  = 32 // AES-256
	saltSize = 32

	// Argon2id parameters (OWASP recommended)
	argon2Time    = 3
	argon2Memory  = 64 * 1024 // 64 MB
	argon2Threads = 4
)

// deriveKey derives an AES-256 key from a passphrase using Argon2id.
func deriveKey(passphrase string, salt []byte) []byte {
	return argon2.IDKey([]byte(passphrase), salt, argon2Time, argon2Memory, argon2Threads, keySize)
}

// generateKey creates a cryptographically random 256-bit key.
func generateKey() ([]byte, error) {
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	return key, nil
}

// generateSalt creates a cryptographically random salt.
func generateSalt() ([]byte, error) {
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generate salt: %w", err)
	}
	return salt, nil
}

// encrypt encrypts plaintext using AES-256-GCM. Returns nonce+ciphertext.
func encrypt(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}

	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// decrypt decrypts data produced by encrypt (nonce prepended to ciphertext).
func decrypt(key, data []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}

	if len(data) < gcm.NonceSize() {
		return nil, fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}

	return plaintext, nil
}

// wipeBytes overwrites a byte slice with zeros.
// Uses a write-then-read pattern to discourage compiler optimization.
// Note: Go does not guarantee secure memory erasure — the GC may have
// copied the data elsewhere. This is a best-effort defense-in-depth measure.
func wipeBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
	// Prevent the compiler from optimizing away the wipe by "using" the slice.
	// runtime.KeepAlive is not sufficient for this; a volatile-like read is better.
	if len(b) > 0 && b[0] != 0 {
		panic("wipeBytes: unreachable")
	}
}
