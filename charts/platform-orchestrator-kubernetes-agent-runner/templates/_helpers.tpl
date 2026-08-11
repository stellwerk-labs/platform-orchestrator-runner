{{/*
Expand the name of the chart.
*/}}
{{- define "platform-orchestrator-kubernetes-agent-runner.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "platform-orchestrator-kubernetes-agent-runner.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "platform-orchestrator-kubernetes-agent-runner.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "platform-orchestrator-kubernetes-agent-runner.labels" -}}
helm.sh/chart: {{ include "platform-orchestrator-kubernetes-agent-runner.chart" . }}
{{ include "platform-orchestrator-kubernetes-agent-runner.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "platform-orchestrator-kubernetes-agent-runner.selectorLabels" -}}
app.kubernetes.io/name: {{ include "platform-orchestrator-kubernetes-agent-runner.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "platform-orchestrator-kubernetes-agent-runner.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "platform-orchestrator-kubernetes-agent-runner.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}


{{/*
Allow the release namespace to be overridden for multi-namespace deployments in combined charts.
*/}}
{{- define "platform-orchestrator-kubernetes-agent-runner.namespace" -}}
{{- default .Release.Namespace .Values.namespaceOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}


{{- define "platform-orchestrator-kubernetes-agent-runner.deploymentJobNamespace" -}}
{{- default (include "platform-orchestrator-kubernetes-agent-runner.namespace" .) .Values.jobsRbac.namespace | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "platform-orchestrator-kubernetes-agent-runner.protectedNatsEnv" -}}
{{- if eq .Values.airgap.nats.authType "token" }}
- name: NATS_TOKEN
  valueFrom:
    secretKeyRef:
      name: {{ required "airgap.nats.existingSecret is required for token authentication" .Values.airgap.nats.existingSecret }}
      key: {{ .Values.airgap.nats.tokenKey }}
{{- end }}
{{- if eq .Values.airgap.nats.authType "credentials" }}
- name: NATS_CREDS_FILE
  value: /etc/nats-auth/creds
{{- end }}
{{- if .Values.airgap.nats.caEnabled }}
- name: NATS_CA_FILE
  value: /etc/nats-auth/ca.crt
{{- end }}
{{- end -}}

{{- define "platform-orchestrator-kubernetes-agent-runner.diodeMounts" -}}
volumeMounts:
  - {name: diode, mountPath: /diode}
  - {name: signing-key, mountPath: /keys/sign, readOnly: true}
  - {name: verification-key, mountPath: /keys/verify, readOnly: true}
  {{- if or (eq .Values.airgap.nats.authType "credentials") .Values.airgap.nats.caEnabled }}
  - {name: nats-auth, mountPath: /etc/nats-auth, readOnly: true}
  {{- end }}
securityContext:
  {{- toYaml .Values.securityContext | nindent 2 }}
resources:
  {{- toYaml .Values.resources | nindent 2 }}
{{- end -}}
