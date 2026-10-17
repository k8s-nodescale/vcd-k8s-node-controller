{{- define "vcd-nodescale.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "vcd-nodescale.fullname" -}}
{{- printf "%s" (include "vcd-nodescale.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "vcd-nodescale.namespace" -}}
{{- default .Release.Namespace .Values.namespace.name -}}
{{- end -}}

{{- define "vcd-nodescale.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "vcd-nodescale.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "vcd-nodescale.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
