package gatewayapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type outboxOperation struct {
	Kind            string          `json:"kind"`
	OrganizationID  string          `json:"organization_id"`
	RunnerID        string          `json:"runner_id"`
	DeploymentID    string          `json:"deployment_id"`
	EnvironmentID   string          `json:"environment_id,omitempty"`
	DeploymentToken string          `json:"deployment_token"`
	Content         json.RawMessage `json:"content"`
}

type Outbox struct {
	directory string
	client    *Client
	mu        sync.Mutex
}

func NewOutbox(directory string, client *Client) (*Outbox, error) {
	if strings.TrimSpace(directory) == "" || client == nil {
		return nil, errors.New("outbox directory and gateway client are required")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create gateway outbox: %w", err)
	}
	return &Outbox{directory: directory, client: client}, nil
}

func (o *Outbox) PostResults(ctx context.Context, deploymentID, deploymentToken string, results any) error {
	if err := o.client.PostResults(ctx, deploymentID, deploymentToken, results); err == nil {
		return nil
	}
	content, err := json.Marshal(results)
	if err != nil {
		return err
	}
	return o.store(outboxOperation{
		Kind: "result", OrganizationID: o.client.organizationID, RunnerID: o.client.runnerID,
		DeploymentID: deploymentID, DeploymentToken: deploymentToken, Content: content,
	})
}

func (o *Outbox) PutLogs(ctx context.Context, environmentID, deploymentID, deploymentToken, encryptedLogs string) error {
	if err := o.client.PutLogs(ctx, environmentID, deploymentID, deploymentToken, strings.NewReader(encryptedLogs)); err == nil {
		return nil
	}
	content, err := json.Marshal(encryptedLogs)
	if err != nil {
		return err
	}
	return o.store(outboxOperation{
		Kind: "logs", OrganizationID: o.client.organizationID, RunnerID: o.client.runnerID,
		DeploymentID: deploymentID, EnvironmentID: environmentID,
		DeploymentToken: deploymentToken, Content: content,
	})
}

func (o *Outbox) Flush(ctx context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	entries, err := os.ReadDir(o.directory)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Name() < entries[right].Name() })
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		operationPath := filepath.Join(o.directory, entry.Name())
		contents, err := os.ReadFile(operationPath)
		if err != nil {
			return err
		}
		var operation outboxOperation
		if err := json.Unmarshal(contents, &operation); err != nil {
			return fmt.Errorf("decode gateway outbox operation %s: %w", entry.Name(), err)
		}
		if operation.OrganizationID != o.client.organizationID || operation.RunnerID != o.client.runnerID {
			return fmt.Errorf("gateway outbox operation %s belongs to another runner", entry.Name())
		}
		switch operation.Kind {
		case "result":
			if err := o.client.PostResults(ctx, operation.DeploymentID, operation.DeploymentToken, operation.Content); err != nil {
				return err
			}
		case "logs":
			var encryptedLogs string
			if err := json.Unmarshal(operation.Content, &encryptedLogs); err != nil {
				return fmt.Errorf("decode spooled encrypted logs: %w", err)
			}
			if err := o.client.PutLogs(ctx, operation.EnvironmentID, operation.DeploymentID, operation.DeploymentToken, strings.NewReader(encryptedLogs)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("gateway outbox operation %s has unknown kind %q", entry.Name(), operation.Kind)
		}
		if err := os.Remove(operationPath); err != nil {
			return err
		}
		if err := syncOutboxDirectory(o.directory); err != nil {
			return err
		}
	}
	return nil
}

func (o *Outbox) store(operation outboxOperation) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	contents, err := json.Marshal(operation)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(operation.Kind + "\x00" + operation.OrganizationID + "\x00" + operation.RunnerID + "\x00" + operation.DeploymentID))
	name := hex.EncodeToString(digest[:]) + ".json"
	temporary, err := os.CreateTemp(o.directory, ".pending-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, filepath.Join(o.directory, name)); err != nil {
		return err
	}
	return syncOutboxDirectory(o.directory)
}

func syncOutboxDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer func() { _ = handle.Close() }()
	return handle.Sync()
}
