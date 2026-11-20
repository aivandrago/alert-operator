{{- define "alert-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "alert-operator.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "alert-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "alert-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "alert-operator.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "alert-operator.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: alert-operator
{{- end -}}

{{- define "alert-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "alert-operator.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* The user name the API server sees for the operator. */}}
{{- define "alert-operator.username" -}}
system:serviceaccount:{{ .Release.Namespace }}:{{ include "alert-operator.serviceAccountName" . }}
{{- end -}}
{{/*
Whether cert-manager issues the webhook certificate: "true" or empty.
"auto" (the default) looks for the cert-manager API in the cluster, which is what a chart can
honestly know about it. A plain true or false decides it without asking.
*/}}
{{- define "alert-operator.webhook.certManager" -}}
{{- $setting := .Values.webhook.certificate.certManager -}}
{{- if kindIs "bool" $setting -}}
{{- if $setting }}true{{ end -}}
{{- else if eq (lower (toString $setting)) "auto" -}}
{{- if .Capabilities.APIVersions.Has "cert-manager.io/v1" }}true{{ end -}}
{{- else if eq (lower (toString $setting)) "true" -}}
true
{{- end -}}
{{- end -}}

{{/*
The Secret the webhook serving certificate is in: webhook.certificate.existingSecret, or the
one the chart (or cert-manager) writes.
*/}}
{{- define "alert-operator.webhook.secretName" -}}
{{- default (printf "%s-webhook-cert" (include "alert-operator.fullname" .)) (dig "certificate" "existingSecret" "" .Values.webhook) -}}
{{- end -}}

{{/*
Seconds in a Go duration such as "8760h". mustDateModify parses it with time.ParseDuration,
which is the only duration parser a template has.
*/}}
{{- define "alert-operator.durationSeconds" -}}
{{- $now := now -}}
{{- sub (mustDateModify (printf "+%s" (toString .)) $now).Unix $now.Unix -}}
{{- end -}}

{{/*
The first certificate of a PEM bundle.
*/}}
{{- define "alert-operator.firstCertificate" -}}
{{- regexFind "(?s)-----BEGIN CERTIFICATE-----.*?-----END CERTIFICATE-----" . -}}
{{- end -}}

{{/*
The webhook serving certificate when cert-manager is not there, as ca/cert/key in base64 and
the annotations the Secret carries.

The certificate lives webhook.certificate.duration (whole days) and is signed again on the
first upgrade within renewBefore of its expiry. A template cannot parse a certificate, so the
chart records when it signed one in annotations on the Secret and decides from those:

- an upgrade reads the certificate back out of the Secret while it has more than renewBefore
  left and does not outlive the configured duration;
- otherwise, and when the Secret has no record, it signs a new CA and certificate.

A new CA is written into the webhook configuration while the running pods may still serve
the old certificate for the minute or so the kubelet takes to update the mounted Secret. So
ca.crt carries the new CA followed by the previous one, and the previous one is dropped by the
first upgrade after previous-ca-until (an hour on).

helm template (Argo CD, Flux) has no lookup: every render signs a new CA, as it always did.
Use cert-manager or existingSecret there.
*/}}
{{- define "alert-operator.webhook.selfSignedCert" -}}
{{- $root := .root -}}
{{- $service := .service -}}
{{- $prefix := "alerts.hypersurgery.dev/webhook-" -}}
{{- $layout := "2006-01-02T15:04:05Z07:00" -}}
{{- $now := now -}}
{{- $days := max 1 (div (include "alert-operator.durationSeconds" $root.Values.webhook.certificate.duration | atoi) 86400) -}}
{{- $validity := mul $days 86400 -}}
{{- $renewBefore := include "alert-operator.durationSeconds" $root.Values.webhook.certificate.renewBefore | atoi -}}
{{- if ge $renewBefore $validity -}}
{{- fail (printf "webhook.certificate.renewBefore (%s) must be shorter than webhook.certificate.duration (%s, %d days for the chart-signed certificate)" (toString $root.Values.webhook.certificate.renewBefore) (toString $root.Values.webhook.certificate.duration) $days) -}}
{{- end -}}
{{- $existing := default dict (lookup "v1" "Secret" $root.Release.Namespace .secret) -}}
{{- $data := default dict (get $existing "data") -}}
{{- $annotations := default dict (dig "metadata" "annotations" dict $existing) -}}
{{- $notBefore := (toDate $layout (default "" (get $annotations (print $prefix "not-before")))).Unix -}}
{{- $notAfter := (toDate $layout (default "" (get $annotations (print $prefix "not-after")))).Unix -}}
{{- $reuse := and (get $data "ca.crt") (get $data "tls.crt") (get $data "tls.key") -}}
{{- $reuse = and $reuse (gt $notBefore 0) (gt $notAfter 0) -}}
{{- $reuse = and $reuse (gt (sub $notAfter $now.Unix) $renewBefore) -}}
{{- /* A day of slack for clocks: the record is written when the chart signs. */ -}}
{{- $reuse = and $reuse (le (sub $notAfter $notBefore) (add $validity 86400)) -}}
{{- $out := dict -}}
{{- if $reuse -}}
{{- $ca := get $data "ca.crt" -}}
{{- $previousUntil := get $annotations (print $prefix "previous-ca-until") -}}
{{- if and $previousUntil (gt (toDate $layout $previousUntil).Unix $now.Unix) -}}
{{- $_ := set $out "previousCAUntil" $previousUntil -}}
{{- else if $previousUntil -}}
{{- $ca = printf "%s\n" (include "alert-operator.firstCertificate" (b64dec $ca)) | b64enc -}}
{{- end -}}
{{- $_ := set $out "ca" $ca -}}
{{- $_ := set $out "cert" (get $data "tls.crt") -}}
{{- $_ := set $out "key" (get $data "tls.key") -}}
{{- $_ := set $out "notBefore" (get $annotations (print $prefix "not-before")) -}}
{{- $_ := set $out "notAfter" (get $annotations (print $prefix "not-after")) -}}
{{- else -}}
{{- $altNames := list (printf "%s.%s.svc" $service $root.Release.Namespace) (printf "%s.%s.svc.cluster.local" $service $root.Release.Namespace) -}}
{{- /* A commonName holds 64 characters (RFC 5280); the names clients check are the SANs. */ -}}
{{- $ca := genCA (printf "%s-ca" ($service | trunc 61)) (int $days) -}}
{{- $cert := genSignedCert ($service | trunc 64) nil $altNames (int $days) $ca -}}
{{- $bundle := $ca.Cert -}}
{{- $previous := include "alert-operator.firstCertificate" (b64dec (default "" (get $data "ca.crt"))) -}}
{{- if $previous -}}
{{- $bundle = printf "%s%s\n" $bundle $previous -}}
{{- $_ := set $out "previousCAUntil" (dateInZone $layout (dateModify "+1h" $now) "UTC") -}}
{{- end -}}
{{- $_ := set $out "ca" ($bundle | b64enc) -}}
{{- $_ := set $out "cert" ($cert.Cert | b64enc) -}}
{{- $_ := set $out "key" ($cert.Key | b64enc) -}}
{{- $_ := set $out "notBefore" (dateInZone $layout $now "UTC") -}}
{{- $_ := set $out "notAfter" (dateInZone $layout (dateModify (printf "+%dh" (mul $days 24)) $now) "UTC") -}}
{{- end -}}
{{- toYaml $out -}}
{{- end -}}

{{/*
The CA bundle for a certificate in webhook.certificate.existingSecret, base64: caBundle when
set, else ca.crt from the Secret. helm template cannot read the Secret, so there caBundle is
required.
*/}}
{{- define "alert-operator.webhook.existingCABundle" -}}
{{- $pem := dig "certificate" "caBundle" "" .Values.webhook -}}
{{- if $pem -}}
{{- $pem | b64enc -}}
{{- else -}}
{{- $secret := lookup "v1" "Secret" .Release.Namespace (include "alert-operator.webhook.secretName" .) -}}
{{- $ca := dig "data" "ca.crt" "" (default dict $secret) -}}
{{- if not $ca -}}
{{- fail (printf "webhook.certificate.existingSecret: no ca.crt in Secret %s/%s to put into the webhook configurations; create the Secret with ca.crt first, or set webhook.certificate.caBundle (PEM), which helm template needs because it cannot read Secrets" .Release.Namespace (include "alert-operator.webhook.secretName" .)) -}}
{{- end -}}
{{- $ca -}}
{{- end -}}
{{- end -}}
