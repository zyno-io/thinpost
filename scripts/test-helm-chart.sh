#!/bin/sh
# Lint every chart variant and assert the migration hook rendering contract.
# Needs only helm and POSIX tools; no cluster or database is contacted.
set -eu

chart=${1:-resources/helm/chart}
image=example.invalid/control:test
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail() {
  echo "chart assertion failed: $*" >&2
  exit 1
}

render() { # name args...
  name=$1
  shift
  helm template thinpost "$chart" -n thinpost-control "$@" >"$tmp/$name.yaml"
}

helm lint "$chart" --set workload=edge
helm lint "$chart" --set workload=control --set control.image=$image
helm lint "$chart" --set workload=control --set control.image=$image --set control.migrations.enabled=false

render control --set workload=control --set control.image=$image
render edge --set workload=edge
render disabled --set workload=control --set control.image=$image --set control.migrations.enabled=false
render scaled_down --set workload=control --set control.image=$image --set control.replicas=0
render bootstrap --set workload=control

# Additional addresses: one attachment, interface and routing table each, and
# one SMTP listener each with its own hostname and certificate Secret; none
# changes the primary attachment.
render edge_extra --set workload=edge --set 'additionalSources[0].address=203.0.113.10' --set 'additionalSources[0].range=203.0.113.0/24' \
  --set 'additionalSources[0].gateway=203.0.113.1' --set 'additionalSources[0].hostname=smtp.hosting.example' \
  --set 'additionalSources[0].certificateSecret=thinpost-smtp-hosting' \
  --set 'additionalSources[1].address=192.0.2.77' --set 'additionalSources[1].hostname=smtp.second.example' --set 'additionalBounceDomains[0]=bounce.hosting.example'
[ "$(grep -c '^kind: NetworkAttachmentDefinition$' "$tmp/edge.yaml")" = 1 ] || fail "edge with no additional sources must render one attachment"
[ "$(grep -c '^kind: NetworkAttachmentDefinition$' "$tmp/edge_extra.yaml")" = 3 ] || fail "two additional sources must render three attachments"
grep -q "k8s.v1.cni.cncf.io/networks: '\[{\"name\":\"thinpost-public\",\"interface\":\"net1\"},{\"name\":\"thinpost-public-1\",\"interface\":\"net2\"},{\"name\":\"thinpost-public-2\",\"interface\":\"net3\"}\]'" "$tmp/edge_extra.yaml" || fail "networks annotation"
grep -q '{"type": "sbr", "table": 101 }' "$tmp/edge_extra.yaml" || fail "second attachment table"
grep -q '{"type": "sbr", "table": 102 }' "$tmp/edge_extra.yaml" || fail "third attachment table"
[ "$(grep -c '"type": "ipvlan",' "$tmp/edge_extra.yaml")" = 3 ] || fail "default ipvlan CNI type on every attachment"
[ "$(grep -c '{"type": "sbr", "table"' "$tmp/edge_extra.yaml")" = 3 ] || fail "default sbr CNI type on every attachment"
render edge_cni --set workload=edge --set 'additionalSources[0].address=203.0.113.10' --set 'additionalSources[0].hostname=smtp.hosting.example' \
  --set publicNetwork.cniTypes.sbr=sbr-custom --set publicNetwork.cniTypes.ipvlan=ipvlan-custom
[ "$(grep -c '"type": "ipvlan-custom",' "$tmp/edge_cni.yaml")" = 2 ] || fail "ipvlan CNI type override on every attachment"
[ "$(grep -c '{"type": "sbr-custom", "table"' "$tmp/edge_cni.yaml")" = 2 ] || fail "sbr CNI type override on every attachment"
if grep -q '"type": "ipvlan",\|{"type": "sbr",' "$tmp/edge_cni.yaml"; then fail "default CNI type leaked into an override render"; fi

# NetworkAttachmentDefinition labels: none by default, applied to every attachment when set.
helm template thinpost "$chart" --set workload=edge --set 'additionalSources[0].address=203.0.113.10' --set 'additionalSources[0].hostname=smtp.hosting.example' \
  --show-only templates/network-attachment.yaml >"$tmp/nad.yaml" || fail "attachments must render alone"
