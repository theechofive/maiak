{{/* Chart name. */}}
{{- define "maiak.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Fully qualified base name. Release "maiak" -> "maiak". */}}
{{- define "maiak.fullname" -}}
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

{{/* Per-component resource name: <fullname>-<component>. Args: dict ctx, component */}}
{{- define "maiak.componentName" -}}
{{- printf "%s-%s" (include "maiak.fullname" .ctx) .component | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Backend image tag (falls back to appVersion). */}}
{{- define "maiak.imageTag" -}}
{{- default .Chart.AppVersion .Values.image.tag }}
{{- end }}

{{/* Backend image reference. */}}
{{- define "maiak.image" -}}
{{- printf "%s:%s" .Values.image.repository (include "maiak.imageTag" .) }}
{{- end }}

{{/* Web image reference (tag falls back to image.tag). */}}
{{- define "maiak.webImage" -}}
{{- printf "%s:%s" .Values.web.image.repository (default (include "maiak.imageTag" .) .Values.web.image.tag) }}
{{- end }}

{{/* Selector labels. Args: dict ctx, component */}}
{{- define "maiak.selectorLabels" -}}
app.kubernetes.io/name: {{ include "maiak.name" .ctx }}
app.kubernetes.io/instance: {{ .ctx.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{/* Common labels. Args: dict ctx, component */}}
{{- define "maiak.labels" -}}
{{ include "maiak.selectorLabels" . }}
app.kubernetes.io/version: {{ include "maiak.imageTag" .ctx | trunc 63 | trimSuffix "-" | quote }}
app.kubernetes.io/managed-by: {{ .ctx.Release.Service }}
{{- end }}

{{/* Secret names. Arg: root context */}}
{{/* Not managed by Helm/ArgoCD: created at runtime by the bootstrap Job (dbBootstrap.secretName). */}}
{{- define "maiak.secret.db" -}}{{ default (printf "%s-db" (include "maiak.fullname" .)) .Values.dbBootstrap.secretName }}{{- end }}
{{- define "maiak.secret.dbAdmin" -}}{{ include "maiak.secret.db" . }}-admin{{- end }}
{{- define "maiak.secret.admin" -}}{{ include "maiak.fullname" . }}-admin{{- end }}
{{/* One-time plaintext admin password, created by the secrets bootstrap Job (ADR-0091), never by Helm. */}}
{{- define "maiak.secret.adminInitial" -}}{{ include "maiak.secret.admin" . }}-initial{{- end }}
{{- define "maiak.secret.crypto" -}}{{ include "maiak.fullname" . }}-crypto{{- end }}

{{/* True when the secrets bootstrap Job is needed (any secrets.*.source is generate). Arg: root context */}}
{{- define "maiak.secrets.generate" -}}
{{- if or (eq .Values.secrets.admin.source "generate") (eq .Values.secrets.crypto.source "generate") }}true{{ end }}
{{- end }}

{{/* Env shared by api/worker/jobs (config.Server + config.Base). Args: dict ctx, dbKey */}}
{{- define "maiak.env.common" -}}
- name: MAIAK_ENV
  value: {{ .ctx.Values.config.env | quote }}
- name: MAIAK_LOG_LEVEL
  value: {{ .ctx.Values.config.logLevel | quote }}
- name: MAIAK_ADMIN_ADDR
  value: {{ printf ":%d" (int .ctx.Values.config.adminPort) | quote }}
- name: MAIAK_DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ include "maiak.secret.db" .ctx }}
      key: {{ .dbKey }}
{{- end }}

{{/*
Encryption keys (config.Encryption). Only api and jobs include this: the worker never gets the
keys (ADR-0016, L-042). Arg: root context
*/}}
{{- define "maiak.env.encryption" -}}
- name: MAIAK_ENCRYPTION_KEYS
  valueFrom:
    secretKeyRef:
      name: {{ include "maiak.secret.crypto" . }}
      key: keys
- name: MAIAK_ENCRYPTION_ACTIVE_KEY_ID
  valueFrom:
    secretKeyRef:
      name: {{ include "maiak.secret.crypto" . }}
      key: active_key_id
{{- end }}

{{/* SSRF guard extra ranges (config.Guard), api/worker/jobs. Optional: rendered only when set. Arg: root context */}}
{{- define "maiak.env.guard" -}}
{{- with .Values.config.blockedCIDRs }}
- name: MAIAK_BLOCKED_CIDRS
  value: {{ join "," . | quote }}
{{- end }}
{{- end }}

{{/* MAIAK_PUBLIC_URL (api and jobs; required there). Arg: root context */}}
{{- define "maiak.env.publicUrl" -}}
- name: MAIAK_PUBLIC_URL
  value: {{ required "config.publicUrl is required" .Values.config.publicUrl | quote }}
{{- end }}

{{/* api-only env. Arg: root context */}}
{{- define "maiak.env.api" -}}
- name: MAIAK_HTTP_ADDR
  value: {{ printf ":%d" (int .Values.api.httpPort) | quote }}
- name: MAIAK_ADMIN_LOGIN
  valueFrom:
    secretKeyRef:
      name: {{ include "maiak.secret.admin" . }}
      key: login
- name: MAIAK_ADMIN_PASSWORD_HASH
  valueFrom:
    secretKeyRef:
      name: {{ include "maiak.secret.admin" . }}
      key: password_hash
- name: MAIAK_SESSION_TTL
  value: {{ .Values.config.sessionTTL | quote }}
- name: MAIAK_COOKIE_SECURE
  value: {{ .Values.config.cookieSecure | quote }}
- name: MAIAK_TRUSTED_PROXY_HOPS
  value: {{ .Values.config.trustedProxyHops | quote }}
{{- /* Only an explicit false turns registration off; a missing value keeps the app default (true). */}}
- name: MAIAK_REGISTRATION_ENABLED
  value: {{ ternary "false" "true" (and (kindIs "bool" .Values.config.registrationEnabled) (not .Values.config.registrationEnabled)) | quote }}
{{- end }}

{{/* worker-only env (config.Worker). Arg: root context */}}
{{- define "maiak.env.worker" -}}
{{- $w := .Values.worker }}
- name: MAIAK_WORKER_CLAIM_BATCH
  value: {{ $w.claimBatch | quote }}
- name: MAIAK_WORKER_CONCURRENCY
  value: {{ $w.concurrency | quote }}
- name: MAIAK_WORKER_TENANT_CONCURRENCY
  value: {{ $w.tenantConcurrency | quote }}
- name: MAIAK_WORKER_HOST_CONCURRENCY
  value: {{ $w.hostConcurrency | quote }}
- name: MAIAK_WORKER_LOCATION
  value: {{ required "worker.location is required" $w.location | quote }}
{{- end }}

{{/*
Backend Deployment (api / worker / jobs). Args: dict
  ctx, component, cfg (component values), env (pre-rendered env list yaml), httpPort (0 = none)
All three expose the admin port; probes hit /healthz and /readyz there.
NOTE: hostNetwork is intentionally never set (see values.yaml worker comment).
*/}}
{{- define "maiak.backendDeployment" -}}
{{- $ctx := .ctx -}}
{{- $name := include "maiak.componentName" (dict "ctx" $ctx "component" .component) -}}
{{- $sel := dict "ctx" $ctx "component" .component -}}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ $name }}
  labels:
    {{- include "maiak.labels" $sel | nindent 4 }}
