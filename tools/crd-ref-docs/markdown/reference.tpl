{{- define "gvList" -}}
<!-- Generated from Go API sources by crd-ref-docs. DO NOT EDIT. -->
# API reference

Types, presence, default values, enum values and CEL rules come from the Go API source.

{{ range . }}{{ template "gvDetails" . }}{{ end }}
{{- end -}}

{{- define "gvDetails" -}}
## {{ .GroupVersionString }}

{{ .Doc }}

{{ range .SortedTypes }}{{ template "type" . }}{{ end }}
{{- end -}}

{{- define "type" -}}
{{- if markdownShouldRenderType . }}
### {{ .Name }}

{{ .Doc }}
{{- with index .Markers "kubebuilder:validation:XValidation" }}
{{- range . }}
CEL: `{{ .Rule | markdownRenderFieldDoc }}`{{ with .Message }} - {{ . | markdownRenderFieldDoc }}{{ end }}
{{- end }}
{{- end }}
{{- with .Validation }}
Validation: {{ range $i, $validation := . }}{{ if $i }}; {{ end }}{{ markdownRenderFieldDoc $validation }}{{ end }}
{{- end }}
{{ with .Members }}
| Field | Type | Presence | Default | Validation | Description |
| --- | --- | --- | --- | --- | --- |
{{- range . }}
| `{{ .Name }}` | {{ markdownRenderType .Type }} | {{ if or (index .Markers "optional") (index .Markers "k8s:optional") (index .Markers "kubebuilder:validation:Optional") }}optional{{ else }}required{{ end }} | {{ markdownRenderDefault .Default }} | {{ range .Validation }}{{ if ne . "Optional: {}" }}{{ markdownRenderFieldDoc . }}<br />{{ end }}{{ end }}{{ with index .Markers "kubebuilder:validation:XValidation" }}{{ range . }}CEL: `{{ .Rule | markdownRenderFieldDoc }}`{{ with .Message }} - {{ . | markdownRenderFieldDoc }}{{ end }}<br />{{ end }}{{ end }} | {{ markdownRenderFieldDoc .Doc }} |
{{- end }}
{{ end }}
{{ end }}
{{- end -}}
