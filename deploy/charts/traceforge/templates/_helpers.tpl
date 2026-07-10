{{/*
Naming and labelling helpers, written the standard Helm way so that a reader
who has seen any other chart already knows what these do. The one thing worth
knowing: names are truncated to 63 characters because that is the hard limit
on a Kubernetes label value and on many object names, and a chart that ignores
it produces objects that apply on a short release name and fail on a long one.
*/}}

{{/* The chart's base name, overridable, capped at 63 chars. */}}
{{- define "traceforge.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
The fully-qualified release name. If fullnameOverride is set it wins outright.
Otherwise it is "<release>-<chart>", collapsed to just the release name when the
release name already contains the chart name (so `helm install traceforge ...`
does not yield `traceforge-traceforge`).
*/}}
{{- define "traceforge.fullname" -}}
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

{{/* chart name + version, for the helm.sh/chart label. */}}
{{- define "traceforge.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Common labels stamped on every object. */}}
{{- define "traceforge.labels" -}}
helm.sh/chart: {{ include "traceforge.chart" . }}
{{ include "traceforge.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Selector labels — the stable subset that is safe to put in a selector.
It must NOT include app.kubernetes.io/version: a selector is immutable on a
Deployment/StatefulSet, so a version label in here would make every upgrade
fail with "field is immutable".
*/}}
{{- define "traceforge.selectorLabels" -}}
app.kubernetes.io/name: {{ include "traceforge.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Per-component names and selectors. */}}
{{- define "traceforge.server.fullname" -}}
{{- printf "%s-server" (include "traceforge.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "traceforge.agent.fullname" -}}
{{- printf "%s-agent" (include "traceforge.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "traceforge.server.selectorLabels" -}}
{{ include "traceforge.selectorLabels" . }}
app.kubernetes.io/component: server
{{- end -}}

{{- define "traceforge.agent.selectorLabels" -}}
{{ include "traceforge.selectorLabels" . }}
app.kubernetes.io/component: agent
{{- end -}}

{{/* The ServiceAccount name the pods run as. */}}
{{- define "traceforge.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "traceforge.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Build a fully-qualified image reference and refuse to emit one that is unpinned.
A single Go test rejects any image with the "latest" tag or no tag at all,
because either one makes what actually runs on a node depend on when the image
was last pulled rather than on this chart. The tag defaults to .Chart.AppVersion.
Call with a dict: {"repo": "...", "tag": "...", "root": $}.
*/}}
{{- define "traceforge.image" -}}
{{- $tag := .tag | default .root.Chart.AppVersion -}}
{{- if or (not $tag) (eq (toString $tag) "latest") -}}
{{- fail (printf "image %s must be pinned to an explicit, non-\"latest\" tag; set image.tag or Chart.appVersion" .repo) -}}
{{- end -}}
{{- printf "%s:%s" .repo (toString $tag) -}}
{{- end -}}

{{/*
The hardened container securityContext shared by every container in the chart.
readOnlyRootFilesystem is why /tmp and the data dir must be emptyDir/PVC mounts:
the process can write nowhere else, so a compromised binary cannot drop a payload
onto its own filesystem. drop:[ALL] plus no-privilege-escalation means even a
code-exec bug cannot regain a capability. A Go test rejects any container missing
any of these three.
*/}}
{{- define "traceforge.containerSecurityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
capabilities:
  drop:
    - ALL
{{- end -}}

{{/*
The pod securityContext for every unprivileged pod: run as the distroless
nonroot uid/gid (65532) and pin fsGroup so the mounted PVC is owned by that gid,
otherwise a read-only-root process cannot write its own data volume.
RuntimeDefault seccomp trades nothing for blocking the exotic syscalls neither
the server nor the agent ever makes.
*/}}
{{- define "traceforge.podSecurityContext" -}}
runAsNonRoot: true
runAsUser: 65532
runAsGroup: 65532
fsGroup: 65532
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{/*
Liveness/readiness/startup probes, all HTTP GETs on the named "telemetry" port.
The three endpoints answer different questions on purpose:
  /startupz gates the other two — it flips to 200 once init finished and never
            flips back, so a slow first start does not trip liveness and get the
            pod killed mid-boot.
  /healthz  is liveness: 200 while the process lives; a failure means "restart me".
  /readyz   is readiness: 200 only when the drain gate is open and every backend
            check (including the tsdb fsync health) passes, so a replica with a
            sick disk leaves the Service instead of black-holing writes.
Call with the root context.
*/}}
{{- define "traceforge.probes" -}}
startupProbe:
  httpGet:
    path: /startupz
    port: telemetry
  # Generous: initial TSDB replay can take a while. failureThreshold*periodSeconds
  # is the total boot budget before the pod is declared failed.
  periodSeconds: 5
  failureThreshold: 60
  timeoutSeconds: 3
livenessProbe:
  httpGet:
    path: /healthz
    port: telemetry
  periodSeconds: 10
  failureThreshold: 3
  timeoutSeconds: 3
readinessProbe:
  httpGet:
    path: /readyz
    port: telemetry
  periodSeconds: 10
  failureThreshold: 3
  timeoutSeconds: 3
{{- end -}}
