# platform-orchestrator-kubernetes-agent-runner

The agent receives durable runner commands through outbound HTTPS. It never
needs inbound connectivity or NATS credentials.

## Simple mode

Create an Ed25519 identity Secret, then install the chart:

```bash
kubectl create secret generic runner-identity \
  --from-file=private-key.pem=runner_private_key.pem

helm install runner oci://ghcr.io/stellwerk-labs/charts/platform-orchestrator-kubernetes-agent-runner \
  --version 0.3.0 \
  --set platformOrchestrator.orgId=my-org \
  --set platformOrchestrator.runnerId=my-runner \
  --set gateway.url=https://api.example.com/runner-gateway \
  --set gateway.privateKeyExistingSecret=runner-identity
```

The matching public key must be registered in the Orchestrator's
`kubernetes-agent` runner configuration.

## Edge mode

Use an RWX-capable storage class because the agent-created Jobs and the outbox
flusher may run on different nodes:

```yaml
gateway:
  mode: edge
  url: https://api.example.com/runner-gateway
  privateKeyExistingSecret: runner-identity
outbox:
  enabled: true
  storageClassName: rwx-storage
  size: 5Gi
```

Install with `helm install runner ... -f edge-values.yaml`. Commands are
buffered centrally; the PVC buffers results and encrypted logs while HTTPS
connectivity is unavailable. Monitor PVC capacity and flusher errors.

## Air-gapped mode

Create the protected-side Secrets, then install with an explicit values file:

```bash
kubectl create secret generic runner-identity \
  --from-file=private-key.pem=runner_private_key.pem

kubectl create secret generic protected-gateway \
  --from-file=public-key.pem=runner_public_key.pem \
  --from-literal=runner-token-salt='<central RUNNER_TOKEN_SALT>' \
  --from-literal=receipt-key="$(openssl rand -base64 32)"

kubectl create secret generic protected-nats-credentials \
  --from-literal=token="$(openssl rand -base64 32)"
```

```yaml
platformOrchestrator:
  orgId: my-org
  runnerId: my-runner
gateway:
  mode: airgap
  privateKeyExistingSecret: runner-identity
protected-nats:
  enabled: true
  container:
    env:
      NATS_AUTH_TOKEN:
        valueFrom:
          secretKeyRef:
            name: protected-nats-credentials
            key: token
airgap:
  gatewaySecret: protected-gateway
  nats:
    existingSecret: protected-nats-credentials
```

The chart deploys a protected-side HTTPS gateway and broker. The central
`RUNNER_TOKEN_SALT` is sensitive authentication material and must cross the
boundary through an approved secret-transfer procedure. Configure the diode
relay resources separately for both directions. This mode is not qualified
with a physical diode by the chart test suite.

## Requirements

