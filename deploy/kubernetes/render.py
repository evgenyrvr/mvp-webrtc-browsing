#!/usr/bin/env python3
"""Render reviewable Kubernetes JSON (kubectl accepts JSON). No cluster writes."""
import ipaddress
import json
import re
import sys
from urllib.parse import urlsplit


def render(c):
    ns = c['namespace']
    if not re.fullmatch(r'[a-z][a-z0-9-]{0,30}', ns):
        raise ValueError('invalid namespace')
    for key in ('control_image', 'worker_image', 'storage_class', 'turn_node_name', 'image_pull_secret'):
        if not c[key] or any(ch.isspace() for ch in c[key]):
            raise ValueError('invalid ' + key)
    origin = urlsplit(c['origin'])
    if origin.scheme != 'https' or not origin.hostname or origin.path or origin.query or origin.fragment or origin.username:
        raise ValueError('origin must be an HTTPS origin without path')
    if not c['turn_url'].startswith(('turn:', 'turns:')):
        raise ValueError('invalid TURN URL')
    turn_ips = list(dict.fromkeys(str(ipaddress.IPv4Address(c[k])) for k in ('turn_public_ip', 'turn_node_ip')))
    app_cidrs = [str(ipaddress.IPv4Network(v, strict=True)) for v in c['application_cidrs']]
    worker_nets = [ipaddress.IPv4Network(v, strict=True) for v in c['worker_pod_cidrs']]
    if not worker_nets:
        raise ValueError('worker_pod_cidrs required to restrict TURN peers')
    if any(ipaddress.ip_network(a).overlaps(w) for a in app_cidrs for w in worker_nets):
        raise ValueError('application_cidrs must not open access to worker Pod networks')
    items = []
    def add(kind, name, spec=None, api='v1', **fields):
        obj = dict(apiVersion=api, kind=kind, metadata=dict(name=name, namespace=ns), **fields)
        if spec is not None:
            obj['spec'] = spec
        items.append(obj)
        return obj
    items.append(dict(apiVersion='v1', kind='Namespace', metadata=dict(name=ns, labels={'pod-security.kubernetes.io/enforce': 'baseline'})))
    add('ServiceAccount', 'mvp1-runner', automountServiceAccountToken=False)
    add('ServiceAccount', 'mvp1-session', automountServiceAccountToken=False,
        imagePullSecrets=[dict(name=c['image_pull_secret'])])
    add('Role', 'mvp1-runner', api='rbac.authorization.k8s.io/v1', rules=[dict(apiGroups=[''], resources=['pods'], verbs=['get', 'list', 'create', 'delete'])])
    add('RoleBinding', 'mvp1-runner', api='rbac.authorization.k8s.io/v1',
        subjects=[dict(kind='ServiceAccount', name='mvp1-runner', namespace=ns)],
        roleRef=dict(apiGroup='rbac.authorization.k8s.io', kind='Role', name='mvp1-runner'))
    add('PersistentVolumeClaim', 'mvp1-db', dict(accessModes=['ReadWriteOnce'], volumeMode='Filesystem', storageClassName=c['storage_class'], resources=dict(requests=dict(storage='1Gi'))))
    def secret_env(name, key, secret='mvp1-secrets'):
        return dict(name=name, valueFrom=dict(secretKeyRef=dict(name=secret, key=key)))
    turn_env = [dict(name='MVP_TURN_URL', value=c['turn_url']), secret_env('MVP_TURN_SECRET', 'turn-secret')]
    security = dict(runAsNonRoot=True, runAsUser=10001, runAsGroup=10001, allowPrivilegeEscalation=False, readOnlyRootFilesystem=True, capabilities=dict(drop=['ALL']))
    def resources(memory):
        return dict(requests=dict(cpu='100m', memory=memory), limits=dict(cpu='500m', memory='256Mi'))
    runner = dict(name='runner', image=c['control_image'], command=['/usr/local/bin/runner'], args=[
        '-backend', 'kubernetes', '-namespace', ns, '-socket', '/run/mvp1/runner.sock', '-image', c['worker_image'], '-image-pull-secret', c['image_pull_secret']],
        env=turn_env, securityContext=security, resources=resources('64Mi'), volumeMounts=[
            dict(name='socket', mountPath='/run/mvp1'), dict(name='api', mountPath='/var/run/secrets/kubernetes.io/serviceaccount', readOnly=True)])
    gateway = dict(name='gateway', image=c['control_image'], command=['/bin/sh', '-ec'], args=[
        'n=0; while [ ! -S /run/mvp1/runner.sock ]; do n=$((n+1)); [ "$n" -lt 100 ] || exit 1; sleep 1; done; exec /usr/local/bin/gateway "$@"',
        'gateway', '-addr', '0.0.0.0:8443', '-origin', c['origin'], '-db', '/data/mvp1.db', '-runner-socket', '/run/mvp1/runner.sock',
        '-tls-cert', '/tls/tls.crt', '-tls-key', '/tls/tls.key', '-relay-only'],
        env=turn_env+[secret_env('MVP_ADMIN_LOGIN', 'admin-login'), secret_env('MVP_ADMIN_PASSWORD', 'admin-password')],
        securityContext=security, resources=resources('128Mi'), ports=[dict(name='https', containerPort=8443)],
        volumeMounts=[dict(name='socket', mountPath='/run/mvp1'), dict(name='db', mountPath='/data'), dict(name='tls', mountPath='/tls', readOnly=True)],
        startupProbe=dict(httpGet=dict(path='/readyz', port=8443, scheme='HTTPS'), periodSeconds=2, failureThreshold=60),
        readinessProbe=dict(httpGet=dict(path='/readyz', port=8443, scheme='HTTPS'), periodSeconds=5),
        livenessProbe=dict(httpGet=dict(path='/healthz', port=8443, scheme='HTTPS'), periodSeconds=10, failureThreshold=3))
    add('Deployment', 'mvp1-control', dict(replicas=1, strategy=dict(type='Recreate'), selector=dict(matchLabels={'app': 'mvp1-control'}), template=dict(
        metadata=dict(labels={'app': 'mvp1-control'}), spec=dict(serviceAccountName='mvp1-runner', automountServiceAccountToken=False,
        imagePullSecrets=[dict(name=c['image_pull_secret'])], nodeSelector={'kubernetes.io/os': 'linux'}, terminationGracePeriodSeconds=60,
        securityContext=dict(fsGroup=10001, seccompProfile=dict(type='RuntimeDefault')), containers=[gateway, runner], volumes=[
            dict(name='socket', emptyDir={}), dict(name='db', persistentVolumeClaim=dict(claimName='mvp1-db')), dict(name='tls', secret=dict(secretName='mvp1-tls')),
            dict(name='api', projected=dict(defaultMode=0o440, sources=[dict(serviceAccountToken=dict(path='token', expirationSeconds=3600)),
                dict(configMap=dict(name='kube-root-ca.crt', items=[dict(key='ca.crt', path='ca.crt')]))]))]))), api='apps/v1')
    # Pull browser images before serving users; avoid holding a launch open while downloading Qt.
    add('DaemonSet','mvp1-prepull',dict(selector=dict(matchLabels={'app':'mvp1-prepull'}),template=dict(
        metadata=dict(labels={'app':'mvp1-prepull'}),spec=dict(automountServiceAccountToken=False,
        imagePullSecrets=[dict(name=c['image_pull_secret'])],nodeSelector={'kubernetes.io/os':'linux'},
        securityContext=dict(runAsNonRoot=True,runAsUser=10001,runAsGroup=10001,seccompProfile=dict(type='RuntimeDefault')),
        initContainers=[dict(name='pull-worker',image=c['worker_image'],imagePullPolicy='IfNotPresent',command=['/bin/true'],securityContext=security,
            resources=dict(requests=dict(cpu='10m',memory='16Mi'),limits=dict(cpu='100m',memory='32Mi')))],
        containers=[dict(name='hold',image=c['control_image'],command=['/bin/sleep','infinity'],securityContext=security,
            resources=dict(requests=dict(cpu='1m',memory='8Mi'),limits=dict(cpu='10m',memory='16Mi')))]))),api='apps/v1')
    add('Service', 'mvp1', dict(type='ClusterIP', selector={'app': 'mvp1-control'}, ports=[dict(name='https', port=443, targetPort=8443)]))
    # TURN has one stable node IP; the operator exposes these ports and routes all Pod CIDRs.
    turn_args = ['exec turnserver --log-file=stdout --simple-log --fingerprint --use-auth-secret --static-auth-secret="$MVP_TURN_SECRET" '
        '--realm=mvp1 --listening-ip="$NODE_IP" --relay-ip="$NODE_IP" --external-ip="$TURN_PUBLIC_IP/$NODE_IP" '
        '--listening-port=3478 --tls-listening-port=5349 --min-port=49160 --max-port=49259 '
        '--cert=/tls/tls.crt --pkey=/tls/tls.key --no-cli --no-dtls --no-multicast-peers --no-tcp-relay '
        '--user-quota=16 --total-quota=64 --relay-threads=2']
    add('Deployment', 'mvp1-turn', dict(replicas=1, strategy=dict(type='Recreate'), selector=dict(matchLabels={'app': 'mvp1-turn'}), template=dict(
        metadata=dict(labels={'app': 'mvp1-turn'}), spec=dict(hostNetwork=True, dnsPolicy='ClusterFirstWithHostNet', automountServiceAccountToken=False,
        nodeSelector={'kubernetes.io/hostname': c['turn_node_name'], 'kubernetes.io/os': 'linux'}, containers=[dict(name='coturn',
        image='coturn/coturn:4.18.0-r0@sha256:bbefd3e1fdfdc0d58770fe01b581fd8b00d9f3a5580d00acb77cf719a6bc78e3',
        command=['/bin/sh','-ec'], args=turn_args, resources=dict(requests=dict(cpu='100m',memory='64Mi'),limits=dict(cpu='1000m',memory='256Mi')),
        env=[secret_env('MVP_TURN_SECRET','turn-secret'),dict(name='TURN_PUBLIC_IP',value=c['turn_public_ip']),dict(name='NODE_IP',valueFrom=dict(fieldRef=dict(fieldPath='status.hostIP')))],
        securityContext=dict(runAsUser=65534,runAsGroup=65534,runAsNonRoot=True,allowPrivilegeEscalation=False,readOnlyRootFilesystem=True,capabilities=dict(drop=['ALL'],add=['NET_BIND_SERVICE'])),
        volumeMounts=[dict(name='tmp',mountPath='/tmp'),dict(name='tls',mountPath='/tls',readOnly=True)],
        readinessProbe=dict(tcpSocket=dict(port=3478),periodSeconds=5))],volumes=[dict(name='tmp',emptyDir=dict(medium='Memory',sizeLimit='16Mi')),dict(name='tls',secret=dict(secretName='mvp1-turn-tls'))]))),api='apps/v1')
    peer_rules = ['--denied-peer-ip=0.0.0.0-255.255.255.255']
    peer_rules += ['--allowed-peer-ip='+str(n.network_address)+'-'+str(n.broadcast_address) for n in worker_nets]
    peer_rules += ['--allowed-peer-ip='+ip for ip in turn_ips]
    turn_args[0] += ' ' + ' '.join(peer_rules)
    # hostNetwork requires an explicit namespace exception; do not label a whole namespace privileged implicitly.
    # A separate TURN namespace is used, so session/control namespace can keep baseline admission.
    turn = items[-1]
    turn_ns = ns + '-turn'
    turn['metadata']['namespace'] = turn_ns
    items.insert(1,dict(apiVersion='v1',kind='Namespace',metadata=dict(name=turn_ns)))
    peers = [dict(ipBlock=dict(cidr=ip+'/32')) for ip in turn_ips]
    worker_selector = {'app.kubernetes.io/component': 'session'}
    add('NetworkPolicy','mvp1-sessions',dict(podSelector=dict(matchLabels=worker_selector),policyTypes=['Ingress','Egress'],
        ingress=[dict(**{'from':[dict(podSelector=dict(matchLabels={'app':'mvp1-control'}))]},ports=[dict(protocol='TCP',port=8081)]),
                 dict(**{'from':peers},ports=[dict(protocol='UDP',port=50000)])],
        egress=[dict(to=[dict(namespaceSelector=dict(matchLabels={'kubernetes.io/metadata.name':'kube-system'}),podSelector=dict(matchLabels={'k8s-app':'kube-dns'}))],ports=[dict(protocol='UDP',port=53),dict(protocol='TCP',port=53)]),
                dict(to=peers,ports=[dict(protocol='UDP',port=3478),dict(protocol='TCP',port=3478),dict(protocol='TCP',port=5349),dict(protocol='UDP',port=49160,endPort=49259)])]+
               [dict(to=[dict(ipBlock=dict(cidr=cidr))],ports=[dict(protocol='TCP',port=80),dict(protocol='TCP',port=443)]) for cidr in app_cidrs]),api='networking.k8s.io/v1')
    return dict(apiVersion='v1',kind='List',items=items)

if __name__ == '__main__':
    with open(sys.argv[1], encoding='utf-8') as f:
        config=json.load(f)
    print(json.dumps(render(config),ensure_ascii=False,indent=2))
