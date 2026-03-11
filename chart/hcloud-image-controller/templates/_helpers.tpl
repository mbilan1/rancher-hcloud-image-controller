{{/*
Expand the name of the chart.
*/}}
{{- define "hcloud-image-controller.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "hcloud-image-controller.fullname" -}}
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
Common labels.
*/}}
{{- define "hcloud-image-controller.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
{{ include "hcloud-image-controller.selectorLabels" . }}
app.kubernetes.io/version: {{ default "unknown" .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "hcloud-image-controller.selectorLabels" -}}
app.kubernetes.io/name: {{ include "hcloud-image-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Service account name.
*/}}
{{- define "hcloud-image-controller.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "hcloud-image-controller.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Controller namespace.
*/}}
{{- define "hcloud-image-controller.namespace" -}}
{{- .Values.namespace | default "hcloud-image-system" }}
{{- end }}

{{/*
Builder image reference.
*/}}
{{- define "hcloud-image-controller.builderImage" -}}
{{ .Values.builder.image.repository }}:{{ .Values.builder.image.tag }}
{{- end }}
