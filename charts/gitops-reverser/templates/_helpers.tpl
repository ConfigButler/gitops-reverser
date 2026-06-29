{{/*
Expand the name of the chart.
*/}}
{{- define "gitops-reverser.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "gitops-reverser.fullname" -}}
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
{{- define "gitops-reverser.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "gitops-reverser.labels" -}}
{{- if eq .Values.labels.managedBy "Helm" }}
helm.sh/chart: {{ include "gitops-reverser.chart" . }}
{{- end }}
{{ include "gitops-reverser.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Values.labels.managedBy | quote }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "gitops-reverser.selectorLabels" -}}
app.kubernetes.io/name: {{ include "gitops-reverser.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "gitops-reverser.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "gitops-reverser.fullname" .) .Values.serviceAccount.name }}
{{- else if .Values.serviceAccount.name }}
{{- .Values.serviceAccount.name }}
{{- else }}
{{- fail "serviceAccount.name must be set when serviceAccount.create=false" }}
{{- end }}
{{- end }}

{{/*
Create the name of the audit service.
*/}}
{{- define "gitops-reverser.auditServiceName" -}}
{{- printf "%s-audit" (include "gitops-reverser.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Default Secret name for the audit root CA.
*/}}
{{- define "gitops-reverser.auditRootCASecretName" -}}
{{- .Values.servers.audit.tls.rootCA.secretNameOverride | default (printf "%s-audit-root-ca" (include "gitops-reverser.fullname" .)) -}}
{{- end }}

{{/*
Default Secret name for the audit server certificate.
*/}}
{{- define "gitops-reverser.auditServerSecretName" -}}
{{- .Values.servers.audit.tls.secretNameOverride | default (printf "%s-audit-server-cert" (include "gitops-reverser.fullname" .)) -}}
{{- end }}

{{/*
Default Secret name for the kube-apiserver audit client certificate.
*/}}
{{- define "gitops-reverser.auditClientSecretName" -}}
{{- .Values.servers.audit.tls.client.secretNameOverride | default (printf "%s-audit-client-cert" (include "gitops-reverser.fullname" .)) -}}
{{- end }}

{{/*
Default Secret name for the validating admission (validate-operator-types) serving certificate.
*/}}
{{- define "gitops-reverser.admissionServerSecretName" -}}
{{- .Values.servers.admission.tls.secretNameOverride | default (printf "%s-admission-server-cert" (include "gitops-reverser.fullname" .)) -}}
{{- end }}

{{/*
Name of the cert-manager Certificate that mints the admission serving cert. Used both
as the Certificate name and in the webhook's cert-manager.io/inject-ca-from annotation.
*/}}
{{- define "gitops-reverser.admissionServerCertName" -}}
{{- printf "%s-admission-server-cert" (include "gitops-reverser.fullname" .) -}}
{{- end }}