if grep -q 'labels:' "$tmp/nad.yaml"; then fail "attachments must carry no labels by default"; fi
render edge_labels --set workload=edge --set 'additionalSources[0].address=203.0.113.10' --set 'additionalSources[0].hostname=smtp.hosting.example' \
  --set 'publicNetwork.attachmentLabels.example\.test/approved=granted' --set publicNetwork.attachmentLabels.tier=public
[ "$(grep -c '^    example.test/approved: granted$' "$tmp/edge_labels.yaml")" = 2 ] || fail "attachment label on both attachments"
[ "$(grep -c '^    tier: public$' "$tmp/edge_labels.yaml")" = 2 ] || fail "second attachment label on both attachments"
grep -q '"gateway": "203.0.113.1"' "$tmp/edge_extra.yaml" || fail "per-address gateway"
grep -q 'name: THINPOST_EDGE_SMTP_ADDITIONAL_SOURCES' "$tmp/edge_extra.yaml" || fail "additional sources env"
grep -q 'value: "203.0.113.10,192.0.2.77"' "$tmp/edge_extra.yaml" || fail "additional sources env value"
grep -q 'name: THINPOST_EDGE_SMTP_ADDITIONAL_HOSTNAMES' "$tmp/edge_extra.yaml" || fail "additional hostnames env"
grep -q 'value: "smtp.hosting.example,smtp.second.example"' "$tmp/edge_extra.yaml" || fail "additional hostnames env value"
grep -q 'name: THINPOST_EDGE_SMTP_ADDITIONAL_CERT_ROOT' "$tmp/edge_extra.yaml" || fail "additional certificate root env"
grep -q 'value: /run/thinpost/smtp-sources$' "$tmp/edge_extra.yaml" || fail "additional certificate root value"
grep -q 'value: "bounce.hosting.example"' "$tmp/edge_extra.yaml" || fail "additional bounce domains env"
# One whole-directory secret volume per entry: its own Secret (the default for
# an entry that names none), mounted without subPath so renewals propagate.
grep -q 'mountPath: /run/thinpost/smtp-sources/0$' "$tmp/edge_extra.yaml" || fail "first additional certificate mount"
grep -q 'mountPath: /run/thinpost/smtp-sources/1$' "$tmp/edge_extra.yaml" || fail "second additional certificate mount"
grep -q 'secretName: "thinpost-smtp-hosting"' "$tmp/edge_extra.yaml" || fail "named certificate secret"
[ "$(grep -c 'secretName: "thinpost-smtp-public"' "$tmp/edge_extra.yaml")" = 1 ] || fail "default certificate secret for the second entry"
[ "$(grep -c 'secretName: thinpost-smtp-public' "$tmp/edge_extra.yaml")" = 1 ] || fail "primary certificate secret"
if grep -q 'subPath' "$tmp/edge_extra.yaml"; then fail "certificate volumes must not use subPath"; fi
if grep -q 'ADDITIONAL\|smtp-sources' "$tmp/edge.yaml"; then fail "an Edge with no additional sources must not set the additional env or mounts"; fi
for bad in 'additionalSources[0].address=192.0.2.1' 'additionalSources[0].address=not-an-ip' 'additionalBounceDomains[0]=Bad.Example.com'; do
  if helm template thinpost "$chart" --set workload=edge --set 'additionalSources[0].hostname=smtp.a.example' --set "$bad" >/dev/null 2>&1; then fail "accepted invalid $bad"; fi
done
# Each entry needs a valid, unique hostname and a sane certificate Secret name.
valid_source='additionalSources[0].address=203.0.113.10'
for bad in 'additionalSources[0].hostname=' 'additionalSources[0].hostname=Smtp.Example.com' 'additionalSources[0].hostname=localhost' \
  'additionalSources[0].hostname=smtp.example.invalid' 'additionalSources[0].certificateSecret=Bad_Name' 'additionalSources[0].certificateSecret=-x'; do
  if helm template thinpost "$chart" --set workload=edge --set "$valid_source" --set 'additionalSources[0].hostname=smtp.a.example' --set "$bad" >/dev/null 2>&1; then fail "accepted invalid $bad"; fi
