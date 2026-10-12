{{- define "epco.labels" -}}
app.kubernetes.io/name: epco
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- end }}
{{- define "epco.image" -}}
{{- printf "%s:%s" .Values.image.repository .Values.image.tag -}}
{{- if .Values.image.digest -}}@{{ .Values.image.digest }}{{- end -}}
{{- end }}
