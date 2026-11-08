{{/* Expand the chart name. */}}
{{- define "workload-knocker.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Create a default fully qualified app name. */}}
{{- define "workload-knocker.fullname" -}}
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

{{/* Chart name and version for labels. */}}
{{- define "workload-knocker.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Common labels. */}}
{{- define "workload-knocker.labels" -}}
helm.sh/chart: {{ include "workload-knocker.chart" . | quote }}
{{ include "workload-knocker.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}

{{/* Selector labels. */}}
{{- define "workload-knocker.selectorLabels" -}}
app.kubernetes.io/name: {{ include "workload-knocker.name" . | quote }}
app.kubernetes.io/instance: {{ .Release.Name | quote }}
{{- end }}

{{/* ServiceAccount name. */}}
{{- define "workload-knocker.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "workload-knocker.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- required "serviceAccount.name is required when serviceAccount.create is false" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* ConfigMap name. */}}
{{- define "workload-knocker.configMapName" -}}
{{- default (include "workload-knocker.fullname" .) .Values.config.existingConfigMap }}
{{- end }}

{{/* Container image reference. Digest takes priority; an empty tag uses appVersion. */}}
{{- define "workload-knocker.image" -}}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}
{{- end }}
