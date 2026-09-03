{{/* Copyright (c) Microsoft Corporation. Licensed under the MIT License. */}}
{{/* Common helpers for the brewlet chart. */}}

{{- define "brewlet.namespace" -}}
{{- default "brewlet" .Values.namespace -}}
{{- end -}}

{{/*
Resolve a component image. Explicit images.<component> overrides everything.
Otherwise a recorded images.digests.<component> produces an immutable
<registry>/<name>@sha256:<digest> reference, and only when no digest is recorded
does the reference fall back to <registry>/<name>:<tag> (tag defaults to
Chart.appVersion). Released charts record digests, so an installed release is
bound to the exact images the release workflow built and attested.
*/}}
{{- define "brewlet.image" -}}
{{- $override := index .root.Values.images .component -}}
{{- if $override -}}
{{- $override -}}
{{- else -}}
{{- $registry := trimSuffix "/" .root.Values.images.registry -}}
{{- $digests := default (dict) .root.Values.images.digests -}}
{{- $digest := default "" (index $digests .component) -}}
{{- if $digest -}}
{{- if not (regexMatch "^sha256:[0-9a-f]{64}$" $digest) -}}
{{- fail (printf "images.digests.%s must be a full sha256 digest, got %q" .component $digest) -}}
{{- end -}}
{{- printf "%s/%s@%s" $registry .name $digest -}}
{{- else -}}
{{- $tag := default .root.Chart.AppVersion .root.Values.images.tag -}}
{{- printf "%s/%s:%s" $registry .name $tag -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Standard labels applied to every rendered object. */}}
{{- define "brewlet.labels" -}}
app.kubernetes.io/name: brewlet
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: brewlet-{{ .Chart.Version }}
{{- end -}}

{{/* Fully-qualified DNS name of the admission webhook Service. */}}
{{- define "brewlet.admission.serviceName" -}}
brewlet-admission
{{- end -}}

{{/* Render the required structured JDK source list. */}}
{{- define "brewlet.jdkItems" -}}
{{- if kindIs "slice" .value -}}
{{- if eq (len .value) 0 -}}{{- fail "provisioner.jdks must list at least one JDK" -}}{{- end -}}
{{- toYaml .value -}}
{{- else -}}
{{- fail "JDK inventory must be a list of explicit JDK source objects" -}}
{{- end -}}
{{- end -}}

{{/* Render structured JDK inventory as a human-readable token list. */}}
{{- define "brewlet.jdkTokens" -}}
{{- if kindIs "slice" .value -}}
{{- $tokens := list -}}
{{- range $jdk := .value -}}
{{- $tokens = append $tokens (printf "%s-%v" $jdk.distribution $jdk.feature) -}}
{{- end -}}
{{- join "," $tokens -}}
{{- else -}}
{{- fail "JDK inventory must be a list of explicit JDK source objects" -}}
{{- end -}}
{{- end -}}

{{/* Render structured launcher inventory, or nothing when empty. */}}
{{- define "brewlet.launcherItems" -}}
{{- if kindIs "slice" .value -}}
{{- if gt (len .value) 0 -}}
{{- toYaml .value -}}
{{- end -}}
{{- else -}}
{{- fail "launcher inventory must be a list of explicit launcher source objects" -}}
{{- end -}}
{{- end -}}

{{/* Render structured launcher inventory as a human-readable name list. */}}
{{- define "brewlet.launcherNames" -}}
{{- $names := list -}}
{{- range $launcher := .value -}}
{{- $names = append $names $launcher.name -}}
{{- end -}}
{{- default "(vanilla java only)" (join "," $names) -}}
{{- end -}}