done
if helm template thinpost "$chart" --set workload=edge --set "$valid_source" >/dev/null 2>&1; then fail "accepted an additional source with no hostname"; fi
if helm template thinpost "$chart" --set workload=edge --set "$valid_source" --set 'additionalSources[0].hostname=smtp.a.example' \
  --set 'additionalSources[1].address=203.0.113.11' --set 'additionalSources[1].hostname=smtp.a.example' >/dev/null 2>&1; then fail "accepted duplicate hostnames"; fi
if helm template thinpost "$chart" --set workload=edge --set "$valid_source" --set 'additionalSources[0].hostname=smtp.example.invalid' >/dev/null 2>&1; then fail "accepted the primary hostname"; fi
if helm template thinpost "$chart" --set workload=edge --set "$valid_source" --set 'additionalSources[0].hostname=smtp.a.example' \
  --set 'additionalBounceDomains[0]=smtp.a.example' >/dev/null 2>&1; then fail "accepted a bounce domain that repeats a listener hostname"; fi

# The Edge floats across nodes unless node is set: an empty or placeholder node
# renders no pin, and a set node still pins it (existing values keep working until
# the routers stop checking a source MAC for these addresses). One pod at a time
# (Recreate, one replica at most) because the journal volume is single-writer.
if grep -q 'nodeSelector' "$tmp/edge.yaml"; then fail "an Edge with no node value must not be pinned"; fi
helm template thinpost "$chart" --set workload=edge --set node=unconfigured >"$tmp/edge_unconfigured.yaml" || fail "the placeholder node value must render"
if grep -q 'nodeSelector' "$tmp/edge_unconfigured.yaml"; then fail "the placeholder node value must not pin the Edge"; fi
helm template thinpost "$chart" --set workload=edge --set node=edge-node-1 >"$tmp/edge_node.yaml" || fail "a node value must render"
grep -q 'kubernetes.io/hostname: "edge-node-1"' "$tmp/edge_node.yaml" || fail "a node value must pin the Edge"
# Source announcement: one gateway per source (primary first, then each
# additional source's own or the default), and a pod annotation that follows the
# attachments, so a NAD-only change restarts the pod. No placement constraints.
grep -A1 'name: THINPOST_EDGE_SOURCE_GATEWAYS' "$tmp/edge.yaml" | grep -q 'value: "192.0.2.254"$' || fail "primary gateway env"
grep -A1 'name: THINPOST_EDGE_SOURCE_GATEWAYS' "$tmp/edge_extra.yaml" | grep -q 'value: "192.0.2.254,203.0.113.1,192.0.2.254"$' || fail "gateway env order and defaults"
sum_of() { grep 'checksum/public-network:' "$1" | sed 's/.*: //'; }
[ -n "$(sum_of "$tmp/edge.yaml")" ] || fail "missing checksum/public-network annotation"
[ "$(sum_of "$tmp/edge.yaml")" != "$(sum_of "$tmp/edge_extra.yaml")" ] || fail "additionalSources must change the public-network checksum"
render edge_again --set workload=edge
[ "$(sum_of "$tmp/edge.yaml")" = "$(sum_of "$tmp/edge_again.yaml")" ] || fail "the public-network checksum must be stable"
render edge_gw --set workload=edge --set publicNetwork.gateway=192.0.2.253
[ "$(sum_of "$tmp/edge.yaml")" != "$(sum_of "$tmp/edge_gw.yaml")" ] || fail "a gateway change must change the checksum"
for forbidden in affinity topologySpread PodDisruptionBudget hostNetwork; do
  if grep -q "$forbidden" "$tmp/edge_extra.yaml"; then fail "the Edge must not render $forbidden"; fi
done
# Control is the sole authority for the recipient allowlist and the egress
# policy: the Edge renders neither their environment nor their ConfigMap.
for forbidden in THINPOST_RECIPIENT THINPOST_EDGE_INFRASTRUCTURE_CIDRS THINPOST_EDGE_ROUTE_EXCEPTIONS_FILE thinpost-recipient-allowlist /run/thinpost/recipients; do
  if grep -q "$forbidden" "$tmp/edge.yaml" "$tmp/edge_extra.yaml"; then fail "the Edge must not render $forbidden"; fi
