# platform-orchestrator-kubernetes-agent-runner

![Version: 0.2.0](https://img.shields.io/badge/Version-0.2.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: v2.0.0](https://img.shields.io/badge/AppVersion-v2.0.0-informational?style=flat-square)

A Helm chart for the Platform Orchestrator Kubernetes Agent Runner

## Requirements

| Repository | Name | Version |
|------------|------|---------|
| https://nats-io.github.io/k8s/helm/charts/ | protected-nats(nats) | 2.14.0 |

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` |  |
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
| image.pullPolicy | string | `"IfNotPresent"` |  |
| image.repository | string | `"ghcr.io/stellwerk-labs/platform-orchestrator-runner"` |  |
| image.tag | string | `""` |  |
| imagePullSecrets | list | `[]` |  |
| jobsRbac.create | bool | `true` |  |
| jobsRbac.namespace | string | `""` |  |
| jobsRbac.serviceAccountName | string | `"kubernetes-runner-job"` |  |
| nameOverride | string | `""` |  |
| namespaceOverride | string | `""` |  |
| nats.authType | string | `"token"` |  |
| nats.caEnabled | bool | `false` |  |
| nats.caKey | string | `"ca.crt"` |  |
| nats.credentialsKey | string | `"creds"` |  |
| nats.existingSecret | string | `""` |  |
| nats.jobCredentialsExistingSecret | string | `""` |  |
| nats.mode | string | `"simple"` |  |
| nats.token | string | `""` |  |
| nats.tokenKey | string | `"token"` |  |
| nats.url | string | `""` |  |
| nodeSelector | object | `{}` |  |
| outbox.accessModes[0] | string | `"ReadWriteMany"` |  |
| outbox.enabled | bool | `true` |  |
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