spec:
  replicas: {{ .cfg.replicas }}
  revisionHistoryLimit: {{ $ctx.Values.revisionHistoryLimit }}
  strategy:
    type: RollingUpdate
    rollingUpdate:
      {{- toYaml $ctx.Values.strategy.rollingUpdate | nindent 6 }}
  selector:
    matchLabels:
      {{- include "maiak.selectorLabels" $sel | nindent 6 }}
  template:
    metadata:
      labels:
        {{- include "maiak.labels" $sel | nindent 8 }}
        {{- with $ctx.Values.podLabels }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      {{- with $ctx.Values.podAnnotations }}
      annotations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
    spec:
      serviceAccountName: {{ $name }}
      automountServiceAccountToken: false
      terminationGracePeriodSeconds: {{ default $ctx.Values.terminationGracePeriodSeconds .cfg.terminationGracePeriodSeconds }}
      {{- with $ctx.Values.imagePullSecrets }}
      imagePullSecrets:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      securityContext:
        {{- toYaml $ctx.Values.podSecurityContext | nindent 8 }}
      containers:
        - name: {{ .component }}
          image: {{ include "maiak.image" $ctx | quote }}
          imagePullPolicy: {{ $ctx.Values.image.pullPolicy }}
          command:
            {{- toYaml .cfg.command | nindent 12 }}
          ports:
            {{- if .httpPort }}
            - name: http
              containerPort: {{ .httpPort }}
              protocol: TCP
            {{- end }}
            - name: admin
              containerPort: {{ $ctx.Values.config.adminPort }}
              protocol: TCP
          env:
            {{- .env | nindent 12 }}
            {{- with .cfg.env }}
            {{- toYaml . | nindent 12 }}
            {{- end }}
          livenessProbe:
            httpGet:
              path: /healthz
              port: admin
            initialDelaySeconds: 10
            periodSeconds: 10
            timeoutSeconds: 3
            failureThreshold: 3
          readinessProbe:
            httpGet:
              path: /readyz
              port: admin
            initialDelaySeconds: 3
            periodSeconds: 5
            timeoutSeconds: 3
            failureThreshold: 3
          resources:
            {{- toYaml .cfg.resources | nindent 12 }}
          securityContext:
            {{- toYaml $ctx.Values.securityContext | nindent 12 }}
          volumeMounts:
            - name: tmp
              mountPath: /tmp
      volumes:
        - name: tmp
          emptyDir:
            sizeLimit: 64Mi
      {{- include "maiak.scheduling" $sel | nindent 6 }}
{{- end }}

{{/* nodeSelector / tolerations / affinity / topologySpread. Args: dict ctx, component */}}
{{- define "maiak.scheduling" -}}
{{- $ctx := .ctx -}}
{{- with $ctx.Values.nodeSelector }}
nodeSelector:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $ctx.Values.tolerations }}
tolerations:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $ctx.Values.affinity }}
affinity:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- if $ctx.Values.topologySpread.enabled }}
topologySpreadConstraints:
  - maxSkew: {{ $ctx.Values.topologySpread.maxSkew }}
    topologyKey: {{ $ctx.Values.topologySpread.topologyKey }}
    whenUnsatisfiable: {{ $ctx.Values.topologySpread.whenUnsatisfiable }}
    labelSelector:
      matchLabels:
        {{- include "maiak.selectorLabels" . | nindent 8 }}
{{- end }}
{{- end }}
