package secrets_test

import (
	"crypto/x509"
	"encoding/pem"
	"testing"

	secpkg "github.com/folsomintel/fuse/internal/secrets"
)

func TestGenerateVMCredentials(t *testing.T) {
	creds, err := secpkg.GenerateVMCredentials("test-vm-001")
	if err != nil {
		t.Fatalf("secpkg.GenerateVMCredentials: %v", err)
	}

	// Token should be 64 hex characters (32 bytes).
	if len(creds.AuthToken) != 64 {
		t.Errorf("token length = %d, want 64", len(creds.AuthToken))
	}

	// Cert should be valid PEM.
	block, _ := pem.Decode(creds.CertPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatal("failed to decode certificate PEM")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	if cert.Subject.CommonName != "test-vm-001" {
		t.Errorf("CN = %q, want %q", cert.Subject.CommonName, "test-vm-001")
	}
	if len(cert.IPAddresses) != 2 {
		t.Errorf("SAN IPs = %d, want 2", len(cert.IPAddresses))
	}

	// Key should be valid PEM.
	keyBlock, _ := pem.Decode(creds.KeyPEM)
	if keyBlock == nil || keyBlock.Type != "EC PRIVATE KEY" {
		t.Fatal("failed to decode key PEM")
	}

	// Each call should produce unique tokens.
	creds2, err := secpkg.GenerateVMCredentials("test-vm-002")
	if err != nil {
		t.Fatalf("second secpkg.GenerateVMCredentials: %v", err)
	}
	if creds.AuthToken == creds2.AuthToken {
		t.Error("two calls produced the same token")
	}
}

func TestEncryptDecryptToken(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}

	original := "my-secret-token-value"
	ciphertext, err := secpkg.EncryptToken(original, key)
	if err != nil {
		t.Fatalf("secpkg.EncryptToken: %v", err)
	}

	decrypted, err := secpkg.DecryptToken(ciphertext, key)
	if err != nil {
		t.Fatalf("secpkg.DecryptToken: %v", err)
	}
	if decrypted != original {
		t.Errorf("decrypted = %q, want %q", decrypted, original)
	}
}

func TestEncryptTokenWrongKeyLength(t *testing.T) {
	_, err := secpkg.EncryptToken("token", make([]byte, 16))
	if err == nil {
		t.Error("expected error for 16-byte key")
	}
}

func TestDecryptTokenWrongKey(t *testing.T) {
	key1 := make([]byte, 32)
	key2 := make([]byte, 32)
	key2[0] = 0xFF

	ciphertext, err := secpkg.EncryptToken("token", key1)
	if err != nil {
		t.Fatalf("secpkg.EncryptToken: %v", err)
	}

	_, err = secpkg.DecryptToken(ciphertext, key2)
	if err == nil {
		t.Error("expected error decrypting with wrong key")
	}
}

func TestDecryptTokenTamperedCiphertext(t *testing.T) {
	key := make([]byte, 32)
	ciphertext, err := secpkg.EncryptToken("token", key)
	if err != nil {
		t.Fatalf("secpkg.EncryptToken: %v", err)
	}

	// Flip a byte in the ciphertext (after the nonce).
	ciphertext[len(ciphertext)-1] ^= 0xFF

	_, err = secpkg.DecryptToken(ciphertext, key)
	if err == nil {
		t.Error("expected error for tampered ciphertext")
	}
}

func TestDecryptTokenTooShort(t *testing.T) {
	key := make([]byte, 32)
	_, err := secpkg.DecryptToken([]byte("short"), key)
	if err == nil {
		t.Error("expected error for short ciphertext")
	}
}
