{{/*
The control image. The Deployment and both migration hook Jobs render this one
string, so the rollout check in the post hook compares exactly what the
Deployment runs.
*/}}
{{- define "thinpost.controlImage" -}}
{{- required "control.image is required" (default .Values.control.bootstrapImage .Values.control.image) -}}
{{- end -}}

{{/*
The gate actor shared by the pre and post hooks of one Helm revision. Helm has
already incremented .Release.Revision when it renders the upgrade hooks.
*/}}
{{- define "thinpost.deployActor" -}}
deploy:{{ .Release.Namespace }}/{{ .Release.Name }}:r{{ .Release.Revision }}
{{- end -}}

{{/*
Hooks run only for a real control release: control workload, enabled, at least
one replica, and a real image (never the bootstrap placeholder).
*/}}
{{- define "thinpost.migrationHooksEnabled" -}}
{{- if and (eq .Values.workload "control") .Values.control.migrations.enabled (gt (int .Values.control.replicas) 0) .Values.control.image -}}true{{- end -}}
{{- end -}}

{{/*
A hardened hook Job. Parameters: ctx (root), name, hooks (Helm hook events),
args (list), kube (bool, mounts a projected token for the pod list).
The schema owner credential is mounted here only, never in the Deployment.
The pods carry the label the migration network policy selects and never the
operator label, so they are not behind the Service, PDB or operator policy.
*/}}
{{- define "thinpost.hookJob" -}}
{{- $ctx := .ctx -}}
apiVersion: batch/v1
kind: Job
metadata:
  name: {{ .name }}
  namespace: {{ $ctx.Release.Namespace }}
  annotations:
    helm.sh/hook: {{ .hooks }}
    helm.sh/hook-weight: "0"
    helm.sh/hook-delete-policy: before-hook-creation
spec:
  backoffLimit: 0
  activeDeadlineSeconds: {{ $ctx.Values.control.migrations.activeDeadlineSeconds }}
  template:
    metadata:
      labels:
        app.kubernetes.io/name: thinpost-migrate
        app.kubernetes.io/component: {{ .name }}
    spec:
      automountServiceAccountToken: false
      restartPolicy: Never
      serviceAccountName: {{ $ctx.Values.control.migrations.serviceAccount | quote }}
      terminationGracePeriodSeconds: 30
      containers:
      - name: migrate
        image: {{ include "thinpost.controlImage" $ctx | quote }}
        imagePullPolicy: IfNotPresent
        args:
{{- range .args }}
        - {{ . | quote }}
{{- end }}
        env:
        - name: THINPOST_DATABASE_BASE_URL
          valueFrom:
            configMapKeyRef:
              key: THINPOST_DATABASE_BASE_URL
              name: thinpost-operator-config
        - name: THINPOST_DEPLOYMENT_GENERATION
          valueFrom:
            configMapKeyRef:
              key: THINPOST_DEPLOYMENT_GENERATION
              name: thinpost-operator-config
        - name: THINPOST_DATABASE_USERNAME_FILE
          value: /run/thinpost/db-owner/username
        - name: THINPOST_DATABASE_PASSWORD_FILE
          value: /run/thinpost/db-owner/password
        - name: THINPOST_RUNTIME_ROLE
          valueFrom:
            secretKeyRef:
              key: username
              name: thinpost-db-runtime
        resources:
          limits:
            cpu: 500m
            ephemeral-storage: 64Mi
            memory: 256Mi
          requests:
            cpu: 50m
            ephemeral-storage: 16Mi
            memory: 64Mi
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            drop:
            - ALL
          readOnlyRootFilesystem: true
        terminationMessagePolicy: File
        volumeMounts:
        - mountPath: /run/thinpost/db-owner
          name: database-owner
          readOnly: true
        - mountPath: /run/thinpost/db-ca
          name: database-ca
          readOnly: true
{{- if .kube }}
        - mountPath: /run/thinpost/kube
          name: kube-api
          readOnly: true
{{- end }}
{{- with $ctx.Values.imagePullSecrets }}
      imagePullSecrets:
{{ toYaml . | indent 6 }}
{{- end }}
      securityContext:
        fsGroup: 65532
        runAsGroup: 65532
        runAsNonRoot: true
        runAsUser: 65532
        seccompProfile:
          type: RuntimeDefault
      volumes:
      - name: database-owner
        secret:
          defaultMode: 288
          secretName: thinpost-db-owner
      - name: database-ca
        configMap:
          name: {{ $ctx.Values.database.caConfigMap | quote }}
          items:
          - key: ca.crt
            path: ca.crt
{{- if .kube }}
      - name: kube-api
        projected:
          defaultMode: 288
          sources:
          - serviceAccountToken:
              expirationSeconds: 3600
              path: token
          - configMap:
              name: kube-root-ca.crt
              items:
              - key: ca.crt
                path: ca.crt
{{- end }}
{{- end -}}
