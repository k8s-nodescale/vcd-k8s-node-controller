{{- define "vcd-nodescale.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "vcd-nodescale.fullname" -}}
{{- printf "%s" (include "vcd-nodescale.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "vcd-nodescale.namespace" -}}
{{- default .Release.Namespace .Values.namespace.name -}}
{{- end -}}

{{- /*
  The installation serves exactly one platform. The value is mandatory, so the
  render fails instead of silently guessing: a vcd release and a vsphere
  release differ in which credentials they mount, and guessing wrong would
  either leak an unused secret reference or leave the controller without the
  only credentials it needs.
*/ -}}
{{- define "vcd-nodescale.validate" -}}
{{- $p := lower (trim (required "platform is required: set it to vcd or vsphere (an installation never serves both)" .Values.platform)) -}}
{{- if not (has $p (list "vcd" "vsphere")) -}}
{{- fail (printf "platform %q is not supported: use vcd or vsphere" .Values.platform) -}}
{{- end -}}
{{- if and (eq $p "vcd") (not (trim (default "" .Values.vcd.existingSecret))) -}}
{{- fail "platform is vcd but vcd.existingSecret is empty: name the Secret that holds url, user, password, org and vdc" -}}
{{- end -}}
{{- if and (eq $p "vsphere") (not (trim (default "" .Values.vsphere.existingSecret))) -}}
{{- fail "platform is vsphere but vsphere.existingSecret is empty: name the Secret that holds url, user, password and insecure" -}}
{{- end -}}
{{- end -}}

{{- define "vcd-nodescale.platform" -}}
{{- include "vcd-nodescale.validate" . -}}
{{- lower (trim .Values.platform) -}}
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

{{- define "vcd-nodescale.uiImage" -}}
{{- $repo := default .Values.image.repository .Values.ui.image.repository -}}
{{- $tag := default (default .Chart.AppVersion .Values.image.tag) .Values.ui.image.tag -}}
{{- printf "%s:%s" $repo $tag -}}
{{- end -}}
