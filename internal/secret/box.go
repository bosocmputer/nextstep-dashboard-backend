package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"io"
)

type Box struct {
	aead    cipher.AEAD
	keyID   string
	entropy io.Reader
}

type Sealed struct {
	KeyID      string
	Nonce      []byte
	Ciphertext []byte
}

func NewBox(key []byte, keyID string, entropy io.Reader) (*Box, error) {
	if len(key) != 32 {
		return nil, errors.New("encryption master key must contain exactly 32 bytes")
	}
	if keyID == "" || entropy == nil {
		return nil, errors.New("encryption key ID and entropy source are required")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("create AES cipher")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("create AES-GCM cipher")
	}
	return &Box{aead: aead, keyID: keyID, entropy: entropy}, nil
}

func (box *Box) Encrypt(plaintext, additionalData []byte) (Sealed, error) {
	nonce := make([]byte, box.aead.NonceSize())
	if _, err := io.ReadFull(box.entropy, nonce); err != nil {
		return Sealed{}, errors.New("generate encryption nonce")
	}
	ciphertext := box.aead.Seal(nil, nonce, plaintext, additionalData)
	return Sealed{KeyID: box.keyID, Nonce: nonce, Ciphertext: ciphertext}, nil
}

func (box *Box) Decrypt(sealed Sealed, additionalData []byte) ([]byte, error) {
	if sealed.KeyID != box.keyID {
		return nil, errors.New("ciphertext uses an unavailable encryption key")
	}
	if len(sealed.Nonce) != box.aead.NonceSize() {
		return nil, errors.New("ciphertext nonce is invalid")
	}
	plaintext, err := box.aead.Open(nil, sealed.Nonce, sealed.Ciphertext, additionalData)
	if err != nil {
		return nil, errors.New("ciphertext authentication failed")
	}
	return plaintext, nil
}
