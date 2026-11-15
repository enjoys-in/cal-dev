package crypto

import (
	"testing"
)

func TestEncryptDecrypt(t *testing.T) {
	key := DeriveKey("test-secret-key-12345")

	plaintext := "SuperSecretPassword!@#123"
	encrypted, err := Encrypt(plaintext, key)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	if encrypted == plaintext {
		t.Fatal("Encrypted text equals plaintext")
	}

	decrypted, err := Decrypt(encrypted, key)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}

	if decrypted != plaintext {
		t.Fatalf("Expected %q, got %q", plaintext, decrypted)
	}
}

func TestDecryptWithWrongKey(t *testing.T) {
	key1 := DeriveKey("key-1")
	key2 := DeriveKey("key-2")

	encrypted, err := Encrypt("my-password", key1)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	_, err = Decrypt(encrypted, key2)
	if err == nil {
		t.Fatal("Expected decryption with wrong key to fail, but it succeeded")
	}
}

func TestDecryptCorruptData(t *testing.T) {
	key := DeriveKey("test-key")

	_, err := Decrypt("not-valid-base64!@@#", key)
	if err == nil {
		t.Fatal("Expected base64 error")
	}

	_, err = Decrypt("YQ==", key) // "a", too short for nonce
	if err == nil {
		t.Fatal("Expected error for short payload")
	}
}

func TestDecryptWithFallbackLegacyKey(t *testing.T) {
	t.Setenv("CALDEV_SMTP_ENCRYPTION_KEY", "")
	legacy, err := Encrypt("old-password", DeriveKey("jwt-secret"))
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	t.Setenv("CALDEV_SMTP_ENCRYPTION_KEY", "dedicated-key")
	fresh, err := Encrypt("new-password", DeriveKey("jwt-secret"))
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	if got, err := DecryptWithFallback(legacy, "jwt-secret"); err != nil || got != "old-password" {
		t.Fatalf("legacy ciphertext: got %q, %v", got, err)
	}
	if got, err := DecryptWithFallback(fresh, "jwt-secret"); err != nil || got != "new-password" {
		t.Fatalf("env-key ciphertext: got %q, %v", got, err)
	}
	if _, err := DecryptWithFallback(fresh, "other-secret"); err != nil {
		t.Fatalf("env-key ciphertext should not depend on the JWT secret: %v", err)
	}
}
