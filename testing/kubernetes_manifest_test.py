"""Offline checks of deployment boundaries, not Kubernetes schema validation."""
import importlib.util
import json
import pathlib
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('renderer', ROOT / 'deploy/kubernetes/render.py')
renderer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(renderer)


class ManifestTest(unittest.TestCase):
    def setUp(self):
        self.config = json.loads((ROOT / 'deploy/kubernetes/config.example.json').read_text())

    def test_single_writer_and_token_boundary(self):
        items = renderer.render(self.config)['items']
        control = next(v for v in items if v['kind'] == 'Deployment' and v['metadata']['name'] == 'mvp1-control')
        self.assertEqual(control['spec']['replicas'], 1)
        self.assertEqual(control['spec']['strategy']['type'], 'Recreate')
        pod = control['spec']['template']['spec']
        self.assertFalse(pod['automountServiceAccountToken'])
        containers = {v['name']: v for v in pod['containers']}
        self.assertNotIn('api', [v['name'] for v in containers['gateway']['volumeMounts']])
        self.assertIn('api', [v['name'] for v in containers['runner']['volumeMounts']])
        self.assertNotIn('hostPath', json.dumps(items))
        self.assertNotIn('docker.sock', json.dumps(items))

    def test_turn_is_separate_and_peers_restricted(self):
        items = renderer.render(self.config)['items']
        turn = next(v for v in items if v['kind'] == 'Deployment' and v['metadata']['name'] == 'mvp1-turn')
        self.assertEqual(turn['metadata']['namespace'], self.config['namespace'] + '-turn')
        pod = turn['spec']['template']['spec']
        self.assertTrue(pod['hostNetwork'])
        self.assertFalse(pod['automountServiceAccountToken'])
        command = pod['containers'][0]['args'][0]
        self.assertIn('--denied-peer-ip=0.0.0.0-255.255.255.255', command)
        self.assertIn('--allowed-peer-ip=10.244.0.0-10.244.255.255', command)
        policy = next(v for v in items if v['kind'] == 'NetworkPolicy')
        self.assertEqual(set(policy['spec']['policyTypes']), {'Ingress', 'Egress'})

    def test_worker_network_cannot_be_application_allowlist(self):
        self.config['application_cidrs'] = ['0.0.0.0/0']
        with self.assertRaises(ValueError):
            renderer.render(self.config)


if __name__ == '__main__':
    unittest.main()
