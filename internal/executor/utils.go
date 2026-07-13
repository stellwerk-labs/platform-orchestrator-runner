package executor

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/platformorchestratorapi"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/runner"

	"github.com/golang-jwt/jwt/v4"
	"github.com/pkg/errors"
)

func fromIaCChangesToTFResourceCounts(changes *runner.IaCChanges) platformorchestratorapi.DeploymentTFResourceCounts {
	if changes == nil {
		return platformorchestratorapi.DeploymentTFResourceCounts{}
	} else {
		return platformorchestratorapi.DeploymentTFResourceCounts{
			NumResourcesAdded:   changes.Added,
			NumResourcesChanged: changes.Change,
			NumResourcesRemoved: changes.Remove,
		}
	}
}

func signJWT(privateKeyPEM []byte, orgID, runnerID string) (string, error) {
	block, _ := pem.Decode(privateKeyPEM)
	if block == nil || block.Type != "PRIVATE KEY" {
		return "", errors.New("failed to decode PEM block or block is not a private key")
	}

	parsedKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", errors.Wrap(err, "failed to parse private key from PEM")
	}

	privateKey, ok := parsedKey.(ed25519.PrivateKey)
	if !ok {
		return "", errors.New("parsed key is not an Ed25519 private key")
	}
	publicKey := ed25519.PublicKey(privateKey[32:])
	hash := sha256.Sum256(publicKey)

	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims(map[string]interface{}{
		"org_id":    orgID,
		"runner_id": runnerID,
		"alg":       "EdDSA",
		"typ":       "JWT",
		"kid":       hex.EncodeToString(hash[:]),
	}))
	if signedToken, err := token.SignedString(ed25519.PrivateKey(privateKey)); err != nil {
		return "", errors.Wrap(err, "failed to sign JWT token")
	} else {
		return signedToken, nil
	}
}
