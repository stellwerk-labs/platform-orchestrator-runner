package gatewayapi

import (
	"encoding/json"
	"errors"
	"time"
)

var ErrNoCommand = errors.New("no runner command available")

const (
	AuthorizationAudience = "platform-orchestrator-runner-gateway"
	DeploymentTokenHeader = "X-Deployment-Token"
)

type CommandResponse struct {
	Command        json.RawMessage `json:"command"`
	Receipt        string          `json:"receipt"`
	Attempt        uint64          `json:"attempt"`
	LeaseExpiresAt time.Time       `json:"lease_expires_at"`
}

type ReceiptRequest struct {
	Receipt string `json:"receipt"`
}

type RetryRequest struct {
	Receipt      string `json:"receipt"`
	DelaySeconds int    `json:"delay_seconds"`
}

type RejectRequest struct {
	Receipt string `json:"receipt"`
	Reason  string `json:"reason"`
}