done
grep -q 'thinpost-recipient-allowlist' "$tmp/control.yaml" || fail "control must still render the recipient policy ConfigMap"
# Liveness and startup check the journal only; readiness also waits for neighbours.
[ "$(grep -c 'healthcheck, liveness\]' "$tmp/edge.yaml")" = 2 ] || fail "liveness and startup probes must use the journal-only check"
grep -q 'type: Recreate' "$tmp/edge.yaml" || fail "the Edge must use the Recreate strategy"
if helm template thinpost "$chart" --set workload=edge --set replicas=2 >/dev/null 2>&1; then fail "accepted more than one Edge replica"; fi

# The schema owner credential must never reach the Deployment.
name=control
helm template thinpost "$chart" -n thinpost-control --set workload=control --set control.image=$image \
  --show-only templates/control-deployment.yaml >"$tmp/$name-deployment.yaml"
grep -q 'kind: Deployment' "$tmp/$name-deployment.yaml" || fail "$name Deployment did not render"
if grep -Eq 'db-owner|thinpost-db-owner' "$tmp/$name-deployment.yaml"; then
  fail "$name Deployment references the owner credential"
fi

# The database CA comes from the cluster trust bundle ConfigMap, never a Secret:
# one volume in the Deployment and in each of the two Jobs, no subPath.
[ "$(grep -c 'name: "thinpost-db-ca"' "$tmp/control.yaml")" = 3 ] || fail "thinpost-db-ca ConfigMap volume must be in the Deployment and both Jobs"
grep -q 'name: "thinpost-db-ca"' "$tmp/control-deployment.yaml" || fail "Deployment lacks the thinpost-db-ca volume"
if grep -rq 'secretName: .*db-ca' "$tmp"; then fail "the database CA must not be mounted from a Secret"; fi
grep -q 'secret.reloader.stakater.com/reload: thinpost-control-client' "$tmp/control-deployment.yaml" || fail "operator Deployment must restart when thinpost-control-client renews"
helm template thinpost "$chart" -n thinpost-control --set workload=control --set control.image=$image --set control.reloader.enabled=false \
  --show-only templates/control-deployment.yaml >"$tmp/control-noreloader.yaml" || fail "control.reloader.enabled=false must render"
if grep -q 'reloader.stakater.com' "$tmp/control-noreloader.yaml"; then fail "reloader annotation must be absent when control.reloader.enabled=false"; fi
if grep -q 'annotations:' "$tmp/control-noreloader.yaml" && [ "$(awk '/^metadata:/{f=1;next} /^spec:/{f=0} f&&/annotations:/{c++} END{print c+0}' "$tmp/control-noreloader.yaml")" != 0 ]; then fail "Deployment metadata must have no annotations block when none are set"; fi
# Deployment annotations: none beyond the reloader by default; extra ones render beside it or alone.
if grep -q 'kustomize.toolkit' "$tmp/control-deployment.yaml"; then fail "no extra Deployment annotation by default"; fi
helm template thinpost "$chart" -n thinpost-control --set workload=control --set control.image=$image \
  --set 'control.deploymentAnnotations.example\.test/keep=disabled' --show-only templates/control-deployment.yaml >"$tmp/control-annot.yaml" || fail "control.deploymentAnnotations must render"
grep -q 'example.test/keep: disabled' "$tmp/control-annot.yaml" || fail "deployment annotation missing"
grep -q 'secret.reloader.stakater.com/reload: thinpost-control-client' "$tmp/control-annot.yaml" || fail "reloader annotation lost beside a custom annotation"
helm template thinpost "$chart" -n thinpost-control --set workload=control --set control.image=$image --set control.reloader.enabled=false \
  --set 'control.deploymentAnnotations.example\.test/keep=disabled' --show-only templates/control-deployment.yaml >"$tmp/control-annot2.yaml" || fail "annotations without reloader must render"
grep -q 'example.test/keep: disabled' "$tmp/control-annot2.yaml" || fail "deployment annotation missing without reloader"
if grep -q 'reloader.stakater.com' "$tmp/control-annot2.yaml"; then fail "reloader annotation present though disabled"; fi
helm template thinpost "$chart" --set workload=control --set control.image=$image --set database.caConfigMap=other-ca | grep -q 'name: "other-ca"' || fail "database.caConfigMap not honoured"
if helm template thinpost "$chart" --set workload=control --set control.image=$image --set database.caConfigMap= >/dev/null 2>&1; then fail "accepted an empty database.caConfigMap"; fi

