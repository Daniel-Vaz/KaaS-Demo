{{/* Names and labels. */}}

{{- define "kaas.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kaas.fullname" -}}
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

{{- define "kaas.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "kaas.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: kaas
{{- end -}}

{{/* Selector labels for one component: {{ include "kaas.selectorLabels" (dict "ctx" . "component" "api") }} */}}
{{- define "kaas.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kaas.name" .ctx }}
app.kubernetes.io/instance: {{ .ctx.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{/*
Image reference for one component.

Tag precedence, most specific first:

  1. image.tags.<component>  a single component pinned to a different version. This is the HOTFIX
                             path - a `worker-v1.4.1` tag republishes one image, and this is how a
                             deployment consumes it without moving everything else.
  2. image.tag               every component pinned together (a nightly, a local build, a rollback).
  3. .Chart.AppVersion       the DEFAULT, and the reason the chart is releasable at all: a chart
                             pulled at version X deploys the platform it was built against, with no
                             --set at all. Never give image.tag a non-empty default in values.yaml -
                             that shadows this and turns appVersion into dead config.
*/}}
{{- define "kaas.image" -}}
{{- $tag := default (default .ctx.Chart.AppVersion .ctx.Values.image.tag) (get (default (dict) .ctx.Values.image.tags) .component) -}}
{{- printf "%s/%s:%s" (trimSuffix "/" .ctx.Values.image.registry) .component $tag -}}
{{- end -}}

{{/* The Secret holding every credential - either the one we render or the user's own. */}}
{{- define "kaas.secretName" -}}
{{- if .Values.existingSecret -}}
{{- .Values.existingSecret -}}
{{- else -}}
{{- include "kaas.fullname" . -}}
{{- end -}}
{{- end -}}

{{/*
Values that must SURVIVE an upgrade even when generated: regenerating the master key would make
every stored kubeconfig undecryptable, and regenerating the shell token would break the API↔agent
channel until every pod restarts. So on upgrade we read back what the existing Secret holds and only
generate on a genuinely first install.
*/}}
{{- define "kaas.secretKey" -}}
{{- if .Values.config.secretKey -}}
{{- .Values.config.secretKey -}}
{{- else -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "kaas.fullname" .) -}}
{{- if and $existing $existing.data (index $existing.data "kaas-secret-key") -}}
{{- index $existing.data "kaas-secret-key" | b64dec -}}
{{- else -}}
{{- randAlphaNum 48 -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "kaas.shellToken" -}}
{{- if .Values.config.shellToken -}}
{{- .Values.config.shellToken -}}
{{- else -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "kaas.fullname" .) -}}
{{- if and $existing $existing.data (index $existing.data "kaas-shell-token") -}}
{{- index $existing.data "kaas-shell-token" | b64dec -}}
{{- else -}}
{{- randAlphaNum 48 -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* The API↔node-ssh bearer token. Same generate-and-preserve story as shellToken, but a SEPARATE
     secret: the node-ssh sandbox holds the VM key, so a leaked shell token must not open it. */}}
{{- define "kaas.nodeSshToken" -}}
{{- if .Values.config.nodeSshToken -}}
{{- .Values.config.nodeSshToken -}}
{{- else -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "kaas.fullname" .) -}}
{{- if and $existing $existing.data (index $existing.data "kaas-node-ssh-token") -}}
{{- index $existing.data "kaas-node-ssh-token" | b64dec -}}
{{- else -}}
{{- randAlphaNum 48 -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* The database DSN: the bundled Postgres, or the external one. */}}
{{- define "kaas.databaseURL" -}}
{{- if .Values.postgres.enabled -}}
{{- printf "postgres://%s:%s@%s-postgres:5432/%s?sslmode=disable" .Values.postgres.auth.username .Values.postgres.auth.password (include "kaas.fullname" .) .Values.postgres.auth.database -}}
{{- else -}}
{{- .Values.postgres.external.dsn -}}
{{- end -}}
{{- end -}}

{{/*
Is this a real install (actual VMs) or the fake one (no hypervisor, same control plane)? The fake
mode drops the shell sandbox and the SSH tunnel entirely - with no real clusters there is nothing for
them to reach - and pins every backend seam to its in-process fake.
*/}}
{{- define "kaas.real" -}}
{{- eq .Values.providers "real" -}}
{{- end -}}

{{/* Is the hypervisor remote (the mode Kubernetes actually wants)? */}}
{{- define "kaas.kvmRemote" -}}
{{- eq .Values.kvm.mode "remote" -}}
{{- end -}}

