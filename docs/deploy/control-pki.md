---
title: Control PKI
description: Build the private certificate authority and the SPIFFE-style identities that secure the mTLS channel between the control plane and the edge worker, and rotate them safely.
---

# Control PKI

The control plane and the edge worker talk over a mutual-TLS gRPC stream that the control plane opens. A private certificate authority issues both sides' certificates, and each side checks the other's **URI identity** in the certificate, not only the chain. The identities look like SPIFFE IDs (`spiffe://thinpost.example/control`), but Thinpost uses neither SPIRE nor a service mesh. You only need a CA that can put URI names in certificates, such as cert-manager.

This page is the certificate setup. The protocol is in [Architecture](/guide/architecture) and the [control protocol reference](/reference/control-protocol).

## The three certificates

| Certificate | Held by | Chain | Names | Key usage |
| --- | --- | --- | --- | --- |
| **Control client** | Control plane | Private CA | URI `spiffe://thinpost.example/control` | Client authentication |
| **Edge control server** | Edge worker | Private CA | DNS name the control plane dials, and URI `spiffe://thinpost.example/edge` | Server authentication |
| **Public SMTP** | Edge worker | A public CA | The SMTP host name of each listener | Server authentication |

The first two are issued by your private CA. The SMTP certificate is separate: it must validate for the SMTP host name against the system trust store of the senders and receivers you talk to, and it must use a **different key** from the control certificate. Each SMTP listener compares the current public keys and refuses to run if a key is reused.

## What each side checks

- **The edge** requires a client certificate that chains to its CA bundle and carries **exactly one** URI from its allowed list. The list is `controllerIdentity` in the edge values, which becomes `THINPOST_EDGE_CONTROLLER_IDENTITIES`. TLS 1.3 is the minimum.
- **The control plane** verifies the edge's chain against its CA bundle, the server name against `THINPOST_CONTROL_EDGE_DNS`, and requires exactly one URI equal to `THINPOST_CONTROL_EDGE_IDENTITY`.
- The identity URI must use the `spiffe` scheme, have a host (the trust domain) and a path, and carry no user information, query or fragment.

Each certificate may contain other URIs, but exactly one must match the configured identity. A certificate that matches two or none is refused.

## Settings

| Where | Variable or value | Example |
| --- | --- | --- |
| Control | `THINPOST_CONTROL_IDENTITY` | `spiffe://thinpost.example/control` |
| Control | `THINPOST_CONTROL_EDGE_DNS` | `edge.thinpost.example` |
| Control | `THINPOST_CONTROL_EDGE_IDENTITY` | `spiffe://thinpost.example/edge` |
| Control | `THINPOST_CONTROL_PRIVATE_CA`, `_CLIENT_CERT`, `_CLIENT_KEY` | `/run/thinpost/control/ca.crt`, `tls.crt`, `tls.key` |
| Edge | `controllerIdentity` (chart value) | `spiffe://thinpost.example/control` |
| Edge | CA, certificate, key | `/run/thinpost/control/ca.crt`, `tls.crt`, `tls.key` (set by the chart) |

The control-side variables live in the `thinpost-mail-runtime` ConfigMap; see [Helm: control](/deploy/helm-control). The edge reads the Secret `thinpost-edge-control`; see [Helm: Edge](/deploy/helm-edge).

## Issue with cert-manager

A self-signed bootstrap issuer creates a CA certificate, and a CA issuer signs both control certificates. Use a `ClusterIssuer` so that the control and edge namespaces can use the same CA.

```yaml
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: thinpost-bootstrap
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: thinpost-control-ca
  namespace: cert-manager
spec:
  isCA: true
  commonName: thinpost-control-ca
  secretName: thinpost-control-ca
  duration: 87600h        # 10 years
  privateKey:
    algorithm: ECDSA
    size: 256
  issuerRef:
    kind: ClusterIssuer
    name: thinpost-bootstrap
---
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: thinpost-control-ca
spec:
  ca:
    secretName: thinpost-control-ca
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: thinpost-control-client
  namespace: thinpost-control
spec:
  secretName: thinpost-control-client
  duration: 2160h         # 90 days
  renewBefore: 720h
  privateKey:
    algorithm: ECDSA
    size: 256
    rotationPolicy: Always
  usages: [digital signature, client auth]
  uris:
    - spiffe://thinpost.example/control
  issuerRef:
    kind: ClusterIssuer
    name: thinpost-control-ca
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: thinpost-edge-control
  namespace: thinpost-edge
spec:
  secretName: thinpost-edge-control
  duration: 2160h
  renewBefore: 720h
  privateKey:
    algorithm: ECDSA
    size: 256
    rotationPolicy: Always
  usages: [digital signature, server auth]
  dnsNames:
    - edge.thinpost.example
  uris:
    - spiffe://thinpost.example/edge
  issuerRef:
    kind: ClusterIssuer
    name: thinpost-control-ca
```