# Control TLS listener: off by default; when on, a whole-directory Secret mount,
# env, container port, and no restart-on-renewal annotation for it.
for needle in THINPOST_CONTROL_TLS control-tls 'name: https'; do
  if grep -q "$needle" "$tmp/control-deployment.yaml"; then fail "default control render must not contain $needle"; fi
done
helm template thinpost "$chart" -n thinpost-control --set workload=control --set control.image=$image --set control.tls.enabled=true \
  --show-only templates/control-deployment.yaml >"$tmp/control-tls.yaml" || fail "control.tls.enabled must render"
grep -q 'name: THINPOST_CONTROL_TLS_LISTEN' "$tmp/control-tls.yaml" && grep -q 'value: ":8443"' "$tmp/control-tls.yaml" || fail "TLS listen env"
grep -q 'value: /run/thinpost/control-tls/tls.crt$' "$tmp/control-tls.yaml" || fail "TLS cert env"
grep -q 'value: /run/thinpost/control-tls/tls.key$' "$tmp/control-tls.yaml" || fail "TLS key env"
grep -q 'containerPort: 8443' "$tmp/control-tls.yaml" && grep -q 'name: https' "$tmp/control-tls.yaml" || fail "https container port"
grep -q 'containerPort: 8080' "$tmp/control-tls.yaml" || fail "plain port must remain"
grep -q 'mountPath: /run/thinpost/control-tls$' "$tmp/control-tls.yaml" || fail "TLS Secret mount"
grep -q 'secretName: "thinpost-operator-tls"' "$tmp/control-tls.yaml" || fail "TLS Secret name"
if grep -q 'subPath' "$tmp/control-tls.yaml"; then fail "TLS Secret must not use subPath"; fi
grep -q 'path: /operator-readyz' "$tmp/control-tls.yaml" || fail "readiness stays on the plain listener"
helm template thinpost "$chart" -n thinpost-control --set workload=control --set control.image=$image --set control.tls.enabled=true \
  --set control.tls.port=9443 --set control.tls.secretName=other-tls --show-only templates/control-deployment.yaml >"$tmp/control-tls2.yaml" || fail "custom TLS values"
grep -q 'value: ":9443"' "$tmp/control-tls2.yaml" && grep -q 'secretName: "other-tls"' "$tmp/control-tls2.yaml" || fail "custom TLS port/secret not honoured"
for bad in control.tls.port=8080 control.tls.port=0 control.tls.secretName= control.tls.secretName=Bad_Name; do
  if helm template thinpost "$chart" --set workload=control --set control.image=$image --set control.tls.enabled=true --set "$bad" >/dev/null 2>&1; then fail "accepted invalid $bad"; fi
done
# Internal webhook exceptions: empty by default (no env at all), rendered only on
# the control Deployment, joined with commas, and never without Service CIDRs.
if grep -q 'THINPOST_WEBHOOK_INTERNAL_DESTINATIONS\|THINPOST_SERVICE_CIDRS' "$tmp/control.yaml" "$tmp/edge.yaml"; then fail "internal webhook env must be absent by default"; fi
hook=http://hooks.mail.svc.cluster.local/events
helm template thinpost "$chart" --set workload=control --set control.image=$image \
  --set "control.webhookInternalDestinations[0]=$hook" --set 'control.webhookInternalDestinations[1]=http://other.mail.svc.cluster.local:3000/e?x=1' \
  --set 'control.serviceCidrs[0]=10.96.0.0/12' --set 'control.serviceCidrs[1]=fd00:10:96::/112' >"$tmp/internal.yaml" || fail "internal webhook values must render"