{{/*
Where kubectl/helm find the SOCKS proxy to the cluster API servers.

  worker - its own, inside its pod: each pod is its own network namespace, so 127.0.0.1 is private
           and replicas never contend for the port (internal/kvmhost opens it at start-up).
  shell  - the shared tunnel Service, because the sandbox holds no hypervisor key and so cannot open
           a tunnel of its own.

Empty in local mode: the VMs are directly routable, and every proxy hop is a no-op.
*/}}
{{- define "kaas.workerSocksAddr" -}}
{{- if eq .Values.kvm.mode "remote" -}}
{{- printf "127.0.0.1:%d" (int .Values.kvm.socks.port) -}}
{{- end -}}
{{- end -}}

{{- define "kaas.shellSocksAddr" -}}
{{- if eq .Values.kvm.mode "remote" -}}
{{- printf "%s-socks:%d" (include "kaas.fullname" .) (int .Values.kvm.socks.port) -}}
{{- end -}}
{{- end -}}

{{/* KAAS_INFRA_PROVIDERS: the comma-joined provider list. */}}
{{- define "kaas.infraProviders" -}}
{{- join "," .Values.infra.providers -}}
{{- end -}}

{{/* Shared env: the platform bits every Go process needs (api and worker alike). Which
infrastructures are enabled, and - for the shared-network providers (vSphere, Proxmox) - the
network shape and capacity cap ADMISSION needs, are common to api and worker exactly as in compose
(deploy/compose.yaml vs. compose.real.yaml): the API decides addresses and quota but never
provisions, so vCenter/Proxmox/NetBox CREDENTIALS live only in worker.yaml, never here. */}}
{{- define "kaas.commonEnv" -}}
- name: DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ include "kaas.secretName" . }}
      key: database-url
- name: KAAS_SECRET_KEY
  valueFrom:
    secretKeyRef:
      name: {{ include "kaas.secretName" . }}
      key: kaas-secret-key
- name: KAAS_ADMIN_USERNAME
  value: {{ .Values.config.adminUsername | quote }}
- name: KAAS_ADMIN_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ include "kaas.secretName" . }}
      key: kaas-admin-password
- name: KAAS_BUDGET_VCPU
  value: {{ .Values.config.budgetVCPU | quote }}
- name: KAAS_BUDGET_MEM_MB
  value: {{ .Values.config.budgetMemMB | quote }}
- name: KAAS_INFRA_PROVIDERS
  value: {{ include "kaas.infraProviders" . | quote }}
{{- if .Values.dns.baseDomain }}
{{/*
Site DNS, NAMING half - needed by both Deployments: the api derives each cluster's domain at
admission, the worker re-runs admission when it seeds/reconciles. The server address and the
credential that writes the zone are worker-only (see worker.yaml), like every provisioning secret.
*/}}
- name: KAAS_DNS_BASE_DOMAIN
  value: {{ .Values.dns.baseDomain | quote }}
- name: KAAS_DNS_APPS_LABEL
  value: {{ .Values.dns.appsLabel | quote }}
{{- end }}
{{/*
HashiCorp Vault (internal/vault) - needed by both Deployments: the worker (reconciler) provisions the
mount/policies/paths with the management token; the API mints the "View in Vault" handoff. Both share
the same address and mount. The token is a chart value here (a dev shortcut, like config.secretKey);
production would mount a scoped token via secretKeyRef.
*/}}
- name: KAAS_VAULT
  value: {{ .Values.vault.seam | quote }}
- name: KAAS_VAULT_ADDR
  value: {{ tpl .Values.vault.addr . | quote }}
{{- if .Values.vault.clusterAddr }}
- name: KAAS_VAULT_CLUSTER_ADDR
  value: {{ tpl .Values.vault.clusterAddr . | quote }}
{{- end }}
- name: KAAS_VAULT_MOUNT
  value: {{ .Values.vault.mount | quote }}
- name: KAAS_VAULT_TOKEN
  value: {{ .Values.vault.rootToken | quote }}
