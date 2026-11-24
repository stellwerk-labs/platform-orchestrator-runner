//go:build integration

package rollout

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func postgresBackendFile(t *testing.T, server, token string) string {
	t.Helper()
	ca, err := os.ReadFile("../rolloutexec/testdata/native-backend-ca.pem")
	require.NoError(t, err)
	config := clientcmdapi.Config{CurrentContext: "selected",
		Contexts:  map[string]*clientcmdapi.Context{"selected": {Cluster: "cluster", AuthInfo: "user"}},
		Clusters:  map[string]*clientcmdapi.Cluster{"cluster": {Server: server, CertificateAuthorityData: ca}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"user": {Token: token}}}
	data, err := clientcmd.Write(config)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(path, data, 0600))
	return path
}

func TestPostgresNativeBackendBindsDestinationAndAllowsAuthenticationRefresh(t *testing.T) {
	f, _, _ := canonicalFixture(t, "plan", "opentofu")
	f.config.Executor.KubernetesBackendConfigPath = postgresBackendFile(t, "https://cluster-a.test", "PRIVATE_OLD_TOKEN")
	admin := openPostgresFixture(t, f)
	table := pq.QuoteIdentifier(f.config.Postgres.ApplicationSchema)
	var before string
	require.NoError(t, admin.QueryRow(`SELECT row_to_json(s)::text FROM `+table+`.rollout_storage s`).Scan(&before))
	identity := f.engine.executor.NativeBackendIdentity()
	for _, sameTarget := range []bool{true, false} {
		cfg := f.config
		pg := *cfg.Postgres
		cfg.Postgres = &pg
		cfg.Directory = filepath.Join(t.TempDir(), "worker-b")
		pg.WorkerID = "worker-b"
		server := "https://cluster-a.test"
		if !sameTarget {
			server = "https://cluster-b.test"
		}
		// Reuse the caller's Executor deliberately. Open must own its prepared snapshot.
		cfg.Executor.KubernetesBackendConfigPath = postgresBackendFile(t, server, "PRIVATE_REFRESHED_TOKEN")
		other, err := Open(t.Context(), cfg)
		if sameTarget {
			require.NoError(t, err)
			closeFixture(t, other)
		} else {
			require.Nil(t, other)
			requireCode(t, err, "native_backend_identity_conflict")
			requireCode(t, InitializePostgres(t.Context(), cfg), "native_backend_identity_conflict")
		}
		require.Equal(t, identity, f.engine.executor.NativeBackendIdentity(), "another worker must not replace a running snapshot")
		var after string
		require.NoError(t, admin.QueryRow(`SELECT row_to_json(s)::text FROM `+table+`.rollout_storage s`).Scan(&after))
		require.Equal(t, before, after)
	}
	var commands, effects int
	require.NoError(t, admin.QueryRow(`SELECT count(*) FROM `+table+`.rollout_commands`).Scan(&commands))
	require.NoError(t, admin.QueryRow(`SELECT count(*) FROM `+table+`.rollout_effects`).Scan(&effects))
	require.Zero(t, commands)
	require.Zero(t, effects)
	require.NotContains(t, before, "PRIVATE_")
	require.NotContains(t, before, "cluster-a.test", "destination identity must stay inside encrypted identity_check")
}

func TestPostgresNativeBackendRefusesFormatFourWithoutRebinding(t *testing.T) {
	f, _, admin := postgresFixture(t, "plan")
	closeFixture(t, f.engine)
	oldCheck, err := f.engine.encrypt([]byte(storageIdentityCheck))
	require.NoError(t, err)
	table := pq.QuoteIdentifier(f.config.Postgres.ApplicationSchema)
	_, err = admin.Exec(`UPDATE `+table+`.rollout_storage SET version=4,identity_check=$1`, oldCheck)
	require.NoError(t, err)
	var before string
	require.NoError(t, admin.QueryRow(`SELECT row_to_json(s)::text FROM `+table+`.rollout_storage s`).Scan(&before))
	other, err := Open(t.Context(), f.config)
	require.Nil(t, other)
	requireCode(t, err, "postgres_storage_format_refused")
	requireCode(t, InitializePostgres(t.Context(), f.config), "postgres_storage_format_refused")
	var after string
	require.NoError(t, admin.QueryRow(`SELECT row_to_json(s)::text FROM `+table+`.rollout_storage s`).Scan(&after))
	require.Equal(t, before, after)
}
