---
title: "Helm: Edge"
description: "Deploy the Thinpost edge worker with the Helm chart (workload=edge): journal volume, certificates, edge binding, sending addresses and optional node pin."
---

# Helm: Edge

This page covers `workload=edge`, the release that runs the SMTP edge worker in the edge zone. The control plane is in [Helm: control](/deploy/helm-control). The edge image is `ghcr.io/zyno-io/thinpost-edge`.

The edge release holds almost nothing. It has no database or object-storage credential, no hook Jobs and no Service account token. It needs a journal volume, two certificates and a direct public network interface. It does not read the recipient policy or the egress deny ranges: the control plane is the sole authority for both, so the network around the edge is the boundary against a compromised control plane (see the [threat model](/security/threat-model#outbound-smtp-egress-policy)).

## What the chart renders

| Object | Name | Notes |
| --- | --- | --- |
| Deployment | `thinpost-edge-worker` | `replicas` 0 or 1, strategy `Recreate`. |
| NetworkPolicy | `thinpost-edge-worker` | Covers only the cluster's primary interface. |
| NetworkAttachmentDefinition | `thinpost-public`, then `thinpost-public-1`, `thinpost-public-2`, ... | One per sending address. See [Direct public networking](/deploy/direct-public-networking). |

The pod runs as user 65532, non-root, with a read-only root file system and all capabilities dropped. It sets the safe sysctl `net.ipv4.ip_unprivileged_port_start=0` so that it can bind port 25 without privilege, which the restricted Pod Security profile permits. The process refuses to run as root.

## Values

```yaml
# values-edge.yaml
workload: edge
replicas: 1
image: ghcr.io/zyno-io/thinpost-edge@sha256:<digest>

workerId: edge-1
smtpHostname: smtp.example.com
bounceDomain: bounce.example.com
controllerIdentity: spiffe://thinpost.example/control
controlClientCidrs:
  - 198.51.100.21/32
  - 198.51.100.22/32

sourceAddress: 203.0.113.10

publicNetwork:
  master: eth1
  range: 203.0.113.0/24
  gateway: 203.0.113.1
  mtu: 1500
```

| Value | Meaning |
| --- | --- |
| `replicas` | `0` or `1`. The journal volume is single-writer. The release renders but runs no pod at `0`. |
| `workerId` | The worker's stable identifier. Equals `worker-id` in the control plane's `thinpost-edge-binding`. |
| `smtpHostname` | The name the primary address greets with, and its HELO, PTR and certificate name. Also the authentication ID and receiving host on inbound mail from every address. |
| `bounceDomain` | The domain for opaque bounce recipients. Equals `THINPOST_CONTROL_BOUNCE_DOMAIN` on control. |
| `controllerIdentity` | The one SPIFFE URI the edge accepts from the control plane. See [Control PKI](/deploy/control-pki). |
| `controlClientCidrs` | Exact IPv4 `/32` addresses allowed to open the control port. Required for `replicas: 1`; any other prefix fails the render. |
| `sourceAddress` | The primary public IPv4 address: the SMTP listener, the MX and the default outbound source. |
| `publicNetwork.*` | The direct public interface; see [Direct public networking](/deploy/direct-public-networking). |
| `journalCapacity`, `journalReserveBytes`, `clockMargin`, `smtpRate.*` | Journal and SMTP limits. The defaults suit most installs. See the [Helm values reference](/reference/helm-values). |

The chart's defaults for these are placeholders such as `192.0.2.1` and `smtp.example.invalid`. Set your own. The edge also rejects documentation, private and other special-purpose ranges as sending sources when it runs.

`controlClientCidrs` are the source addresses the edge sees on control connections, after any NAT between the zones. They must be **stable**: the edge's NetworkPolicy drops every other source, so pod IPs that change on each control rollout cut the control stream after the next upgrade. Route control traffic through something with fixed addresses, such as the router or gateway between the trusted and edge zones (SNAT to its own addresses) or an egress gateway, and list those addresses here. Because they are fixed, you know them before either release is installed. The edge still requires the control plane's mTLS identity on every connection.

## External resources

All of these live in the edge namespace and must exist before the pod can start.

| Resource | Kind | Contents |
| --- | --- | --- |
| `thinpost-edge-control` | Secret | `ca.crt`, `tls.crt`, `tls.key`: the edge's control server certificate and the private CA. Mounted at `/run/thinpost/control`. |
| `thinpost-smtp-public` | Secret | `tls.crt`, `tls.key`: the public certificate for `smtpHostname`. Mounted at `/run/thinpost/smtp`. |
| `thinpost-journal` | PersistentVolumeClaim | The journal volume. |
| `thinpost-edge-control` | Service | A ClusterIP Service for the control port, so the control plane has a stable address. |
| Certificate Secrets for additional addresses | Secret | Optional, see below. |

### Certificates

- **Control certificate.** Issued by your private CA with a DNS name that control dials (`THINPOST_CONTROL_EDGE_DNS`) and the edge's SPIFFE URI. The edge reloads the leaf certificate and key on each new handshake, so renewals need no restart. The CA bundle and the allowed controller identity are loaded at start. See [Control PKI](/deploy/control-pki).
- **SMTP certificate.** A certificate your sending peers trust, for `smtpHostname`, from a public CA. It must use a **different key** from the control certificate; the edge rejects a reused key. It reloads on each new handshake. Each SMTP listener checks its certificate at start and on every handshake, and a bad certificate stops the worker.

Mount behavior depends on whole-directory Secret volumes, so renewals reach the pod. The chart uses no `subPath`.

### The journal volume

The journal is a small database of attempt metadata, with no message bodies. It lets the worker prove what it did across restarts, and it is what keeps an ambiguous SMTP outcome from becoming a duplicate send.

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: thinpost-journal
  namespace: thinpost-edge
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
```

Requirements:

- `ReadWriteOnce`, on its own file system. The worker preallocates a private reserve file next to the journal, `journalReserveBytes` (default 64 MiB; allowed from 1 MiB to 1 GiB), and verifies that the file system can release and reallocate those blocks. Size the volume well above the reserve.
- The Deployment keeps one replica with the `Recreate` strategy, so only one pod holds the volume and the addresses. An upgrade stops the old pod first, and sending pauses briefly. Bounce senders retry.
- Monitor free space with an independent alert. The record limit does not guarantee disk space, and a full volume stops the worker from claiming new attempts.
- Back up or protect the volume according to how you treat uncertain deliveries; see [Recovery](/operations/recovery). A copied or restored snapshot keeps its old identity and the worker cannot detect that it is stale.

## The edge binding

Two values bind the control plane to one edge: the **worker ID** and the **journal ID**.

1. `workerId` in these values equals `worker-id` in control's `thinpost-edge-binding`.
2. The journal ID is created with the journal and printed by the edge image. Run it once, **while no worker has the volume open**, as a Job in the edge namespace:

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: thinpost-journal-id
  namespace: thinpost-edge
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        fsGroup: 65532
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: journal-id
          image: ghcr.io/zyno-io/thinpost-edge@sha256:<digest>
          args: ["journal-id", "/journal/attempts.db"]
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: [ALL]
          volumeMounts:
            - name: journal
              mountPath: /journal
      volumes:
        - name: journal
          persistentVolumeClaim:
            claimName: thinpost-journal
```

```sh
kubectl -n thinpost-edge apply -f journal-id-job.yaml
kubectl -n thinpost-edge wait --for=condition=complete job/thinpost-journal-id --timeout=120s
kubectl -n thinpost-edge logs job/thinpost-journal-id
```

The output is a 32-character lowercase hexadecimal identity. Put it in control's `thinpost-edge-binding` as `journal-id`. The journal path in the Deployment is `/journal/attempts.db`, the same file the Job initialized.

A fresh volume gets a different identity. Never change the expected identity in the binding merely to get past a mismatch: that means a different journal is in use. Fence the old worker, preserve its journal and run [recovery](/operations/recovery) first.

## Additional sending addresses

`additionalSources` adds sending addresses beyond `sourceAddress`. Each entry becomes its own network attachment, interface and routing table, and its own SMTP listener.

```yaml
additionalSources:
  - address: 198.51.100.20
    hostname: smtp2.example.com
    certificateSecret: thinpost-smtp-second
    range: 198.51.100.0/24
    gateway: 198.51.100.1
    master: eth2
```

| Field | Meaning |
| --- | --- |
| `address` | Required. A unique IPv4 literal, different from `sourceAddress`. |
| `hostname` | Required. A unique lowercase DNS name, at most 219 characters, different from `smtpHostname`. The address greets with it, and its PTR must match it. |
| `certificateSecret` | A `kubernetes.io/tls` Secret whose certificate covers `hostname`. Default `thinpost-smtp-public`, which is valid only if that certificate covers the name. Use one Secret per name. |
| `range`, `gateway`, `master`, `mtu` | Default to the `publicNetwork` values. Override them when the address is in another network. |

Rules and consequences:

- At most 15 additional addresses, and `publicNetwork.table` plus the number of entries may not exceed 252.
- Each listener presents its own certificate, mounted at `/run/thinpost/smtp-sources/<index>` from its Secret, and greets with its own name. A **missing or wrong certificate stops the whole worker at its next start**, including the primary address. Create the Secret before you change the values.
- The edge accepts every listener name, including the primary's, as a bounce domain. `additionalBounceDomains` (at most 7) is only for retired bounce domains that still receive late bounces.
- Add each new address's network to `infrastructure-cidrs` in the recipient ConfigMap on the control plane. The edge reads no such list.
- Changing the list restarts the worker. The pod carries a `checksum/public-network` annotation, so any change to an address, gateway or table rolls it.
- Add the address in the console only after the release is up. See [Sending IPs](/guide/sending-ips) for DNS, PTR and the rollout order.

## SMTP submission

Set `smtpSubmission.enabled: true` to open ports **587** (STARTTLS) and **465** (implicit TLS) on every sending address, so apps and devices can send mail with an SMTP credential. Each address presents the certificate it already uses for port 25, so no new certificate is needed. Port 25 is unchanged and never offers AUTH. The router must allow inbound TCP 465 and 587 to every sending address ([Direct public networking](/deploy/direct-public-networking)).

For clients inside the cluster, prefer the internal listener. Traffic that goes out through a public address and back is source-NATed, so all those clients share one source address and its per-source limits.

```yaml
smtpSubmission:
  enabled: true
  internal:
    enabled: true
    hostname: thinpost-edge-submission.thinpost-edge.svc
    certificateSecret: thinpost-edge-submission-internal   # tls.crt, tls.key, ca.crt
    clientNamespaceSelector:
      matchLabels:
        thinpost-submission: allowed
    clientCidrs: []
```

The chart then renders a ClusterIP Service `thinpost-edge-submission` on port 587, reachable as `thinpost-edge-submission.<namespace>.svc`, and a NetworkPolicy ingress rule for the listed clients. The Secret must come from a cluster-local CA (for example cert-manager) and cover `hostname`; clients trust that CA. The internal listener is not shown in the console. Clients connect to the Service on port 587, which forwards to the listener's container port (`smtpSubmission.internal.port`, default 2587). NetworkPolicy matches the pod's port, so clients' namespaces with a default-deny egress policy need a rule that allows that container port (2587 by default) to the edge pod.

## Control Service and network policy

The control plane dials a stable address. Create a ClusterIP Service in the edge namespace and use its address as `THINPOST_CONTROL_EDGE_TARGET`:

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

The edge listens for control on `<pod IP>:7443`. The chart's NetworkPolicy allows ingress on that port only from `controlClientCidrs`, and it denies all other ingress and all egress on the primary interface. The edge needs no cluster egress, no DNS and no access to the trusted zone.

::: warning The policy does not cover the public interface
The NetworkPolicy applies only to the cluster's primary interface. The Multus interface that carries SMTP is outside it, so the router and any host firewall are the only filters for public traffic. See [Direct public networking](/deploy/direct-public-networking).
:::

## Install

Add the Zyno Helm repository once (`helm repo add zyno https://zyno-io.github.io/charts`), then:

```sh
helm upgrade --install thinpost-edge zyno/thinpost --version <chart version> \
  --namespace thinpost-edge \
  --values values-edge.yaml \
  --wait --timeout 10m
```

The pod uses three probes. Startup and liveness run `thinpost-edge healthcheck liveness`, which checks only the journal, so an unresolved neighbour never restarts the worker. Readiness runs `thinpost-edge healthcheck`, which also waits for the router to resolve every source gateway; see [Direct public networking](/deploy/direct-public-networking#arp-refresh-and-readiness).

The worker also exposes a health endpoint on `127.0.0.1:8081`, inside the pod only. It samples the journal every five seconds, and the sample expires after 15 seconds. The probe does not check free disk, public reachability, certificate validity or whether a controller is connected.

## Pinning to a node

`node` is empty by default and the edge floats to any node that can host the attachments. Set `node` only as a debugging pin, for example to reproduce a problem on a known host:

```yaml
node: worker-3
```

The chart then adds `nodeSelector: kubernetes.io/hostname: worker-3`. The value `unconfigured` is treated as empty. Remove the pin after debugging. A pinned edge cannot float, and a hard node failure then needs the manual steps in [Direct public networking](/deploy/direct-public-networking#hard-node-failure).

## Upgrading the edge

The edge release has no hooks and no database access. An upgrade replaces the pod, with a pause in sending, and the control plane reconnects and replays any result the edge has not acknowledged. Control and edge versions may overlap during a rollout. After a receiving domain is live, an edge that predates inbound mail answers `550` for it, which is permanent for the sender. Roll the edge forward rather than back. See [Upgrades](/deploy/upgrades).

## Next

- [Control PKI](/deploy/control-pki)
- [Direct public networking](/deploy/direct-public-networking)
- [Sending IPs](/guide/sending-ips)