- name: KAAS_VAULT_INSECURE
  value: {{ .Values.vault.insecure | ternary "1" "0" | quote }}
{{- if .Values.vault.uiUrl }}
- name: KAAS_VAULT_UI_URL
  value: {{ tpl .Values.vault.uiUrl . | quote }}
{{- end }}
{{/*
Container image registry (internal/registry) - needed by both Deployments, with DIFFERENT
credentials: the worker creates projects/robots/memberships with an admin account, the API only lists
for the Registry page and should hold a read-only robot (registry.apiUsername/apiPassword). Left
unset the API reuses the admin credential, which is what makes the portal's self-service password
button work and is a documented widening.

`host` is the value a cluster NODE uses - deliberately not `url`, which is a Service in the
management cluster. It is baked into every image reference and every node's containerd config.
*/}}
- name: KAAS_REGISTRY
  value: {{ .Values.registry.seam | quote }}
{{- if .Values.registry.url }}
- name: KAAS_REGISTRY_URL
  value: {{ tpl .Values.registry.url . | quote }}
{{- end }}
{{- if .Values.registry.host }}
- name: KAAS_REGISTRY_HOST
  value: {{ tpl .Values.registry.host . | quote }}
{{- end }}
{{- if .Values.registry.uiUrl }}
- name: KAAS_REGISTRY_UI_URL
  value: {{ tpl .Values.registry.uiUrl . | quote }}
{{- end }}
- name: KAAS_REGISTRY_PROJECT_PREFIX
  value: {{ .Values.registry.projectPrefix | quote }}
- name: KAAS_REGISTRY_MIRROR
  value: {{ .Values.registry.mirror | ternary "1" "0" | quote }}
- name: KAAS_REGISTRY_INSECURE
  value: {{ .Values.registry.insecure | ternary "1" "0" | quote }}
- name: KAAS_REGISTRY_RETAIN_PROJECT
  value: {{ .Values.registry.retainProject | ternary "1" "0" | quote }}
- name: KAAS_REGISTRY_PROJECT_QUOTA_GB
  value: {{ .Values.registry.projectQuotaGB | quote }}
{{- if .Values.registry.caCert }}
- name: KAAS_REGISTRY_CA_FILE
  value: /etc/kaas/registry-ca.crt
{{- end }}
{{- if has "vsphere" .Values.infra.providers }}
- name: KAAS_VSPHERE_NETWORK
  value: {{ .Values.infra.vsphere.network.name | quote }}
- name: KAAS_VSPHERE_NET_MODE
  value: {{ .Values.infra.vsphere.network.mode | quote }}
- name: KAAS_VSPHERE_NET_CIDR
  value: {{ .Values.infra.vsphere.network.cidr | quote }}
- name: KAAS_VSPHERE_NET_GATEWAY
  value: {{ .Values.infra.vsphere.network.gateway | quote }}
- name: KAAS_VSPHERE_NET_DNS
  value: {{ .Values.infra.vsphere.network.dns | quote }}
- name: KAAS_VSPHERE_NET_RANGE
  value: {{ .Values.infra.vsphere.network.range | quote }}
- name: KAAS_VSPHERE_BUDGET_VCPU
  value: {{ .Values.infra.vsphere.budget.vcpu | quote }}
- name: KAAS_VSPHERE_BUDGET_MEM_MB
  value: {{ .Values.infra.vsphere.budget.memMB | quote }}
- name: KAAS_VSPHERE_BUDGET_DISK_GB
  value: {{ .Values.infra.vsphere.budget.diskGB | quote }}
{{- end }}
{{- if has "proxmox" .Values.infra.providers }}
- name: KAAS_PROXMOX_NET_BRIDGE
  value: {{ .Values.infra.proxmox.network.bridge | quote }}
- name: KAAS_PROXMOX_NET_MODE
  value: {{ .Values.infra.proxmox.network.mode | quote }}
- name: KAAS_PROXMOX_NET_CIDR
  value: {{ .Values.infra.proxmox.network.cidr | quote }}
- name: KAAS_PROXMOX_NET_GATEWAY
  value: {{ .Values.infra.proxmox.network.gateway | quote }}
- name: KAAS_PROXMOX_NET_DNS
  value: {{ .Values.infra.proxmox.network.dns | quote }}
- name: KAAS_PROXMOX_NET_RANGE
  value: {{ .Values.infra.proxmox.network.range | quote }}
- name: KAAS_PROXMOX_BUDGET_VCPU
  value: {{ .Values.infra.proxmox.budget.vcpu | quote }}
- name: KAAS_PROXMOX_BUDGET_MEM_MB
  value: {{ .Values.infra.proxmox.budget.memMB | quote }}
- name: KAAS_PROXMOX_BUDGET_DISK_GB
  value: {{ .Values.infra.proxmox.budget.diskGB | quote }}
{{- end }}
{{- end -}}