grep -q 'name: THINPOST_WEBHOOK_INTERNAL_DESTINATIONS' "$tmp/internal.yaml" || fail "destinations env"
grep -q 'value: "http://hooks.mail.svc.cluster.local/events,http://other.mail.svc.cluster.local:3000/e?x=1"' "$tmp/internal.yaml" || fail "destinations env value"
grep -q 'name: THINPOST_SERVICE_CIDRS' "$tmp/internal.yaml" || fail "Service CIDRs env"
grep -q 'value: "10.96.0.0/12,fd00:10:96::/112"' "$tmp/internal.yaml" || fail "Service CIDRs env value"
[ "$(grep -c 'name: THINPOST_WEBHOOK_INTERNAL_DESTINATIONS' "$tmp/internal.yaml")" = 1 ] || fail "destinations env must be on the Deployment only"
# An events path may start with "@"; a path "@" is not userinfo.
zhhook='http://hooks.mail.svc.cluster.local/@app/events'
helm template thinpost "$chart" --set workload=control --set control.image=$image --set "control.webhookInternalDestinations[0]=$zhhook" --set 'control.serviceCidrs[0]=10.96.0.0/12' >/dev/null || fail "rejected an internal destination whose path starts with @"
if helm template thinpost "$chart" --set workload=control --set control.image=$image --set 'control.webhookInternalDestinations[0]=http://user@hooks.mail.svc.cluster.local/x' --set 'control.serviceCidrs[0]=10.96.0.0/12' >/dev/null 2>&1; then fail "accepted userinfo in an internal destination"; fi
if helm template thinpost "$chart" --set workload=control --set control.image=$image --set "control.webhookInternalDestinations[0]=$hook" >/dev/null 2>&1; then fail "accepted destinations without Service CIDRs"; fi
for bad in 'ftp://hooks.mail.svc/e' 'http://user@hooks.mail.svc/e' 'http://Hooks.mail.svc/e' 'http://hooks.mail.svc/a\,b' 'http://hooks.mail.svc/e#f' 'hooks.mail.svc/e' 'http://hooks.mail.svc/e' 'http://hooks.example.com/e' 'http://mail.svc.cluster.local/e' 'http://hooks.mail.svc.cluster.local.evil.example/e'; do
  if helm template thinpost "$chart" --set workload=control --set control.image=$image --set "control.webhookInternalDestinations[0]=$bad" --set 'control.serviceCidrs[0]=10.96.0.0/12' >/dev/null 2>&1; then fail "accepted destination $bad"; fi
done
if helm template thinpost "$chart" --set workload=control --set control.image=$image --set 'control.serviceCidrs[0]=not-a-cidr' >/dev/null 2>&1; then fail "accepted an invalid Service CIDR"; fi

# SMTP submission: off by default (no env, ports, Service or volume). Enabled, it
# opens 587 and 465 on the existing per-IP certificates; the internal listener
# adds its own env, a whole-directory Secret mount, a ClusterIP Service and a
# NetworkPolicy ingress rule for the configured clients only.
for needle in THINPOST_EDGE_SUBMISSION 'containerPort: 587' 'containerPort: 465' submission-internal thinpost-edge-submission; do
  if grep -q "$needle" "$tmp/edge.yaml"; then fail "the default Edge render must not contain $needle"; fi
done
render edge_submission --set workload=edge --set smtpSubmission.enabled=true --set 'additionalSources[0].address=203.0.113.10' --set 'additionalSources[0].hostname=smtp.hosting.example'
grep -A1 'name: THINPOST_EDGE_SUBMISSION$' "$tmp/edge_submission.yaml" | grep -q 'value: "true"' || fail "submission env"
grep -q 'containerPort: 587' "$tmp/edge_submission.yaml" && grep -q 'containerPort: 465' "$tmp/edge_submission.yaml" || fail "submission container ports"
if grep -q 'THINPOST_EDGE_SUBMISSION_INTERNAL\|kind: Service' "$tmp/edge_submission.yaml"; then fail "public submission must not render the internal listener or a Service"; fi
internal='smtpSubmission.internal'
render edge_internal --set workload=edge --set "$internal.enabled=true" --set "$internal.hostname=thinpost-edge-submission.mail.svc" \
  --set "$internal.clientCidrs[0]=10.0.0.0/8" --set "$internal.clientNamespaceSelector.matchLabels.team=apps"
