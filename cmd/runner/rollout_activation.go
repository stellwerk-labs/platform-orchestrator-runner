package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"syscall"

	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rollout"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/rolloutexec"
)

var errRolloutActivation = errors.New("native_activation_refused")
var activationID = regexp.MustCompile(`^[0-9a-f]{32}$`)
var activationHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

type rolloutActivation struct {
	Version           int    `json:"version"`
	ActivationID      string `json:"activation_id"`
	State             string `json:"state"`
	Reconciliation    string `json:"reconciliation"`
	OrgID             string `json:"org_id"`
	RunnerUUID        string `json:"runner_uuid"`
	ApplicationSchema string `json:"application_schema"`
	SystemSchema      string `json:"system_schema"`
	RuntimeVersion    string `json:"runtime_version"`
	DSNSHA256         string `json:"dsn_sha256"`
	IdentitySHA256    string `json:"identity_sha256"`
	CatalogueSHA256   string `json:"catalogue_sha256"`
}

func validateRolloutActivation(cfg rolloutConfig, execution *rolloutexec.Executor) error {
	if cfg.Postgres == nil {
		return nil
	}
	data, err := readActivationInput(cfg.ActivationFile, 4096)
	if err != nil {
		return errRolloutActivation
	}
	grant, err := decodeRolloutActivation(data)
	pg := cfg.Postgres
	if err != nil || grant.Version != 1 || !activationID.MatchString(grant.ActivationID) || grant.State != "active" ||
		(grant.Reconciliation != "fresh" && grant.Reconciliation != "reconciled") ||
		grant.OrgID == "" || grant.OrgID != cfg.OrgID || !rolloutexec.ValidRunnerUUID(grant.RunnerUUID) || grant.RunnerUUID != cfg.RunnerUUID ||
		grant.ApplicationSchema == "" || grant.ApplicationSchema != pg.ApplicationSchema || grant.SystemSchema == "" || grant.SystemSchema != pg.SystemSchema ||
		grant.RuntimeVersion != rollout.NativeRuntimeVersion || !activationHash.MatchString(grant.DSNSHA256) ||
		!activationHash.MatchString(grant.IdentitySHA256) || !activationHash.MatchString(grant.CatalogueSHA256) {
		return errRolloutActivation
	}
	for _, input := range []struct {
		path, expected string
		limit          int64
	}{{pg.DSNFile, grant.DSNSHA256, 8192}, {pg.IdentityFile, grant.IdentitySHA256, 1024}} {
		contents, err := readActivationInput(input.path, input.limit)
		if err != nil {
			return errRolloutActivation
		}
		sum := sha256.Sum256(contents)
		if hex.EncodeToString(sum[:]) != input.expected {
			return errRolloutActivation
		}
	}
	identity, err := rollout.LoadStorageIdentity(pg.IdentityFile)
	if err != nil || execution.PrepareNativeCatalogue(identity) != nil || execution.PrepareNativeBackend() != nil || execution.CatalogDigest() != grant.CatalogueSHA256 {
		return errRolloutActivation
	}
	return nil
}

// Projected Secrets use symlinks. Validate and read the opened regular target,
// preserving the same group-readable private-file policy as DSN/key custody.
func readActivationInput(path string, limit int64) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errRolloutActivation
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > limit || info.Mode().Perm()&0007 != 0 {
		return nil, errRolloutActivation
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errRolloutActivation
	}
	return data, nil
}

func decodeRolloutActivation(data []byte) (rolloutActivation, error) {
	var grant rolloutActivation
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return grant, errRolloutActivation
	}
	seen := map[string]bool{}
	for decoder.More() {
		name, err := decoder.Token()
		key, ok := name.(string)
		if err != nil || !ok || seen[key] {
			return grant, errRolloutActivation
		}
		switch key {
		case "version", "activation_id", "state", "reconciliation", "org_id", "runner_uuid", "application_schema", "system_schema", "runtime_version", "dsn_sha256", "identity_sha256", "catalogue_sha256":
		default:
			return grant, errRolloutActivation
		}
		seen[key] = true
		if decoder.Decode(new(json.RawMessage)) != nil {
			return grant, errRolloutActivation
		}
	}
	if _, err := decoder.Token(); err != nil || decoder.Decode(new(any)) != io.EOF {
		return grant, errRolloutActivation
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&grant) != nil {
		return grant, errRolloutActivation
	}
	return grant, nil
}
