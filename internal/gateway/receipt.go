package gateway

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

type receiptClaims struct {
	OrganizationID string `json:"organization_id"`
	RunnerID       string `json:"runner_id"`
	CommandID      string `json:"command_id"`
	AckSubject     string `json:"ack_subject"`
	StreamSequence uint64 `json:"stream_sequence"`
	ExpiresAt      int64  `json:"expires_at"`
}

type receiptCipher struct {
	aead cipher.AEAD
}

func newReceiptCipher(key []byte) (*receiptCipher, error) {
	if len(key) != 32 {
		return nil, errors.New("gateway receipt key must contain exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &receiptCipher{aead: aead}, nil
}

func (c *receiptCipher) seal(claims receiptClaims) (string, error) {
	plaintext, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nonce, nonce, plaintext, nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (c *receiptCipher) open(receipt string, now time.Time) (receiptClaims, error) {
	sealed, err := base64.RawURLEncoding.DecodeString(receipt)
	if err != nil {
		return receiptClaims{}, errors.New("delivery receipt is not valid base64url")
	}
	if len(sealed) < c.aead.NonceSize() {
		return receiptClaims{}, errors.New("delivery receipt is truncated")
	}
	nonce, ciphertext := sealed[:c.aead.NonceSize()], sealed[c.aead.NonceSize():]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return receiptClaims{}, errors.New("delivery receipt authentication failed")
	}
	var claims receiptClaims
	if err := json.Unmarshal(plaintext, &claims); err != nil {
		return receiptClaims{}, fmt.Errorf("decode delivery receipt: %w", err)
	}
	if claims.OrganizationID == "" || claims.RunnerID == "" || claims.CommandID == "" || claims.AckSubject == "" || claims.StreamSequence == 0 {
		return receiptClaims{}, errors.New("delivery receipt is missing required claims")
	}
	if !now.Before(time.Unix(claims.ExpiresAt, 0)) {
		return receiptClaims{}, errors.New("delivery receipt has expired")
	}
	return claims, nil
}
