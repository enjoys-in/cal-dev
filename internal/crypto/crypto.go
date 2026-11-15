package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
)

var (
	ErrInvalidCiphertext = errors.New("crypto: invalid ciphertext")
	ErrDecryptionFailed  = errors.New("crypto: decryption failed")
)

// DeriveKey derives a consistent 32-byte AES key from an environment variable
// or falls back to hashing the provided fallback secret (e.g. JWT secret).
func DeriveKey(fallbackSecret string) []byte {
	envKey := os.Getenv("CALDEV_SMTP_ENCRYPTION_KEY")
	if envKey != "" {
		hash := sha256.Sum256([]byte(envKey))
		return hash[:]
	}
	hash := sha256.Sum256([]byte(fallbackSecret))
	return hash[:]
}

// DecryptWithFallback decrypts with DeriveKey(fallbackSecret), i.e. the CALDEV_SMTP_ENCRYPTION_KEY
// key when set. If that fails and the env key is set, it retries with the key derived from
// fallbackSecret alone, so values encrypted before the env key was introduced keep working.
func DecryptWithFallback(cipherB64, fallbackSecret string) (string, error) {
	plaintext, err := Decrypt(cipherB64, DeriveKey(fallbackSecret))
	if err == nil || os.Getenv("CALDEV_SMTP_ENCRYPTION_KEY") == "" {
		return plaintext, err
	}
	legacy := sha256.Sum256([]byte(fallbackSecret))
	if p, legacyErr := Decrypt(cipherB64, legacy[:]); legacyErr == nil {
		return p, nil
	}
	return "", err
}

// Encrypt encrypts plaintext using AES-256-GCM with a 12-byte random nonce.
// Returns base64-encoded [nonce + ciphertext + gcm_tag].
func Encrypt(plaintext string, key []byte) (string, error) {
	if len(key) != 32 {
		hash := sha256.Sum256(key)
		key = hash[:]
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("aes.NewCipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("cipher.NewGCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("rand.ReadFull: %w", err)
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt decrypts a base64-encoded [nonce + ciphertext + gcm_tag] using AES-256-GCM.
func Decrypt(cipherB64 string, key []byte) (string, error) {
	if cipherB64 == "" {
		return "", nil
	}

	if len(key) != 32 {
		hash := sha256.Sum256(key)
		key = hash[:]
	}

	raw, err := base64.StdEncoding.DecodeString(cipherB64)
	if err != nil {
		return "", fmt.Errorf("%w: base64 decode failed", ErrInvalidCiphertext)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("aes.NewCipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("cipher.NewGCM: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(raw) < nonceSize {
		return "", ErrInvalidCiphertext
	}

	nonce, ciphertext := raw[:nonceSize], raw[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrDecryptionFailed, err)
	}

	return string(plaintext), nil
}