if grep -q 'name: THINPOST_EDGE_SUBMISSION$' "$tmp/edge_internal.yaml"; then fail "the internal listener alone must not open the public ports"; fi
grep -A1 'name: THINPOST_EDGE_SUBMISSION_INTERNAL_LISTEN' "$tmp/edge_internal.yaml" | grep -q 'value: "$(POD_IP):587"' || fail "internal listen env"
grep -A1 'name: THINPOST_EDGE_SUBMISSION_INTERNAL_HOSTNAME' "$tmp/edge_internal.yaml" | grep -q 'value: "thinpost-edge-submission.mail.svc"' || fail "internal hostname env"
grep -q 'value: /run/thinpost/submission-internal/ca.crt$' "$tmp/edge_internal.yaml" || fail "internal CA env"
grep -q 'mountPath: /run/thinpost/submission-internal$' "$tmp/edge_internal.yaml" || fail "internal certificate mount"
grep -q 'secretName: "thinpost-edge-submission-internal"' "$tmp/edge_internal.yaml" || fail "internal certificate Secret"
if grep -q 'subPath' "$tmp/edge_internal.yaml"; then fail "internal certificate volume must not use subPath"; fi
[ "$(grep -c '^kind: Service$' "$tmp/edge_internal.yaml")" = 1 ] || fail "exactly one submission Service"
grep -q 'name: thinpost-edge-submission$' "$tmp/edge_internal.yaml" && grep -q 'type: ClusterIP' "$tmp/edge_internal.yaml" || fail "submission Service"
grep -q 'targetPort: submission-int' "$tmp/edge_internal.yaml" || fail "submission Service target"
grep -q 'cidr: "10.0.0.0/8"' "$tmp/edge_internal.yaml" && grep -q 'team: apps' "$tmp/edge_internal.yaml" || fail "internal ingress sources"
grep -q 'port: 587' "$tmp/edge_internal.yaml" || fail "internal ingress port"
render edge_internal_port --set workload=edge --set "$internal.enabled=true" --set "$internal.hostname=thinpost-edge-submission.mail.svc" --set "$internal.clientCidrs[0]=10.0.0.0/8" --set "$internal.port=2587"
grep -A1 'name: THINPOST_EDGE_SUBMISSION_INTERNAL_LISTEN' "$tmp/edge_internal_port.yaml" | grep -q 'value: "$(POD_IP):2587"' || fail "internal port env"
grep -q 'port: 587$' "$tmp/edge_internal_port.yaml" && grep -q 'targetPort: submission-int' "$tmp/edge_internal_port.yaml" || fail "Service port stays 587"
helm lint "$chart" --set workload=edge --set smtpSubmission.enabled=true --set "$internal.enabled=true" --set "$internal.hostname=thinpost-edge-submission.mail.svc" --set "$internal.clientCidrs[0]=10.0.0.0/8"
for bad in "$internal.hostname=Bad.Name" "$internal.hostname=" "$internal.port=25" "$internal.port=7443" "$internal.port=70000" "$internal.certificateSecret=Bad_Name" "$internal.clientCidrs[0]=not-a-cidr"; do
  if helm template thinpost "$chart" --set workload=edge --set "$internal.enabled=true" --set "$internal.hostname=thinpost-edge-submission.mail.svc" --set "$internal.clientCidrs[0]=10.0.0.0/8" --set "$bad" >/dev/null 2>&1; then fail "accepted invalid $bad"; fi
done
if helm template thinpost "$chart" --set workload=edge --set "$internal.enabled=true" --set "$internal.hostname=thinpost-edge-submission.mail.svc" >/dev/null 2>&1; then fail "accepted the internal listener with no allowed clients"; fi
if helm template thinpost "$chart" --set workload=edge --set smtpSubmission.enabled=yes >/dev/null 2>&1; then fail "accepted a non-boolean smtpSubmission.enabled"; fi
if helm template thinpost "$chart" --set workload=control --set control.image=$image --set smtpSubmission.enabled=true | grep -q 'THINPOST_EDGE_SUBMISSION'; then fail "control must not render the Edge submission settings"; fi

# Edge and every disabled variant render no Job at all.
for name in edge disabled scaled_down bootstrap; do
  if grep -q 'kind: Job' "$tmp/$name.yaml"; then fail "$name render contains a Job"; fi
  if grep -Eq 'db-owner' "$tmp/$name.yaml"; then fail "$name render references the owner credential"; fi
done

