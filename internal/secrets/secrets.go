// Package secrets encrypts sensitive material (kubeconfigs, join tokens, SSH keys) at
// rest. This is a REAL implementation using AES-256-GCM.
//
// Fidelity note: the key comes from an env var / config, not a KMS or Vault.
// Production would use a managed key service. The crypto here is real; the key
// management is the deliberate shortcut.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"io"
)

type Box struct {
	aead cipher.AEAD
}

// NewBox builds an AES-256-GCM box. key must be exactly 32 bytes.
func NewBox(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, errors.New("secrets: key must be 32 bytes (AES-256)")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext, prefixing the random nonce.
func (b *Box) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Open reverses Seal.
func (b *Box) Open(ciphertext []byte) ([]byte, error) {
	ns := b.aead.NonceSize()
	if len(ciphertext) < ns {
		return nil, errors.New("secrets: ciphertext too short")
	}
	nonce, ct := ciphertext[:ns], ciphertext[ns:]
	return b.aead.Open(nil, nonce, ct, nil)
}
