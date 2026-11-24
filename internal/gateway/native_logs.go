package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"

	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/nativelogs"
)

func (s *Server) putNativeLog(w http.ResponseWriter, r *http.Request) {
	org, route, ok := s.authenticateAgent(w, r)
	if !ok {
		return
	}
	values := r.Header.Values(nativelogs.DescriptorHeader)
	if len(values) != 1 || len(values[0]) > base64.RawURLEncoding.EncodedLen(nativelogs.MetadataLimit) {
		writeError(w, 400, "native log metadata is invalid")
		return
	}
	metadata, err := base64.RawURLEncoding.DecodeString(values[0])
	d, parseErr := nativelogs.ParseDescriptor(metadata)
	expected, routeErr := hmessaging.NativeRunnerToken(d.Authority.Target.RunnerUUID)
	if err != nil || parseErr != nil || routeErr != nil || route != expected || org != d.Authority.OrganizationID ||
		d.Authority.Target.RunnerID != r.PathValue("runnerID") || d.Authority.EnvironmentID != r.PathValue("environmentID") || d.Authority.DeploymentID != r.PathValue("deploymentID") {
		writeError(w, 400, "native log authority is invalid")
		return
	}
	content, err := io.ReadAll(io.LimitReader(r.Body, int64(nativelogs.CiphertextLimit)+1))
	if err != nil || d.VerifyCiphertext(content) != nil {
		writeError(w, 400, "native log ciphertext is invalid")
		return
	}
	if err = s.storeNativeLog(r.Context(), d, content); err != nil {
		writeError(w, 503, "native log persistence is unconfirmed")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) storedNativeLog(ctx context.Context, d nativelogs.Descriptor) error {
	reader, err := s.backend.GetLog(ctx, d.ObjectKey)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	content, err := io.ReadAll(io.LimitReader(reader, int64(nativelogs.CiphertextLimit)+1))
	if err != nil {
		return err
	}
	return d.VerifyCiphertext(content)
}

func (s *Server) storeNativeLog(ctx context.Context, d nativelogs.Descriptor, content []byte) error {
	reader, err := s.backend.GetLog(ctx, d.ObjectKey)
	if err == nil {
		defer func() { _ = reader.Close() }()
		existing, readErr := io.ReadAll(io.LimitReader(reader, int64(nativelogs.CiphertextLimit)+1))
		if readErr != nil {
			return readErr
		}
		if !bytes.Equal(existing, content) {
			return nativelogs.ErrArtifact
		}
		return nil
	}
	// Only absence permits creation. An unavailable object store cannot authorize
	// replacement of bytes whose prior durability is unknown.
	if !isLogNotFound(err) {
		return err
	}
	stored, err := s.backend.PutLog(ctx, d.ObjectKey, bytes.NewReader(content))
	if err != nil {
		return err
	}
	if stored.Size != uint64(len(content)) {
		return nativelogs.ErrArtifact
	}
	return s.storedNativeLog(ctx, d)
}