# The control render has exactly the two hook Jobs, with the hook annotations.
[ "$(grep -c '^kind: Job$' "$tmp/control.yaml")" = 2 ] || fail "control render must contain exactly two Jobs"
grep -q 'helm.sh/hook: pre-install,pre-upgrade' "$tmp/control.yaml" || fail "missing pre hook annotation"
grep -q 'helm.sh/hook: post-install,post-upgrade' "$tmp/control.yaml" || fail "missing post hook annotation"
[ "$(grep -c 'helm.sh/hook-delete-policy: before-hook-creation' "$tmp/control.yaml")" = 2 ] || fail "hooks need before-hook-creation"
grep -q -- '- "deploy-migrate"' "$tmp/control.yaml" || fail "missing deploy-migrate command"
grep -q -- '- "deploy-reopen"' "$tmp/control.yaml" || fail "missing deploy-reopen command"

# The owner secret is mounted by the two Jobs only.
[ "$(grep -c 'secretName: thinpost-db-owner' "$tmp/control.yaml")" = 2 ] || fail "owner secret must be mounted by exactly the two Jobs"
# Job pods carry the migration label and never the operator label.
[ "$(grep -c 'app.kubernetes.io/name: thinpost-migrate' "$tmp/control.yaml")" = 2 ] || fail "Job pods need the migrate label"
[ "$(grep -c 'app.kubernetes.io/name: thinpost-operator' "$tmp/control.yaml")" = "$(grep -c 'app.kubernetes.io/name: thinpost-operator' "$tmp/control-deployment.yaml")" ] || fail "operator label leaked into another resource"
# Hardening matches the Deployment.
[ "$(grep -c 'readOnlyRootFilesystem: true' "$tmp/control.yaml")" = 3 ] || fail "read-only root filesystem"
[ "$(grep -c 'runAsNonRoot: true' "$tmp/control.yaml")" = 3 ] || fail "non-root"
[ "$(grep -c 'automountServiceAccountToken: false' "$tmp/control.yaml")" = 3 ] || fail "service account token automount"
if grep -q 'imagePullSecrets' "$tmp/control.yaml" "$tmp/edge.yaml"; then fail "imagePullSecrets must be absent by default"; fi
render control_pull --set workload=control --set control.image=$image --set 'imagePullSecrets[0].name=regcred'
[ "$(grep -c 'name: regcred' "$tmp/control_pull.yaml")" = 3 ] || fail "pull secret on the Deployment and both Jobs"
[ "$(grep -c '^      imagePullSecrets:$' "$tmp/control_pull.yaml")" = 3 ] || fail "pull secret list on every control pod spec"
render edge_pull --set workload=edge --set 'imagePullSecrets[0].name=regcred'
[ "$(grep -c 'name: regcred' "$tmp/edge_pull.yaml")" = 1 ] || fail "pull secret on the Edge pod"
[ "$(grep -c 'backoffLimit: 0' "$tmp/control.yaml")" = 2 ] || fail "Jobs must not retry"
# Only the reopen Job holds a Kubernetes API token.
[ "$(grep -c 'serviceAccountToken:' "$tmp/control.yaml")" = 1 ] || fail "exactly one projected token"
# The Deployment and both Jobs render an identical image string.
[ "$(grep -c "image: \"$image\"" "$tmp/control.yaml")" = 3 ] || fail "image strings differ"
grep -q -- "--image=$image" "$tmp/control.yaml" || fail "reopen Job image flag differs"
# One actor per revision, shared by both hooks.
[ "$(grep -o -- '--actor=deploy:thinpost-control/thinpost:r1' "$tmp/control.yaml" | wc -l | tr -d ' ')" = 2 ] || fail "hook actors differ"

if helm template thinpost "$chart" --set workload=control --set control.image=example.invalid/control:test --set-string 'control.deploymentAnnotations.secret\.reloader\.stakater\.com/reload=a' >/dev/null 2>&1; then fail "accepted a duplicate reloader annotation while the reloader is enabled"; fi
helm template thinpost "$chart" --set workload=control --set control.image=example.invalid/control:test --set control.reloader.enabled=false --set-string 'control.deploymentAnnotations.secret\.reloader\.stakater\.com/reload=a' >/dev/null || fail "a custom reloader annotation must render when the built-in one is disabled"
if helm template thinpost "$chart" --set workload=edge --set publicNetwork.cniTypes.sbr=null >/dev/null 2>&1; then fail "accepted a null CNI type"; fi
echo "chart assertions passed"
