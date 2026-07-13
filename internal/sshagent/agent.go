package sshagent

import (
	"encoding/pem"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

const (
	sshKeysVar           = "RUNNER_GIT_SSH_KEYS"
	authSockVar          = "SSH_AUTH_SOCK"
	gitSshCommand        = "GIT_SSH_COMMAND"
	defaultGitSshCommand = "ssh -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/dev/null"
)

func parseSshKeys(raw string) ([]interface{}, error) {
	out := make([]interface{}, 0)
	var b *pem.Block
	rest := []byte(raw)
	for i := 0; ; i++ {
		if b, rest = pem.Decode(rest); b == nil {
			if strings.TrimSpace(string(rest)) != "" {
				return nil, fmt.Errorf("failed to parse key %d: invalid PEM", i)
			}
			return out, nil
		}
		reEncoded := pem.EncodeToMemory(b)
		pk, err := ssh.ParseRawPrivateKey(reEncoded)
		if err != nil {
			return nil, fmt.Errorf("failed to parse key %d: %w", i, err)
		} else {
			out = append(out, pk)
		}
	}
}

// Launch starts a new ssh-agent socket with the keys extracted from sshKeysVar and returns a function to stop it.
func Launch(socketPath string) (func(), error) {
	keyring := agent.NewKeyring()

	if v, ok := os.LookupEnv(sshKeysVar); ok {
		keys, err := parseSshKeys(v)
		if err != nil {
			return nil, fmt.Errorf("failed to load keys from %s: %w", sshKeysVar, err)
		}
		slog.Info("loading SSH keys", slog.Int("count", len(keys)))
		for i, k := range keys {
			if err := keyring.Add(agent.AddedKey{PrivateKey: k}); err != nil {
				return nil, fmt.Errorf("failed to add key %d from %s: %w", i, sshKeysVar, err)
			}
		}
		if err := os.Unsetenv(sshKeysVar); err != nil {
			return nil, fmt.Errorf("failed to unset %s: %w", sshKeysVar, err)
		}
	}

	c, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		return nil, fmt.Errorf("failed to chmod %s: %w", socketPath, err)
	}

	go func() {
		slog.Info("starting ssh agent socket")
		for {
			conn, err := c.Accept()
			if err != nil {
				return
			}
			go func() {
				if err := agent.ServeAgent(keyring, conn); err != nil {
					slog.Error(fmt.Sprintf("failed to serve agent: %s", err))
				}
			}()
		}
	}()

	if err := os.Setenv(authSockVar, socketPath); err != nil {
		return nil, fmt.Errorf("failed to set %s: %w", authSockVar, err)
	}
	if k := os.Getenv(gitSshCommand); k == "" {
		if err := os.Setenv(gitSshCommand, defaultGitSshCommand); err != nil {
			return nil, fmt.Errorf("failed to set %s: %w", gitSshCommand, err)
		}
	}

	return func() {
		slog.Info("stopping ssh agent socket")
		if err := c.Close(); err != nil {
			slog.Error(fmt.Sprintf("failed to close ssh agent socket: %s", err))
		}
	}, nil
}
