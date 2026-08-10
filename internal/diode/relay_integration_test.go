package diode

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stellwerk-labs/golib/hmessaging"
	"github.com/stellwerk-labs/platform-orchestrator-runner/internal/natstransport"
	"github.com/stretchr/testify/require"
)

func TestNATSBundleAttachmentSelectsOnlyCommandDeployment(t *testing.T) {
	url := os.Getenv("NATS_TEST_URL")
	if url == "" {
		t.Skip("NATS_TEST_URL is not set")
	}
	connection, err := natstransport.Connect(natstransport.Config{URL: url, Name: "diode-attachment-test"})
	require.NoError(t, err)
	defer connection.Close()
	js, err := connection.JetStream()
	require.NoError(t, err)
	store, err := js.ObjectStore("PO_RUNNER_BUNDLES")
	if errors.Is(err, nats.ErrBucketNotFound) || errors.Is(err, nats.ErrStreamNotFound) {
		store, err = js.CreateObjectStore(&nats.ObjectStoreConfig{Bucket: "PO_RUNNER_BUNDLES", Storage: nats.FileStorage})
	}
	require.NoError(t, err)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	keyA := "org-a/deployment-a-" + suffix
	keyB := "org-b/deployment-b-" + suffix
	_, err = store.PutBytes(keyA, []byte("bundle-a"))
	require.NoError(t, err)
	_, err = store.PutBytes(keyB, []byte("bundle-b-must-not-cross"))
	require.NoError(t, err)
	command := hmessaging.CommandEnvelope{
		ProtocolVersion: hmessaging.ProtocolVersionV1, CommandID: "create-job:" + suffix,
		OrganizationID: "org-a", RunnerID: "runner-a", DeploymentID: "deployment-a-" + suffix,
		Type: "create-job", CreatedAt: time.Now().UTC(), Payload: json.RawMessage(`{}`),
	}
	data, err := json.Marshal(command)
	require.NoError(t, err)
	attachment, err := loadAttachment(connection, ExportConfig{
		AttachObject: "bundle", BundleBucket: "PO_RUNNER_BUNDLES", MaxAttachmentBytes: 1024,
	}, hmessaging.Message{
		ID: command.CommandID, Subject: "po.v1.orgs.org-a.runners.runner-a.commands", Data: data, CreatedAt: command.CreatedAt,
	})
	require.NoError(t, err)
	require.Equal(t, keyA, attachment.Key)
	require.Equal(t, []byte("bundle-a"), attachment.Data)
	require.NotContains(t, string(attachment.Data), "must-not-cross")
}
