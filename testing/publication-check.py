#!/usr/bin/env python3
"""Check publishable Git files without displaying secret values.

Default: working tree tracked/non-ignored files. --history: every reachable Git
blob. This is a guardrail, not proof that arbitrary credentials cannot be present.
Synthetic addresses in test fixtures and *.example.* remain permitted.
"""
import argparse
import ipaddress
import pathlib
import re
import subprocess

ROOT = pathlib.Path(__file__).resolve().parents[1]
IPV4 = re.compile(r'(?<![\w.])(?:\d{1,3}\.){3}\d{1,3}(?:/\d+)?(?![\w.])')
SECRETS = {
    'private key': re.compile(r'-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----'),
    'GitHub token': re.compile(r'(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{40,})'),
    'AWS access key': re.compile(r'AKIA[0-9A-Z]{16}'),
    'stored password hash': re.compile(r'\$argon2id\$v=19\$m=\d+,t=\d+,p=\d+\$[A-Za-z0-9+/]{20,}\$[A-Za-z0-9+/]{30,}'),
}
DOCUMENTATION_NETS = tuple(ipaddress.ip_network(n) for n in ('192.0.2.0/24', '198.51.100.0/24', '203.0.113.0/24'))


def git(*args):
    return subprocess.check_output(['git', *args], cwd=ROOT)


def inspect(name, data):
    findings = []
    text = data.decode('utf-8', errors='replace')
    for kind, pattern in SECRETS.items():
        if pattern.search(text):
            findings.append(kind)
    p = pathlib.PurePosixPath(name)
    if (name.startswith(('testing/data/', 'deploy/data/', '.tools/'))
            or p.suffix in ('.db', '.sqlite', '.sqlite3', '.pem', '.key', '.p12', '.pfx')
            or p.name == '.env' or (p.name.startswith('.env.') and p.name != '.env.example')):
        findings.append('local data/credential file')
    fixture = (p.name.endswith(('_test.go', '_test.py')) or '.example.' in p.name
               or p.name.endswith('.example'))
    if not fixture and name != 'go.sum':
        for number, line in enumerate(text.splitlines(), 1):
            for literal in IPV4.findall(line):
                try:
                    address = ipaddress.ip_address(literal.split('/')[0])
                except ValueError:
                    continue
                if (str(address) in ('127.0.0.1', '0.0.0.0', '255.255.255.255')
                        or any(address in net for net in DOCUMENTATION_NETS)):
                    continue
                findings.append(f'non-placeholder IPv4 at line {number}')
    return findings


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--history', action='store_true')
    args = parser.parse_args()
    records = []
    if args.history:
        seen = set()
        for commit in git('rev-list', '--all').decode().split():
            for entry in git('ls-tree', '-rz', commit).split(b'\0'):
                if not entry:
                    continue
                meta, raw_name = entry.split(b'\t', 1)
                _, kind, oid = meta.decode().split()
                name = raw_name.decode()
                if kind != 'blob' or (name, oid) in seen:
                    continue
                seen.add((name, oid))
                records.append((commit[:8] + ':' + name, name, git('cat-file', 'blob', oid)))
    else:
        for raw in git('ls-files', '--cached', '--others', '--exclude-standard', '-z').split(b'\0'):
            if raw:
                name = raw.decode()
                if (ROOT / name).is_file():
                    records.append((name, name, (ROOT / name).read_bytes()))
    failed = False
    for label, name, data in records:
        for problem in inspect(name, data):
            print(f'{label}: {problem}')
            failed = True
    print(f'Checked {len(records)} file versions; ' + ('findings require review' if failed else 'no findings'))
    return int(failed)


if __name__ == '__main__':
    raise SystemExit(main())
