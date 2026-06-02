{{- define "basuyudb.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "basuyudb.fullname" -}}
{{- printf "%s" (default .Chart.Name .Values.nameOverride) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "basuyudb.labels" -}}
app.kubernetes.io/name: {{ include "basuyudb.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "basuyudb.selectorLabels" -}}
app.kubernetes.io/name: {{ include "basuyudb.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