| Repository | Name | Version |
|------------|------|---------|
| https://nats-io.github.io/k8s/helm/charts/ | protected-nats(nats) | 2.14.0 |

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` |  |
| airgap.gatewaySecret | string | `""` |  |
| airgap.nats.authType | string | `"token"` |  |
| airgap.nats.caEnabled | bool | `false` |  |
| airgap.nats.caKey | string | `"ca.crt"` |  |
| airgap.nats.credentialsKey | string | `"creds"` |  |
| airgap.nats.existingSecret | string | `""` |  |
| airgap.nats.tokenKey | string | `"token"` |  |
| airgap.nats.url | string | `""` |  |
| airgap.publicKeyKey | string | `"public-key.pem"` |  |
| airgap.receiptKeyKey | string | `"receipt-key"` |  |
| airgap.runnerTokenSaltKey | string | `"runner-token-salt"` |  |
| commonAnnotations | object | `{}` |  |
| commonLabels | object | `{}` |  |
| diode.commandSubject | string | `""` |  |
| diode.enabled | bool | `false` |  |
| diode.existingClaim | string | `""` |  |
| diode.role | string | `"protected"` |  |
| diode.signingKey | string | `"signing-key.pem"` |  |
| diode.signingKeyExistingSecret | string | `""` |  |
| diode.verificationKey | string | `"verification-key.pem"` |  |
| diode.verificationKeyExistingSecret | string | `""` |  |
| fullnameOverride | string | `""` |  |
| gateway.caExistingSecret | string | `""` |  |
| gateway.caKey | string | `"ca.crt"` |  |
| gateway.clientCertificateExistingSecret | string | `""` |  |
| gateway.clientCertificateKey | string | `"tls.crt"` |  |
| gateway.clientKeyKey | string | `"tls.key"` |  |
| gateway.jobCaExistingSecret | string | `""` |  |
| gateway.mode | string | `"simple"` |  |
| gateway.privateKeyExistingSecret | string | `""` |  |
| gateway.privateKeyKey | string | `"private-key.pem"` |  |
| gateway.url | string | `""` |  |
| image.pullPolicy | string | `"IfNotPresent"` |  |
| image.repository | string | `"ghcr.io/stellwerk-labs/platform-orchestrator-runner"` |  |
| image.tag | string | `""` |  |
| imagePullSecrets | list | `[]` |  |
| jobsRbac.create | bool | `true` |  |
| jobsRbac.namespace | string | `""` |  |
| jobsRbac.serviceAccountName | string | `"kubernetes-runner-job"` |  |
| nameOverride | string | `""` |  |
| namespaceOverride | string | `""` |  |
| nodeSelector | object | `{}` |  |
| outbox.accessModes[0] | string | `"ReadWriteMany"` |  |
| outbox.enabled | bool | `false` |  |
| outbox.size | string | `"1Gi"` |  |
| outbox.storageClassName | string | `""` |  |
| platformOrchestrator.extraEnvVars | list | `[]` |  |
| platformOrchestrator.logLevel | string | `"info"` |  |
| platformOrchestrator.orgId | string | `""` |  |
| platformOrchestrator.runnerId | string | `""` |  |
| podAnnotations | object | `{}` |  |
| podLabels | object | `{}` |  |
| podSecurityContext.fsGroup | int | `1000` |  |
| podSecurityContext.fsGroupChangePolicy | string | `"Always"` |  |
| podSecurityContext.runAsGroup | int | `1000` |  |
| podSecurityContext.runAsNonRoot | bool | `true` |  |
| podSecurityContext.runAsUser | int | `1000` |  |
| protected-nats.config.jetstream.enabled | bool | `true` |  |
| protected-nats.config.jetstream.fileStore.enabled | bool | `true` |  |
| protected-nats.config.jetstream.fileStore.pvc.enabled | bool | `true` |  |
| protected-nats.config.jetstream.fileStore.pvc.size | string | `"2Gi"` |  |
| protected-nats.config.merge.authorization.token | string | `"<< $NATS_AUTH_TOKEN >>"` |  |
| protected-nats.container.env.NATS_AUTH_TOKEN.valueFrom.secretKeyRef.key | string | `"token"` |  |
| protected-nats.container.env.NATS_AUTH_TOKEN.valueFrom.secretKeyRef.name | string | `""` |  |
| protected-nats.container.resources.limits.cpu | string | `"250m"` |  |
| protected-nats.container.resources.limits.memory | string | `"256Mi"` |  |
| protected-nats.container.resources.requests.cpu | string | `"25m"` |  |
| protected-nats.container.resources.requests.memory | string | `"64Mi"` |  |
| protected-nats.enabled | bool | `false` |  |
| protectedNatsBootstrap.image | string | `"natsio/nats-box:0.19.5"` |  |
| rbac.create | bool | `true` |  |
| replicaCount | int | `1` |  |
| resources.limits.cpu | string | `"0.250"` |  |
| resources.limits.memory | string | `"256Mi"` |  |
| resources.requests.cpu | string | `"0.025"` |  |
| resources.requests.memory | string | `"128Mi"` |  |
| securityContext.allowPrivilegeEscalation | bool | `false` |  |
| securityContext.capabilities.drop[0] | string | `"ALL"` |  |
| securityContext.readOnlyRootFilesystem | bool | `true` |  |
| securityContext.runAsNonRoot | bool | `true` |  |
| securityContext.runAsUser | int | `1000` |  |
| securityContext.seccompProfile.type | string | `"RuntimeDefault"` |  |
| serviceAccount.annotations | object | `{}` |  |
| serviceAccount.create | bool | `true` |  |
| serviceAccount.name | string | `""` |  |
| tolerations | list | `[]` |  |
| volumeMounts | list | `[]` |  |
| volumes | list | `[]` |  |
