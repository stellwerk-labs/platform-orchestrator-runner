package gatewayapi

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
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
	token, err := SignAgentToken(privateKey, "my-org", "runner-one", "", now)
	require.NoError(t, err)
	require.NoError(t, VerifyAgentToken(token, publicKey, "my-org", "runner-one", "", now.Add(time.Minute)))
}

func TestAgentTokenRejectsWrongIdentitySignatureAndTime(t *testing.T) {
	privateKey, publicKey := testKeyPair(t)
	_, otherPublicKey := testKeyPair(t)
	now := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	token, err := SignAgentToken(privateKey, "my-org", "runner-one", "", now)
	require.NoError(t, err)

	require.ErrorContains(t, VerifyAgentToken(token, publicKey, "other-org", "runner-one", "", now), "identity")
	require.ErrorContains(t, VerifyAgentToken(token, otherPublicKey, "my-org", "runner-one", "", now), "signature")
	require.ErrorContains(t, VerifyAgentToken(token, publicKey, "my-org", "runner-one", "", now.Add(6*time.Minute)), "expired")
}

func TestNativeTokenRequiresTheResolvedUUIDWithoutDowngrade(t *testing.T) {
	private, public := testKeyPair(t)
	_, other := testKeyPair(t)
	now := time.Now()
	uuid := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	native, err := SignAgentToken(private, "org", "site", uuid, now)
	require.NoError(t, err)
	require.NoError(t, VerifyAgentToken(native, public, "org", "site", uuid, now))
	for _, expected := range []string{"", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"} {
		require.Error(t, VerifyAgentToken(native, public, "org", "site", expected, now))
	}
	legacy, err := SignAgentToken(private, "org", "site", "", now)
	require.NoError(t, err)
	require.Error(t, VerifyAgentToken(legacy, public, "org", "site", uuid, now))
	require.Error(t, VerifyAgentToken(native, other, "org", "site", uuid, now))
	claims, err := base64.RawURLEncoding.DecodeString(strings.Split(legacy, ".")[1])
	require.NoError(t, err)
	require.NotContains(t, string(claims), "runner_uuid")
	for _, invalid := range []string{"00000000-0000-0000-0000-000000000000", strings.ToUpper(uuid), "bad"} {
		_, err = SignAgentToken(private, "org", "site", invalid, now)
		require.Error(t, err)
	}
	_, err = SignAgentToken(private, "org", "0-"+uuid, uuid, now)
	require.Error(t, err)
}
