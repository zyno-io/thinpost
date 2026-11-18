#!/usr/bin/env python3
"""Test synthetic Edge TLS rotation using a disposable Kubernetes Secret.

Creates a non-root loopback-only fixture pod, a default-deny network policy,
and a Secret in the explicitly selected namespace. No deployment keys, public
mail, or Thinpost workload are used. Requires go, kubectl, and access to the
target namespace. Allow up to nine minutes for kubelet Secret projection.
"""
import argparse
import base64
import json
import os
import pathlib
import select
import subprocess
import tempfile
import time
import uuid

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--context', required=True)
parser.add_argument('--namespace', required=True)
parser.add_argument('--node', help='optional Kubernetes hostname selector')
args = parser.parse_args()
root = pathlib.Path(__file__).resolve().parents[1]
ctx = ['kubectl', '--context', args.context, '-n', args.namespace]
name = 'thinpost-projection-' + uuid.uuid4().hex[:10]

def run(args, **kw):
    result = subprocess.run(args, cwd=root, capture_output=True, **kw)
    if result.returncode:
        # Do not print external tool output, which may include Secret payloads.
        raise RuntimeError(f'{args[0]} failed (exit {result.returncode})')
    return result

def apply(obj):
    run(ctx + ['apply', '-f', '-'], input=json.dumps(obj).encode())

def secret(files, phase, mismatch=False, rotated=False):
    pairs = {'ca.crt': 'ca.crt', 'client.crt': 'client.crt', 'client.key': 'client.key',
             'edge.crt': 'edge-next.crt' if mismatch or rotated else 'edge.crt',
             'edge.key': 'edge-next.key' if rotated else 'edge.key',
             'smtp.crt': 'smtp-next.crt' if mismatch or rotated else 'smtp.crt',
             'smtp.key': 'smtp-next.key' if rotated else 'smtp.key'}
    data = {k: base64.b64encode((files / v).read_bytes()).decode() for k, v in pairs.items()}
    data['phase'] = base64.b64encode(phase.encode()).decode()
    apply({'apiVersion':'v1', 'kind':'Secret', 'metadata':{'name':name}, 'data':data})

try:
    with tempfile.TemporaryDirectory(prefix='thinpost-projection-') as td:
        files = pathlib.Path(td)
        env = os.environ.copy()
        env['THINPOST_TEST_EDGE_IMAGE_GENERATE'] = td
        run(['go','test','./internal/controlsession','-run','^TestEdgeImageGenerateFixtureCertificates$','-count=1'], env=env)
        env.update({'GOOS':'linux','GOARCH':'amd64','CGO_ENABLED':'0'})
        run(['go','test','-c','-o',str(files / 'fixture.test'),'./cmd/thinpost-edge'], env=env)
        secret(files, 'initial')
        labels = {'app':'thinpost-projection-fixture', 'fixture':name}
        apply({'apiVersion':'networking.k8s.io/v1','kind':'NetworkPolicy','metadata':{'name':name},'spec':{'podSelector':{'matchLabels':labels},'policyTypes':['Ingress','Egress']}})
        pod = {'apiVersion':'v1','kind':'Pod','metadata':{'name':name,'labels':labels},'spec':{
            'automountServiceAccountToken':False, 'restartPolicy':'Never',
            'nodeSelector':{'kubernetes.io/arch':'amd64'},
            'securityContext':{'runAsNonRoot':True,'runAsUser':10001,'runAsGroup':10001,'fsGroup':10001,'seccompProfile':{'type':'RuntimeDefault'}},
            'containers':[{'name':'fixture','image':'alpine:3.22','command':['sleep','900'],
                'resources':{'requests':{'cpu':'25m','memory':'32Mi'},'limits':{'cpu':'500m','memory':'256Mi'}},
                'securityContext':{'allowPrivilegeEscalation':False,'readOnlyRootFilesystem':True,'capabilities':{'drop':['ALL']}},
                'volumeMounts':[{'name':'certs','mountPath':'/fixture','readOnly':True},{'name':'work','mountPath':'/work'}]}],
            'volumes':[{'name':'certs','secret':{'secretName':name,'defaultMode':288}},{'name':'work','emptyDir':{'sizeLimit':'64Mi'}}]}}
        if args.node:
            pod['spec']['nodeSelector']['kubernetes.io/hostname'] = args.node
        apply(pod)
        run(ctx + ['wait','--for=condition=Ready','pod/'+name,'--timeout=120s'])
        run(ctx + ['exec','-i',name,'--','sh','-c','cat > /work/fixture && chmod 755 /work/fixture'], input=(files/'fixture.test').read_bytes())
        cmd = ctx + ['exec',name,'--','env','THINPOST_TEST_PROJECTED_CERTIFICATES=/fixture','/work/fixture','-test.v','-test.timeout=9m','-test.run=^TestProjectedCertificateRotation$']
        process = subprocess.Popen(cmd, cwd=root, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, bufsize=1)
        assert process.stdout is not None
        seen = set()
        deadline = time.monotonic() + 560
        while time.monotonic() < deadline:
            ready, _, _ = select.select([process.stdout], [], [], 2)
            if ready:
                line = process.stdout.readline()
                if line:
                    print(line, end='', flush=True)
                    if 'PROJECTION_INITIAL_OK' in line and 'initial' not in seen:
                        seen.add('initial'); secret(files,'mismatched',mismatch=True); print('Published synthetic mismatched keypairs',flush=True)
                    if 'PROJECTION_MISMATCH_REJECTED' in line and 'mismatch' not in seen:
                        seen.add('mismatch'); secret(files,'rotated',rotated=True); print('Published synthetic renewed keypairs',flush=True)
                    if 'PROJECTION_ROTATED_OK' in line: seen.add('rotated')
                elif process.poll() is not None:
                    break
            elif process.poll() is not None:
                break
        if process.poll() is None:
            process.kill(); process.wait(); raise RuntimeError('projection fixture timed out')
        if process.returncode or seen != {'initial','mismatch','rotated'}:
            raise RuntimeError(f'fixture failed: exit={process.returncode}, phases={seen}')
        print('Kubernetes Secret projection fixture passed', flush=True)
finally:
    cleanup = subprocess.run(ctx + ['delete','pod/'+name,'secret/'+name,'networkpolicy/'+name,'--ignore-not-found=true','--wait=true','--timeout=90s'],capture_output=True)
    if cleanup.returncode:
        raise RuntimeError('Cleanup failed for temporary resources named '+name)
    else:
        print('Deleted temporary fixture resources '+name,flush=True)
