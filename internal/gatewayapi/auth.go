package gatewayapi

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	agentTokenLifetime = 5 * time.Minute
	clockSkew          = 30 * time.Second
)

type agentTokenHeader struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
	KeyID     string `json:"kid"`
}

type agentTokenClaims struct {
	Audience       string `json:"aud"`
	OrganizationID string `json:"org_id"`
	RunnerID       string `json:"runner_id"`
	IssuedAt       int64  `json:"iat"`
	ExpiresAt      int64  `json:"exp"`
}

func SignAgentToken(privateKeyPEM []byte, organizationID, runnerID string, now time.Time) (string, error) {
	privateKey, err := parsePrivateKey(privateKeyPEM)
	if err != nil {
		return "", err
	}
	if organizationID == "" || runnerID == "" {
		return "", errors.New("organization and runner IDs are required")
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	fingerprint := sha256.Sum256(publicKey)
	header, err := json.Marshal(agentTokenHeader{
		Algorithm: "EdDSA",
		Type:      "JWT",
		KeyID:     hex.EncodeToString(fingerprint[:]),
	})
	if err != nil {
		return "", err
	}
	issuedAt := now.UTC().Truncate(time.Second)
	claims, err := json.Marshal(agentTokenClaims{
		Audience: AuthorizationAudience, OrganizationID: organizationID, RunnerID: runnerID,
		IssuedAt: issuedAt.Unix(), ExpiresAt: issuedAt.Add(agentTokenLifetime).Unix(),
	})
	if err != nil {
		return "", err
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	signature := ed25519.Sign(privateKey, []byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func VerifyAgentToken(token string, publicKeyPEM []byte, organizationID, runnerID string, now time.Time) error {
	publicKey, err := parsePublicKey(publicKeyPEM)
	if err != nil {
		return err
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("token must contain three segments")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return errors.New("token header is not valid base64url")
	}
	var header agentTokenHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return errors.New("token header is not valid JSON")
	}
	if header.Algorithm != "EdDSA" || header.Type != "JWT" {
		return errors.New("token must use EdDSA JWT signing")
	}
	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return errors.New("token claims are not valid base64url")
	}
	var claims agentTokenClaims
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		return errors.New("token claims are not valid JSON")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return errors.New("token signature is not valid base64url")
	}
	if !ed25519.Verify(publicKey, []byte(parts[0]+"."+parts[1]), signature) {
		return errors.New("token signature is invalid")
	}
	if claims.Audience != AuthorizationAudience || claims.OrganizationID != organizationID || claims.RunnerID != runnerID {
		return errors.New("token identity or audience does not match the request")
	}
	issuedAt := time.Unix(claims.IssuedAt, 0)
	expiresAt := time.Unix(claims.ExpiresAt, 0)
	if expiresAt.Sub(issuedAt) != agentTokenLifetime {
		return errors.New("token lifetime is invalid")
	}
	if now.Add(clockSkew).Before(issuedAt) {
		return errors.New("token is not valid yet")
	}
	if !now.Add(-clockSkew).Before(expiresAt) {
		return errors.New("token has expired")
	}
	return nil
}

func parsePrivateKey(privateKeyPEM []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(privateKeyPEM)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("private key is not a PKCS#8 PEM private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not Ed25519")
	}
	return privateKey, nil
}

func parsePublicKey(publicKeyPEM []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode(publicKeyPEM)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("public key is not a PKIX PEM public key")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	publicKey, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("public key is not Ed25519")
	}
	return publicKey, nil
}