cert-manager writes `ca.crt`, `tls.crt` and `tls.key` into each Secret, which are the keys the chart and the mail-runtime ConfigMap expect. The `edge.thinpost.example` name need not exist in DNS: the control plane connects to the Service address and uses the name only for certificate verification.

The control plane signs its leases with the control client key, so treat that key as a long-lived signing secret. Its Secret is `thinpost-control-client`, and the chart's Reloader annotation restarts the control pods when it renews.

## Choose lifetimes

The control plane **closes a connection before the verified edge certificate chain expires**, so an edge certificate that outlives its renewal window simply forces a reconnect, and an expired one cannot connect. Pick a lifetime much longer than your worst-case outage and renew early, as in the example. Keep the CA's lifetime longer than any leaf's.

## Rotation and reload

| Item | How it changes | Restart needed |
| --- | --- | --- |
| Edge control certificate and key | The edge reloads the leaf on each **new** handshake. | No. Existing connections keep the old certificate until they end. |
| Edge SMTP certificates and keys | Reloaded on each new handshake, per listener. | No. |
| Control client certificate and key | Read once at start. The key also signs leases. | Yes, a rolling restart of the control pods. The Reloader annotation does it for you. |
| CA bundle on either side | Read once at start. | Yes, on both sides. |
| Allowed identities (`controllerIdentity`, `THINPOST_CONTROL_*_IDENTITY`) | Read once at start. | Yes, on both sides. |

Renewing a leaf certificate under the same CA and identity is routine: the edge picks up its new certificate, and the control pods restart in a rolling update. Rotation does not revoke established connections, and Thinpost has no live revocation. A compromised key needs a new CA or a new identity, and the coordinated procedure below.

Changing the CA, the identities or the controller key means both sides must trust the new material at the same time, and an edge restart pauses sending. Do it in a planned window:

1. Create the new CA certificate and add it to the **trust bundles** next to the old one: the `ca.crt` the edge loads and the `ca.crt` the control plane loads. A bundle may contain several CA certificates.
2. Roll the control pods and the edge so both load the combined bundle. The edge restart uses `Recreate`, so sending pauses briefly.
3. Issue the new leaf certificates from the new CA, with the new identities if you are changing them, and set the new identity values on both sides in the same step. Roll the control pods and the edge again.
4. Remove the old CA from the bundles and roll once more.

Before relying on a rotation, test it in a non-production environment, and confirm that the old edge is stopped before any change to a controller identity or key. The control plane signs its leases with the control client key and the edge verifies them against the authenticated peer certificate, so change that key only with the old controllers stopped and the edge restarted. See [Rotation](/operations/rotation) for the maintenance checklist.

## Verify

Check what the certificates actually contain before you deploy:

```sh
kubectl -n thinpost-control get secret thinpost-control-client -o jsonpath='{.data.tls\.crt}' \
  | base64 -d | openssl x509 -noout -text | grep -A1 -E 'Subject Alternative Name|Extended Key Usage'
```

The control certificate shows `URI:spiffe://thinpost.example/control` and `TLS Web Client Authentication`. The edge certificate shows the DNS name and `URI:spiffe://thinpost.example/edge` with `TLS Web Server Authentication`.

If the control plane cannot connect, compare these against the certificates. The usual causes are a mismatched identity string, a DNS name missing from the edge certificate, a trust bundle that lacks the issuing CA and an edge `controlClientCidrs` list that excludes the control connection's source address.

## Next

- [Helm: control](/deploy/helm-control)
- [Helm: Edge](/deploy/helm-edge)
- [Rotation](/operations/rotation)
