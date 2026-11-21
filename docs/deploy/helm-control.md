---
title: "Helm: control"
description: Deploy the Thinpost control plane with the Helm chart (workload=control), including every Secret, ConfigMap, ServiceAccount and NetworkPolicy you provide.
---

# Helm: control

The Thinpost chart renders either the control plane or the edge worker, chosen by `workload`. This page covers `workload=control`. The edge is in [Helm: Edge](/deploy/helm-edge).

The chart owns the Deployment and the two hook Jobs. **Everything else is yours:** Secrets, ConfigMaps, the hook ServiceAccount, Services and network policies. Keeping them outside the chart means credentials never pass through Helm values or release history. A missing Secret or ConfigMap leaves pods in `ContainerCreating` or `CreateContainerConfigError`, so create all of them before the first install.

The chart is published as `thinpost` in the Zyno Helm repository, one version per Thinpost release; its source is [`resources/helm/chart`](https://github.com/zyno-io/thinpost/tree/main/resources/helm/chart). The control image is `ghcr.io/zyno-io/thinpost-control`.

```sh
helm repo add zyno https://zyno-io.github.io/charts
helm repo update
```

## What the chart renders

| Object | Name | Notes |
| --- | --- | --- |
| Deployment | `thinpost-operator` | `control.replicas` replicas (0, or 2 or more). Rolling update with `maxSurge: 1` and `maxUnavailable: 0`. |
| Hook Job | `thinpost-control-migrate` | `pre-install`, `pre-upgrade`. Runs `deploy-migrate`. |
| Hook Job | `thinpost-control-gate-reopen` | `post-install`, `post-upgrade`. Runs `deploy-reopen`. |

The hooks render only when `control.migrations.enabled` is true (the default), `control.replicas` is at least 1 and `control.image` is set. See [Upgrades](/deploy/upgrades).

The pods run as user 65532, non-root, with a read-only root file system, all capabilities dropped and the `RuntimeDefault` seccomp profile. Each pod's anti-affinity forbids two operator pods on one node, **including the surge pod during a rollout**. You need at least `control.replicas + 1` schedulable nodes, or a rollout waits for a free node.

## Values

```yaml
# values-control.yaml
workload: control

database:
  caConfigMap: thinpost-db-ca      # default

imagePullSecrets: []

control:
  replicas: 2
  image: ghcr.io/zyno-io/thinpost-control@sha256:<digest>
  reloader:
    enabled: true                  # needs the Stakater Reloader controller
  tls:
    enabled: false
  migrations:
    enabled: true
    serviceAccount: thinpost-migrate
    activeDeadlineSeconds: 600
```

All values are in the [Helm values reference](/reference/helm-values). Some details:

- `control.image` is rendered as one string into the Deployment and both Jobs. The post hook compares pods against that exact string, so a tag and a digest are not interchangeable between releases. Prefer a digest.
- Control has no setting for the host that receiving domains point their MX records at, or for the edge's addresses and names. It takes them from what the edge reports once it connects: the MX host is the main IP's HELO name, shown in the console only when public DNS maps it to the reported address. The old `control.inboundMxHost` value is gone; if you still set it, Helm ignores it.
- `control.webhookInternalDestinations` and `control.serviceCidrs` let webhooks target in-cluster URLs. Leave both empty to allow only public HTTPS. See [Webhooks](/guide/webhooks).
- `control.reloader.enabled` adds the annotation `secret.reloader.stakater.com/reload: thinpost-control-client`, so a renewed control client certificate restarts the pods. The control plane reads that certificate once at start. If you set the annotation yourself in `control.deploymentAnnotations`, set `control.reloader.enabled=false`; the chart refuses both.

## External Secrets

The Deployment mounts each Secret as a read-only directory and reads individual files. Which file name it reads is decided by a path in a ConfigMap, so the names below are conventions you may change as long as the Secret key and the path agree. Every Secret must exist, even if you do not use the feature behind it.

| Secret | Keys | Format | Used for |
| --- | --- | --- | --- |
| `thinpost-db-runtime` | `username`, `password` | Plain text, **no trailing newline** | Runtime database login. Mounted at `/run/thinpost/db`. The `username` also becomes `THINPOST_RUNTIME_ROLE` in the hook Jobs. |
| `thinpost-db-owner` | `username`, `password` | Plain text, no trailing newline | Schema owner login. Mounted **only** in the hook Jobs at `/run/thinpost/db-owner`. |
| `thinpost-object-storage` | `access-key`, `secret-key` | Plain text | Object-store credential, read as environment variables. |
| `thinpost-object-keyring` | `keyring.json` | JSON with `active` and `keys`; see [Object storage](/deploy/object-storage) | Mounted at `/run/thinpost/object-keys`. |
| `thinpost-fingerprint-key` | `keys.json` | JSON array of 1 to 8 base64 strings, each decoding to 32 to 1024 bytes. The first key signs new fingerprints and the rest only verify. | Idempotency fingerprints. Mounted at `/run/thinpost/fingerprint`. |
| `thinpost-bounce-keyring` | `keys.json` | JSON object from version to a base64 secret of at least 32 bytes. At most 32 versions. | Opaque bounce addresses. Mounted at `/run/thinpost/bounce`. |
| `thinpost-tracking-link-keys` | `keys.json` | JSON `{"seed": "<base64 of 32 bytes>", "previous_seeds": []}`; see [Tracking](/guide/tracking) | Tracking link encryption. Mounted at `/run/thinpost/tracking`. May be empty when tracking is off. |
| `thinpost-control-client` | `ca.crt`, `tls.crt`, `tls.key` | PEM | The control plane's client certificate and the private CA. Mounted at `/run/thinpost/control`. See [Control PKI](/deploy/control-pki). |

Create them without a trailing newline in the database files:

```sh
NS=thinpost-control

kubectl -n $NS create secret generic thinpost-db-runtime \
  --from-literal=username=thinpost_runtime --from-literal=password='<runtime password>'
kubectl -n $NS create secret generic thinpost-db-owner \
  --from-literal=username=thinpost_owner --from-literal=password='<owner password>'

printf '["%s"]' "$(openssl rand -base64 32)" > keys.json
kubectl -n $NS create secret generic thinpost-fingerprint-key --from-file=keys.json

printf '{"v1":"%s"}' "$(openssl rand -base64 32)" > keys.json
kubectl -n $NS create secret generic thinpost-bounce-keyring --from-file=keys.json

printf '{"seed":"%s","previous_seeds":[]}' "$(openssl rand -base64 32)" > keys.json
kubectl -n $NS create secret generic thinpost-tracking-link-keys --from-file=keys.json
rm keys.json
```

`--from-literal` adds no newline. The object keyring is created in [Object storage](/deploy/object-storage), the object-storage credential there too, and `thinpost-control-client` by cert-manager in [Control PKI](/deploy/control-pki). Keep the fingerprint key stable for as long as your clients rely on idempotency keys, and keep every bounce key version that a retrying delivery might still carry.

The fingerprint, bounce and tracking material is loaded at start. Rotating any of it needs a rolling restart; see [Rotation](/operations/rotation).

## External ConfigMaps

The Deployment loads `thinpost-operator-config` and `thinpost-mail-runtime` as environment variables with `envFrom`. The chart also reads two keys of `thinpost-operator-config` by name for the hook Jobs. Splitting the settings between the two ConfigMaps is a convention: keep a variable in only one of them, because with duplicates the later source wins silently.

### `thinpost-operator-config`

Process, database, identity and storage settings:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: thinpost-operator-config
  namespace: thinpost-control
data:
  THINPOST_LISTEN: ":8080"
  THINPOST_DATABASE_BASE_URL: "postgres://db.example.com:5432/thinpost?sslmode=verify-full&sslrootcert=/run/thinpost/db-ca/ca.crt&search_path=thinpost"
  THINPOST_DEPLOYMENT_GENERATION: "0123456789abcdef0123456789abcdef"

  THINPOST_OIDC_ISSUER: "https://idp.example.com/realms/thinpost"
  THINPOST_OIDC_JWKS_URL: "https://idp.example.com/realms/thinpost/protocol/openid-connect/certs"
  THINPOST_OIDC_AUDIENCE: "thinpost-api"
  THINPOST_OIDC_ADMIN_SCOPE: "thinpost-admin"
  THINPOST_OIDC_ROLES_CLAIM: "realm_access.roles"
  THINPOST_OIDC_ADMIN_ROLE: "thinpost-admin"
  THINPOST_OIDC_CLIENT_ID: "thinpost-console"
  THINPOST_OIDC_SCOPES: "openid thinpost-admin"
  # Optional: serve the console and operator API only on this hostname
  THINPOST_ADMIN_ORIGIN: "https://thinpost-admin.example.com"

  THINPOST_S3_ENDPOINT: "s3.example.com"
  THINPOST_S3_BUCKET: "thinpost-messages"
  THINPOST_S3_REGION: "us-east-1"
  THINPOST_OBJECT_KEYRING_FILE: "/run/thinpost/object-keys/keyring.json"
```

- **Required keys the chart reads by name:** `THINPOST_DATABASE_BASE_URL` and `THINPOST_DEPLOYMENT_GENERATION`, for the hook Jobs.
- `THINPOST_LISTEN` must be `:8080`. Its default is `127.0.0.1:8080`, which the probes and the Service cannot reach.
- The database URL carries no credentials. The chart sets `THINPOST_DATABASE_USERNAME_FILE` and `THINPOST_DATABASE_PASSWORD_FILE`. See [Database](/deploy/database).
- The OIDC settings are explained in [Operator authentication](/deploy/operator-auth), the S3 settings in [Object storage](/deploy/object-storage).
- `THINPOST_ADMIN_ORIGIN` is optional. Leave it unset to serve the console on every hostname that reaches the Service. Set it to keep the console off the tracking hostname; see [Tracking](/guide/tracking#keep-the-console-off-the-tracking-hostname). It is also required for apps to use their own tracking domains: when set (with tracking configured), the console offers them, and each custom host needs its own route for `/t/`, a certificate and an unchanged `Host` header at your gateway. See [What your gateway must provide](/guide/tracking#what-your-gateway-must-provide).

### `thinpost-mail-runtime`

The mail path: the edge connection, fingerprints and tracking:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: thinpost-mail-runtime
  namespace: thinpost-control
data:
  # Edge connection (all of these are required together)
  THINPOST_CONTROL_EDGE_TARGET: "10.96.40.10:7443"
  THINPOST_CONTROL_IDENTITY: "spiffe://thinpost.example/control"
  THINPOST_CONTROL_EDGE_DNS: "edge.thinpost.example"
  THINPOST_CONTROL_EDGE_IDENTITY: "spiffe://thinpost.example/edge"
  THINPOST_CONTROL_PRIVATE_CA: "/run/thinpost/control/ca.crt"
  THINPOST_CONTROL_CLIENT_CERT: "/run/thinpost/control/tls.crt"
  THINPOST_CONTROL_CLIENT_KEY: "/run/thinpost/control/tls.key"
  THINPOST_CONTROL_BOUNCE_ACTIVE_KEY: "v1"
  THINPOST_CONTROL_BOUNCE_KEYS_FILE: "/run/thinpost/bounce/keys.json"

  # Submission
  THINPOST_CONTROL_FINGERPRINT_KEYS_FILE: "/run/thinpost/fingerprint/keys.json"

  # Optional: open and click tracking
  THINPOST_TRACKING_ORIGIN: "https://t.example.com"
  THINPOST_TRACKING_LINK_KEYS_FILE: "/run/thinpost/tracking/keys.json"
  THINPOST_TRACKING_EVENT_LIMIT: "100"
```

- `THINPOST_CONTROL_EDGE_TARGET` is a **literal IP address and port** that is private or loopback. Use a fixed ClusterIP, see [Services](#services). The edge's certificate must carry the DNS name in `THINPOST_CONTROL_EDGE_DNS` and the URI in `THINPOST_CONTROL_EDGE_IDENTITY`; the control certificate must carry `THINPOST_CONTROL_IDENTITY`.
- The edge's sending addresses and their names come from the edge's own report, so control has no settings for them. `THINPOST_CONTROL_SMTP_SOURCE`, `THINPOST_CONTROL_BOUNCE_DOMAIN` and `THINPOST_INBOUND_MX_HOST` from earlier releases are ignored if still set, so they can stay in place while pods roll out. Remove them when convenient.
- `THINPOST_CONTROL_BOUNCE_ACTIVE_KEY` names a version in the bounce keyring.
- Message submission requires object storage and the edge connection together. There are no DKIM key files to mount: control generates each sender domain's DKIM key itself and keeps it in the database, sealed with the object encryption keyring. `THINPOST_CONTROL_SIGNING_CATALOG` from earlier releases is no longer read; it is ignored if still set, and the chart no longer mounts `thinpost-signing-catalog` or `thinpost-dkim`. See [Domains](/guide/domains).
- Set the tracking variables only if you want tracking. They require the fingerprint key. `THINPOST_TRACKING_EVENT_LIMIT` is an integer from 1 to 1000. See [Tracking](/guide/tracking).
- The chart sets `THINPOST_CONTROL_WORKER_ID`, `THINPOST_CONTROL_JOURNAL_ID` and `THINPOST_CONTROL_INSTANCE_ID` itself.
- Other optional settings, such as `THINPOST_MAX_ATTACHMENT_BYTES`, are in the [configuration reference](/reference/configuration).

### `thinpost-recipient-allowlist`

The recipient policy and the egress deny ranges. The control plane is the sole authority for both, so **this ConfigMap belongs in the control namespace only**: the edge does not read it, and an edge that still carries an older copy ignores it. The recipient allowlist and the egress policy are enforced by control before it authorizes a delivery; secure the edge with network controls (see the [threat model](/security/threat-model#outbound-smtp-egress-policy)).

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: thinpost-recipient-allowlist
data:
  policy: "unrestricted"          # or "allowlist"
  allowlist-file: ""              # for allowlist: /run/thinpost/recipients/allowlist.txt
  infrastructure-cidrs: "203.0.113.0/24"
  route-exceptions-file: ""       # optional: /run/thinpost/recipients/route-exceptions.txt
  # allowlist.txt: |
  #   alice@example.com
```

| Key | Meaning |
| --- | --- |
| `policy` | `allowlist` or `unrestricted`. Required. |
| `allowlist-file` | In `allowlist` mode, a path to a file of exact envelope mailboxes, one per line, that you add to this ConfigMap as another key. Must be empty in `unrestricted` mode. |
| `infrastructure-cidrs` | Comma-separated ranges that SMTP may never target. Include the network of every sending address. |
| `route-exceptions-file` | Optional path to a file of exact recipient, MX host and public IP exceptions. Empty means none. |

The ConfigMap is mounted at `/run/thinpost/recipients` in the control workload and read at start. Changes need a restart of every control replica. An empty, missing or invalid allowlist prevents startup. Use `allowlist` for a staging environment that must only mail test addresses.

### `thinpost-edge-binding`

Ties control to one edge worker and its journal:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: thinpost-edge-binding
  namespace: thinpost-control
data:
  worker-id: "edge-1"
  journal-id: "0123456789abcdef0123456789abcdef"
```

`worker-id` equals the edge release's `workerId`. `journal-id` is the 32-character lowercase hexadecimal identity of the edge's journal volume, printed by `thinpost-edge journal-id`; see [Helm: Edge](/deploy/helm-edge). Never edit it to silence a mismatch.

### The database CA

The ConfigMap named by `database.caConfigMap` (default `thinpost-db-ca`) holds the CA bundle for PostgreSQL under the key `ca.crt`. It is mounted in the Deployment and the hook Jobs at `/run/thinpost/db-ca`.

```sh
kubectl -n thinpost-control create configmap thinpost-db-ca --from-file=ca.crt=db-ca.crt
```

## The hook ServiceAccount

The hook Jobs run as the ServiceAccount named by `control.migrations.serviceAccount` (default `thinpost-migrate`). The post hook lists the operator pods through the Kubernetes API to confirm the rollout, so the account needs `get` and `list` on pods in the release namespace and nothing else:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: thinpost-migrate
  namespace: thinpost-control
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: thinpost-migrate
  namespace: thinpost-control
rules:
  - apiGroups: [""]
    resources: [pods]
    verbs: [get, list]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: thinpost-migrate
  namespace: thinpost-control
subjects:
  - kind: ServiceAccount
    name: thinpost-migrate
roleRef:
  kind: Role
  name: thinpost-migrate
  apiGroup: rbac.authorization.k8s.io
```

The Jobs mount a short-lived projected token for it; the pods otherwise have no token.

## Services

The chart renders no Services. Create two.

```yaml
apiVersion: v1
kind: Service
metadata:
  name: thinpost-operator
  namespace: thinpost-control
spec:
  selector:
    app.kubernetes.io/name: thinpost-operator
  ports:
    - name: http
      port: 8080
      targetPort: http
```

Point your HTTPS gateway at `thinpost-operator` port 8080. If you enable `control.tls`, add a second port that targets the container port named `https`.

For the edge, create a Service in the **edge namespace** with a fixed ClusterIP, because the control plane takes a literal address:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: thinpost-edge-control
  namespace: thinpost-edge
spec:
  clusterIP: 10.96.40.10
  selector:
    app.kubernetes.io/name: thinpost-edge-worker
  ports:
    - name: control
      port: 7443
      targetPort: control
```

Its `<clusterIP>:7443` is `THINPOST_CONTROL_EDGE_TARGET`. If the edge is in another cluster, use whatever private address and route reach its control port. The Service is described again in [Helm: Edge](/deploy/helm-edge).

## Network policies

The chart renders a policy only for the edge. The control plane's policies are yours. Whatever CNI you use, permit these flows and deny the rest:

| Pods | Direction | Peer | Port |
| --- | --- | --- | --- |
| `app.kubernetes.io/name: thinpost-operator` | Ingress | Your HTTPS gateway | TCP 8080, and 8443 if enabled |
| `thinpost-operator` | Egress | PostgreSQL | TCP 5432 |
| `thinpost-operator` | Egress | Object store | TCP 443 |
| `thinpost-operator` | Egress | OIDC provider (JWKS) | TCP 443 |
| `thinpost-operator` | Egress | Cluster DNS and the resolvers it forwards to | UDP and TCP 53 |
| `thinpost-operator` | Egress | The edge control Service | TCP 7443 |
| `thinpost-operator` | Egress | Webhook receivers | TCP 443 (and any internal destination ports) |
| `app.kubernetes.io/name: thinpost-migrate` | Egress | PostgreSQL | TCP 5432 |
| `thinpost-migrate` | Egress | Kubernetes API server | TCP 443 (post hook only) |

The hook pods carry the label `app.kubernetes.io/name: thinpost-migrate` and never the operator label, so they are not behind the Service or the operator's policy. Select them separately.

Add a PodDisruptionBudget for `thinpost-operator` if you want voluntary disruptions to keep a replica running. The chart does not render one.

## Hooks

Two Helm hook Jobs run the control image's `thinpost-control` subcommands. They use the owner credential, which the Deployment never mounts.

| Hook | When | Command | What it does |
| --- | --- | --- | --- |
| `thinpost-control-migrate` | `pre-install`, `pre-upgrade` | `deploy-migrate` | Migrates the schema before pods change; closes and reopens nothing unless the schema must change. |
| `thinpost-control-gate-reopen` | `post-install`, `post-upgrade` | `deploy-reopen` | After Helm's wait succeeds and every pod runs the new image, reopens the gate the pre hook closed. |

Each Job has `backoffLimit: 0` and `activeDeadlineSeconds` from `control.migrations.activeDeadlineSeconds` (default 600). The previous Job stays after a run so that you can read its logs. How the hooks decide what to do, what they refuse and how to recover is in [Upgrades](/deploy/upgrades).

Because the pre-install hook refuses an empty database, run the first-install steps in [Database](/deploy/database) before the first release.

## The HTTPS listener

By default the pods serve plain HTTP on 8080 and your gateway terminates TLS. To serve HTTPS inside the cluster as well, enable the second listener:

```yaml
control:
  tls:
    enabled: true
    secretName: thinpost-operator-tls
    port: 8443
```

The Secret must hold `tls.crt` and `tls.key` and **must exist before you enable the value**. The chart mounts it as a directory with no `subPath` and sets `THINPOST_CONTROL_TLS_LISTEN`, `THINPOST_CONTROL_TLS_CERT` and `THINPOST_CONTROL_TLS_KEY`. The process refuses to start when the files are missing or are not a matching pair. TLS 1.2 is the minimum. On every handshake the server checks the files and reloads them when they change, so a renewed certificate applies without a restart. An unreadable or half-written replacement is ignored with one log line and the previous certificate keeps serving.

Probes stay on the plain port. Add the `https` port to your Service and allow it in your policies. A certificate for `thinpost-operator.thinpost-control.svc.cluster.local`, for example from cert-manager, lets in-cluster clients verify the server against your cluster CA:

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: thinpost-operator-tls
  namespace: thinpost-control
spec:
  secretName: thinpost-operator-tls
  dnsNames:
    - thinpost-operator.thinpost-control.svc.cluster.local
  issuerRef:
    kind: ClusterIssuer
    name: cluster-ca
```

`control.tls.port` must differ from 8080 and be at least 1024.

## Install

```sh
helm upgrade --install thinpost-control zyno/thinpost --version <chart version> \
  --namespace thinpost-control \
  --values values-control.yaml \
  --wait --timeout 20m
```

Pin `--version` to the release you deploy, and set the image to the matching release tag or digest. To deploy from a checkout instead, use `./resources/helm/chart` as the chart path. The `--timeout` must exceed the hooks; see [Upgrades](/deploy/upgrades#helm-and-helmrelease-timeouts).

Check the result:

```sh
kubectl -n thinpost-control get deploy thinpost-operator
kubectl -n thinpost-control logs job/thinpost-control-migrate
kubectl -n thinpost-control logs job/thinpost-control-gate-reopen
```

The pods are ready when `/operator-readyz` succeeds. Startup logs name the failing check (database, schema, runtime privileges, operator identity) without secrets.

## Next

- [Helm: Edge](/deploy/helm-edge)
- [Control PKI](/deploy/control-pki)
- [Upgrades](/deploy/upgrades)
- [Helm values reference](/reference/helm-values)
