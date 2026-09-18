package virtualkey

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const ciphertextVersion = "v1"

// Cipher encrypts the recoverable copy of a virtual key kept by the control
// plane for retries. The data-plane copy is encrypted independently by
// Bifrost's own encryption key.
type Cipher struct {
	aead cipher.AEAD
}

func NewCipher(material string) (*Cipher, error) {
	if len(material) < 32 {
		return nil, errors.New("virtualkey: encryption key must contain at least 32 characters")
	}
	key := sha256.Sum256([]byte(material))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("virtualkey: initialize cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("virtualkey: initialize gcm: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

func (c *Cipher) Encrypt(plaintext, associatedData string) (string, error) {
	if c == nil || c.aead == nil {
		return "", errors.New("virtualkey: cipher is not initialized")
	}
	if plaintext == "" {
		return "", errors.New("virtualkey: plaintext is required")
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("virtualkey: generate nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plaintext), []byte(associatedData))
	return ciphertextVersion + ":" + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (c *Cipher) Decrypt(value, associatedData string) (string, error) {
	if c == nil || c.aead == nil {
		return "", errors.New("virtualkey: cipher is not initialized")
	}
	version, encoded, ok := strings.Cut(value, ":")
	if !ok || version != ciphertextVersion {
		return "", errors.New("virtualkey: unsupported ciphertext version")
	}
	sealed, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(sealed) < c.aead.NonceSize() {
		return "", errors.New("virtualkey: invalid ciphertext")
	}
	nonce, ciphertext := sealed[:c.aead.NonceSize()], sealed[c.aead.NonceSize():]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, []byte(associatedData))
	if err != nil {
		return "", errors.New("virtualkey: ciphertext authentication failed")
	}
	return string(plaintext), nil
}

func associatedData(tenantID, keyID string) string {
	return tenantID + "\x00" + keyID
}
