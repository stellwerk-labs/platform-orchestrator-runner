package gatewayapi

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testKeyPair(t *testing.T) ([]byte, []byte) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	privateDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	require.NoError(t, err)
	publicDER, err := x509.MarshalPKIXPublicKey(publicKey)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
}

func TestAgentTokenRoundTrip(t *testing.T) {
	privateKey, publicKey := testKeyPair(t)
	now := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	token, err := SignAgentToken(privateKey, "my-org", "runner-one", now)
	require.NoError(t, err)
	require.NoError(t, VerifyAgentToken(token, publicKey, "my-org", "runner-one", now.Add(time.Minute)))
}

func TestAgentTokenRejectsWrongIdentitySignatureAndTime(t *testing.T) {
	privateKey, publicKey := testKeyPair(t)
	_, otherPublicKey := testKeyPair(t)
	now := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	token, err := SignAgentToken(privateKey, "my-org", "runner-one", now)
	require.NoError(t, err)

	require.ErrorContains(t, VerifyAgentToken(token, publicKey, "other-org", "runner-one", now), "identity")
	require.ErrorContains(t, VerifyAgentToken(token, otherPublicKey, "my-org", "runner-one", now), "signature")
	require.ErrorContains(t, VerifyAgentToken(token, publicKey, "my-org", "runner-one", now.Add(6*time.Minute)), "expired")
}
