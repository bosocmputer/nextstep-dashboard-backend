package secret

import (
	"bytes"
	"testing"
)

func TestBoxEncryptsWithUniqueNonceAndTenantBoundAAD(t *testing.T) {
	box, err := NewBox(bytes.Repeat([]byte{0x11}, 32), "key-2026-01", bytes.NewReader(append(bytes.Repeat([]byte{0x22}, 12), bytes.Repeat([]byte{0x23}, 12)...)))
	if err != nil {
		t.Fatalf("NewBox() error = %v", err)
	}
	first, err := box.Encrypt([]byte("secret-value"), []byte("tenant-a:password"))
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	second, err := box.Encrypt([]byte("secret-value"), []byte("tenant-a:password"))
	if err != nil {
		t.Fatalf("Encrypt() second error = %v", err)
	}
	if bytes.Equal(first.Ciphertext, []byte("secret-value")) || bytes.Equal(first.Ciphertext, second.Ciphertext) {
		t.Fatal("ciphertext exposed plaintext or reused a nonce")
	}
	plaintext, err := box.Decrypt(first, []byte("tenant-a:password"))
	if err != nil || string(plaintext) != "secret-value" {
		t.Fatalf("Decrypt() = %q, %v", plaintext, err)
	}
	if _, err := box.Decrypt(first, []byte("tenant-b:password")); err == nil {
		t.Fatal("ciphertext decrypted under a different tenant AAD")
	}
}

func TestBoxRejectsInvalidKeyAndTampering(t *testing.T) {
	if _, err := NewBox([]byte("short"), "key", bytes.NewReader(nil)); err == nil {
		t.Fatal("short AES key was accepted")
	}
	box, _ := NewBox(bytes.Repeat([]byte{1}, 32), "key", bytes.NewReader(bytes.Repeat([]byte{2}, 12)))
	sealed, err := box.Encrypt([]byte("secret"), []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	sealed.Ciphertext[0] ^= 0xff
	if _, err := box.Decrypt(sealed, []byte("aad")); err == nil {
		t.Fatal("tampered ciphertext decrypted")
	}
}
