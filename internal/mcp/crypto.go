package mcp

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

type cryptoBox struct {
	aead cipher.AEAD
	key  []byte
}

func newCryptoBox(keyText string) (*cryptoBox, error) {
	keyText = strings.TrimSpace(keyText)
	if keyText == "" {
		return nil, fmt.Errorf("MCP_CONFIG_ENCRYPTION_KEY is required")
	}
	key, err := base64.StdEncoding.DecodeString(keyText)
	if err != nil || len(key) != 32 {
		key = []byte(keyText)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("MCP_CONFIG_ENCRYPTION_KEY must be 32 bytes or base64-encoded 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create MCP config cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create MCP config AEAD: %w", err)
	}
	return &cryptoBox{aead: aead, key: append([]byte(nil), key...)}, nil
}

func (b *cryptoBox) encrypt(plaintext string, aad string) (string, error) {
	if b == nil {
		return "", fmt.Errorf("MCP config crypto is not initialized")
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate MCP config nonce: %w", err)
	}
	ciphertext := b.aead.Seal(nil, nonce, []byte(plaintext), []byte(aad))
	out := append(nonce, ciphertext...)
	return base64.StdEncoding.EncodeToString(out), nil
}

func (b *cryptoBox) decrypt(encoded string, aad string) (string, error) {
	if b == nil {
		return "", fmt.Errorf("MCP config crypto is not initialized")
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", fmt.Errorf("decode MCP config ciphertext: %w", err)
	}
	if len(data) <= b.aead.NonceSize() {
		return "", fmt.Errorf("MCP config ciphertext is too short")
	}
	nonce := data[:b.aead.NonceSize()]
	ciphertext := data[b.aead.NonceSize():]
	plaintext, err := b.aead.Open(nil, nonce, ciphertext, []byte(aad))
	if err != nil {
		return "", fmt.Errorf("decrypt MCP config field: %w", err)
	}
	return string(plaintext), nil
}

func (b *cryptoBox) hostHash(host string) string {
	mac := hmac.New(sha256.New, b.key)
	mac.Write([]byte(strings.ToLower(strings.TrimSpace(host))))
	return hex.EncodeToString(mac.Sum(nil))
}

func fieldAAD(scope, ownerUserID, name, field string) string {
	return "mcp_server:" + scope + ":" + ownerUserID + ":" + name + ":" + field
}
