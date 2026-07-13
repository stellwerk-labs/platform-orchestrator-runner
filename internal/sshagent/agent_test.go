package sshagent

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh/agent"
)

func TestParseSshKeys_empty(t *testing.T) {
	k, err := parseSshKeys("")
	require.NoError(t, err)
	require.Empty(t, k)
}

func TestParseSshKeys_invalid(t *testing.T) {
	k, err := parseSshKeys("hello")
	require.EqualError(t, err, "failed to parse key 0: invalid PEM")
	require.Empty(t, k)
}

func TestParseSshKeys_valid(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pemA := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	})
	privateKey, err = rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)
	derBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	require.NoError(t, err)
	pemB := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: derBytes,
	})
	k, err := parseSshKeys(string(pemA) + "\n" + string(pemB))
	require.NoError(t, err)
	require.Len(t, k, 2)
}

func TestLaunchAgent(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pemA := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	})
	t.Setenv(sshKeysVar, string(pemA))
	t.Setenv(authSockVar, "")

	tf := filepath.Join(t.TempDir(), "agent.sock")

	fin, err := Launch(tf)
	require.NoError(t, err)
	defer fin()

	assert.Equal(t, tf, os.Getenv(authSockVar))
	assert.Equal(t, defaultGitSshCommand, os.Getenv(gitSshCommand))

	var a agent.Agent
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		if d, err := net.Dial("unix", tf); assert.NoError(collect, err) && assert.NotEmpty(collect, d) {
			a = agent.NewClient(d)
		}
	}, 10*time.Second, time.Second)

	keys, err := a.List()
	require.NoError(t, err)
	require.Len(t, keys, 1)
}
