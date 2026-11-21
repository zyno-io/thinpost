---
title: Helm values
description: Every value of the Thinpost Helm chart, with type, default and description, grouped by workload.
---

# Helm values

The chart in `resources/helm/chart` renders one of two workloads, chosen with [`workload`](#workload-and-images): the **control plane** (`control`) or the **Edge worker** (`edge`). Install it twice, once per workload, with different values. `values.schema.json` validates every release, so a wrong type or an out-of-range value fails at `helm install` or `helm template` time. A few cross-field rules are enforced by the templates and are noted below.

The control and Edge workloads read their runtime settings from the environment variables in [Environment variables](/reference/configuration). The chart sets the ones that follow from values; Secrets, ConfigMaps and PVCs that it expects but does not create are described in [Deploy](/deploy/).

How to read the tables:

- Paths use dots for nested keys and `[]` for list items, for example `additionalSources[].address`.
- A **Required** value must be set for the schema to pass; every other value has a default.
- The defaults for addresses, hostnames and identities are placeholders (documentation ranges and `.invalid` names). Replace them for a real install; the control plane and Edge reject documentation ranges as real sending sources.

## Workload and images

| Value | Type | Default | Description |
| --- | --- | --- | --- |
| `workload` | `control` or `edge` | `edge` | Which workload to render. Required. |
| `replicas` | integer 0 to 1 | `0` | Edge replicas. `0` renders the Edge with no pods; `1` runs it. More than one is refused because the journal volume is single-writer. Rendering `1` also requires `controlClientCidrs`. Required. |
| `image` | string | `""` | Edge worker image. Falls back to `bootstrapImage` when empty. |
| `bootstrapImage` | string | `unconfigured` | Placeholder image used for the Edge until `image` is set. |
| `imagePullSecrets` | list | `[]` | Image pull Secrets added to every pod spec: the Edge, the control plane and the hook Jobs. Empty renders no `imagePullSecrets`. |
| `imagePullSecrets[].name` | string | none | Secret name. Required for each entry. |

## Edge identity and addresses

These values apply when `workload` is `edge`.

| Value | Type | Default | Description |
| --- | --- | --- | --- |
| `node` | string | `""` | Optional node to pin the Edge to, by hostname. Empty or `unconfigured` lets the pod run on any node. Pin only when something outside the cluster identifies the Edge by the MAC address of its node. |
| `workerId` | string | `unconfigured` | Worker identifier, sent as `THINPOST_EDGE_WORKER_ID`. The control plane's `THINPOST_CONTROL_WORKER_ID` must equal it. |
| `smtpHostname` | string | `smtp.example.invalid` | Primary SMTP name the Edge greets with and uses as its HELO. It needs a certificate, matching reverse DNS and an A record. |
| `bounceDomain` | string | `bounce.example.invalid` | Primary bounce domain. |
| `controllerIdentity` | string | `spiffe://thinpost.invalid/control` | URI identity of the control plane that the Edge accepts on its control listener, sent as `THINPOST_EDGE_CONTROLLER_IDENTITIES`. |
| `controlClientCidrs` | list of strings | `[]` | Source addresses allowed to reach the Edge's control port (7443/tcp). Each entry must be an exact IPv4 `/32`. Required to be non-empty when `replicas` is `1`. |
| `sourceAddress` | IPv4 address | `192.0.2.1` | Primary address: the SMTP listener, the MX target and the default outbound source for every app. The first address the Edge reports. Must be unique among all sources. |
| `additionalSources` | list | `[]` | Further addresses, one network attachment each so each can sit in its own network behind its own gateway. At most 15. The Edge listens for SMTP on every one and sends from it. Changing the list restarts the Edge. Each address must also be in the control plane's infrastructure ranges (`THINPOST_CONTROL_INFRASTRUCTURE_CIDRS`); the control plane refuses an Edge that reports a source outside them. |
| `additionalSources[].address` | IPv4 address | none | Source address. Required, unique, different from `sourceAddress`. |
| `additionalSources[].hostname` | string | none | Name the address greets with, and its certificate and reverse DNS name. Required: a lowercase DNS name of at most 219 characters, unique and different from `smtpHostname`. |
| `additionalSources[].certificateSecret` | string | `thinpost-smtp-public` | `kubernetes.io/tls` Secret in the release namespace whose certificate covers `hostname`. The default Secret can be shared when it covers the name; otherwise use one Secret per name. |
| `additionalSources[].range` | CIDR | `publicNetwork.range` | Network of the address. |
| `additionalSources[].gateway` | IPv4 address | `publicNetwork.gateway` | Gateway for this address. |
| `additionalSources[].master` | string | `publicNetwork.master` | Parent interface for this address's attachment. |
| `additionalSources[].mtu` | integer 576 to 9000 | `publicNetwork.mtu` | MTU for this address's attachment. |
| `additionalBounceDomains` | list of strings | `[]` | Extra bounce domains the Edge accepts, only for retired domains that still receive bounces. At most 7. Each must be a lowercase DNS name, unique and different from `bounceDomain` and every additional hostname. The Edge already accepts `bounceDomain` and each listener's hostname. |

## SMTP submission

Authenticated SMTP submission on the Edge workload. See [Helm: Edge](/deploy/helm-edge#smtp-submission).

| Key | Type | Default | Description |
| --- | --- | --- | --- |
| `smtpSubmission.enabled` | boolean | `false` | Opens ports 587 (STARTTLS) and 465 (implicit TLS) on every sending address, each with that address's own certificate. Allow both ports to every address in the router. Port 25 is unchanged. |
| `smtpSubmission` | object | see below | SMTP submission settings for the Edge. |
| `smtpSubmission.internal` | object | see below | The optional in-cluster listener. |
| `smtpSubmission.internal.enabled` | boolean | `false` | Opens the in-cluster listener and renders the `thinpost-edge-submission` ClusterIP Service and a NetworkPolicy rule for the allowed clients. |
| `smtpSubmission.internal.port` | integer | `2587` | Listener port on the pod address. The Service always exposes `587` and forwards to it. Not `25` or `7443`, and not `465` or `587` while `smtpSubmission.enabled` (those are already container ports). |
| `smtpSubmission.internal.hostname` | string | `''` | DNS name of the listener certificate, for example `thinpost-edge-submission.<namespace>.svc`. Required when enabled. |
| `smtpSubmission.internal.certificateSecret` | string | `thinpost-edge-submission-internal` | Secret with `tls.crt`, `tls.key` and `ca.crt` from the cluster-local CA, mounted as a directory so renewals propagate. |
| `smtpSubmission.internal.clientCidrs` | list of CIDRs | `[]` | Source addresses allowed to connect. |
| `smtpSubmission.internal.clientNamespaceSelector` | object | `{}` | Namespaces allowed to connect. At least one of `clientCidrs` and `clientNamespaceSelector` is required when enabled. |

## Edge journal, clock and rate limits

| Value | Type | Default | Description |
| --- | --- | --- | --- |
| `journalCapacity` | integer | `1000` | Maximum number of messages in the journal (`THINPOST_EDGE_JOURNAL_CAPACITY`, accepted range 1 to 100000 at runtime). |
| `journalReserveBytes` | integer | `67108864` | Journal bytes held in reserve (`THINPOST_EDGE_JOURNAL_RESERVE_BYTES`, accepted range 1 MiB to 1 GiB at runtime). |
| `clockMargin` | string (duration) | `5s` | Allowed clock skew when validating leases (`THINPOST_EDGE_CLOCK_MARGIN`, greater than zero and at most 30 seconds). |
| `smtpRate` | object | see below | Limits on new inbound SMTP connections. |
| `smtpRate.globalInterval` | string (duration) | `100ms` | Interval at which the shared connection budget refills. |
| `smtpRate.globalBurst` | integer | `100` | Shared connection burst. |
| `smtpRate.sourceInterval` | string (duration) | `1s` | Interval at which the budget of one remote address refills. |
| `smtpRate.sourceBurst` | integer | `12` | Connection burst for one remote address. |

## Public network attachment

The Edge's public addresses are attached with a Multus `NetworkAttachmentDefinition` per address, using the `ipvlan` plugin in layer-2 mode, Whereabouts for single-address IPAM and a source-based routing plugin so each address leaves through its own gateway. These values describe them.

| Value | Type | Default | Description |
| --- | --- | --- | --- |
| `publicNetwork` | object | see below | Public network attachment settings. Each `additionalSources` entry may override `range`, `gateway`, `master` and `mtu`. |
| `publicNetwork.master` | string | `unconfigured` | Parent interface on the node for the primary attachment. Set it for a real install. |
| `publicNetwork.range` | CIDR | `192.0.2.0/24` | Network of the primary address. |
| `publicNetwork.gateway` | IPv4 address | `192.0.2.254` | Default gateway for the primary attachment. |
| `publicNetwork.mtu` | integer 576 to 9000 | `1400` | Interface MTU. |
| `publicNetwork.table` | integer, at least 1 | `100` | First source-based routing table. The primary uses `table`; additional source N uses `table + N`. `table` plus the number of additional sources must not exceed 252. |
| `publicNetwork.attachmentLabels` | map of strings | `{}` | Labels added to every `NetworkAttachmentDefinition`, for example one that a policy engine checks. |
| `publicNetwork.cniTypes` | object | see below | CNI plugin binary names used in each attachment. Override when the cluster installs them under versioned or custom names. |
| `publicNetwork.cniTypes.ipvlan` | string | `ipvlan` | Plugin type for the interface. Required when `cniTypes` is set. |
| `publicNetwork.cniTypes.sbr` | string | `sbr` | Plugin type for source-based routing. Required when `cniTypes` is set. |

## Control plane

These values apply when `workload` is `control`.

| Value | Type | Default | Description |
| --- | --- | --- | --- |
| `control` | object | see below | Control plane settings. Required. |
| `control.replicas` | integer | `2` | Control plane replicas: `0` (nothing runs) or at least `2`. Required. |
| `control.image` | string | `""` | Control image, used by the Deployment and the hook Jobs. Required to be present; the hook Jobs are rendered only when it is non-empty. |
| `control.bootstrapImage` | string | `unconfigured` | Placeholder control image used until `control.image` is set. Required to be present. |
| `control.deploymentAnnotations` | map of strings | `{}` | Extra annotations for the control Deployment's metadata. Must not contain `secret.reloader.stakater.com/reload` while `control.reloader.enabled` is true. |
| `control.reloader` | object | see below | Restart-on-renewal behavior. |
| `control.reloader.enabled` | boolean | `true` | Adds the Stakater Reloader annotation so the control Deployment restarts when the `thinpost-control-client` Secret renews. It needs the Reloader controller in the cluster. Set `false` to omit it or to supply your own annotation. |
| `control.webhookInternalDestinations` | list of strings | `[]` | Exact internal webhook URLs the operator API accepts and the dispatcher delivers to, sent joined as `THINPOST_WEBHOOK_INTERNAL_DESTINATIONS`. At most 16. Each must be an `http` or `https` URL whose host is a lowercase `<service>.<namespace>.svc.cluster.local` name, with no credentials, fragment, comma or space. Requires `control.serviceCidrs`. |
| `control.serviceCidrs` | list of strings | `[]` | Cluster Service CIDRs, sent joined as `THINPOST_SERVICE_CIDRS`. At most 8. An internal destination must resolve inside one of them. Take them from the cluster's Service network; the control binary rejects anything that is not wholly private address space. |

### Control HTTPS listener

| Value | Type | Default | Description |
| --- | --- | --- | --- |
| `control.tls` | object | see below | Optional HTTPS listener beside the plain `:8080` one. The public path stays on the plain listener. The Secret is mounted as a directory and re-read on renewal, so certificates rotate without a restart. |
| `control.tls.enabled` | boolean | `false` | Enables the HTTPS listener. Required when `control.tls` is set. |
| `control.tls.secretName` | string | `thinpost-operator-tls` | Secret with `tls.crt` and `tls.key`, mounted as a directory. Required when `control.tls` is set. |
| `control.tls.port` | integer 1024 to 65535 | `8443` | Container port of the HTTPS listener. Must not be `8080`. Required when `control.tls` is set. The chart does not create a Service for it. |

### Schema migration hook Jobs

| Value | Type | Default | Description |
| --- | --- | --- | --- |
| `control.migrations` | object | see below | Helm hook Jobs that migrate the schema before a rollout and reopen the admission gate after it. See [Upgrades](/deploy/upgrades). |
| `control.migrations.enabled` | boolean | `true` | Renders the hook Jobs. They are rendered only when `control.replicas` is above zero and `control.image` is set. Required when `control.migrations` is set. |
| `control.migrations.serviceAccount` | string | `thinpost-migrate` | ServiceAccount the Jobs run as. The chart does not create it. Required when `control.migrations` is set. |
| `control.migrations.activeDeadlineSeconds` | integer 60 to 3600 | `600` | Deadline of each hook Job, in seconds. Required when `control.migrations` is set. |

### Database

| Value | Type | Default | Description |
| --- | --- | --- | --- |
| `database` | object | see below | Database connection settings for the control plane. |
| `database.caConfigMap` | string | `thinpost-db-ca` | Name of a ConfigMap in the release namespace that holds the CA bundle for PostgreSQL in the key `ca.crt`. It is mounted so a connection URL can use `sslrootcert=/run/thinpost/db-ca/ca.crt`, and is re-read on every new database connection, so rotating it needs no restart. Required for the control workload. |
