"""Run under Linux/WSL. All commands use disposable paths and fake containers."""
import json
import hashlib
import os
import pty
from pathlib import Path
import subprocess
import tempfile
import unittest


MOCK_DOCKER = r'''#!/usr/bin/env python3
import json, os, sys, subprocess
from pathlib import Path
a = sys.argv[1:]
root = Path(os.environ['TEST_ROOT'])
scenario = os.environ['SCENARIO']
with (root / 'calls').open('a') as f:
    f.write(json.dumps(a) + '\n')
statefile = root / 'state'
s = json.loads(statefile.read_text())
commit = 'b' * 40
digest = 'ghcr.io/luckkai1989/new-api@sha256:' + 'c' * 64
key = 'fixture-monitor-key-not-a-secret-123456'
def save(): statefile.write_text(json.dumps(s))
if a[0] == 'inspect':
    if '--format' not in a:
        running_key = '' if scenario == 'key_not_applied' else key
        if scenario == 'key_changed': running_key = 'different-fixture-key-at-least-32-bytes'
        print(json.dumps([{'Config': {'Env': ['UPSTREAM_MONITOR_ENCRYPTION_KEY=' + running_key]}}])); sys.exit(0)
    fmt = a[-1]
    if 'project.config_files' in fmt:
        files = str(root / 'deploy/docker-compose.yml')
        if scenario in ('custom_running', 'registry_custom', 'override_invalid_existing'): files += ',' + str(root / 'deploy/docker-compose.custom.yml')
        if scenario in ('registry_running', 'registry_custom', 'unmanaged_image_override', 'override_invalid_existing'): files += ',' + str(root / 'deploy/docker-compose.image.yml')
        print(files)
    elif 'project' in fmt: print('newapi')
    elif '.State.Running' in fmt: print('true' if s['running'] or a[1] != 'newapi' else 'false')
    elif '.State.Health' in fmt: print('healthy')
    elif '.Config.Image' in fmt:
        ref = 'rayapi/new-api:source-old' if s['image'] == 'custom-old' else 'calciumion/new-api:latest'
        if scenario in ('registry_running', 'registry_custom', 'unmanaged_image_override', 'override_invalid_existing'): ref = digest[:-64] + 'd' * 64
        if (root / 'app-recreated').exists() and (root / 'deploy/docker-compose.image.yml').exists(): ref = digest
        print(ref)
    elif '.Image' in fmt: print(s['image'])
elif a[:2] == ['image', 'inspect']:
    image_id = 'old' if scenario in ('same', 'key_not_applied') else 'new'
    if '--format' in a:
        if scenario == 'digest_mismatch' and '@sha256:' in a[2]: image_id = 'other'
        print('1024' if a[-1] == '{{.Size}}' else image_id)
    else:
        labels = {'org.opencontainers.image.revision': commit,
                  'org.opencontainers.image.source': 'https://github.com/luckkai1989/new-api'}
        if scenario == 'wrong_revision': labels['org.opencontainers.image.revision'] = 'a' * 40
        if scenario == 'wrong_repository': labels['org.opencontainers.image.source'] = 'https://github.com/other/new-api'
        digests = [] if scenario == 'missing_digest' else [digest]
        if scenario == 'foreign_digest': digests = [digest.replace('luckkai1989', 'other')]
        if scenario == 'ambiguous_digest': digests += [digest[:-64] + 'd' * 64]
        print(json.dumps([{'Id': image_id, 'Os': 'linux', 'Architecture': 'arm64' if scenario == 'wrong_platform' else 'amd64',
                           'Config': {'Labels': labels}, 'RepoDigests': digests}]))
elif a[0] == 'info':
    print('linux/aarch64' if scenario == 'wrong_server_platform' else 'linux/x86_64')
elif a[0] == 'pull':
    if not (root / 'git-pulled').exists(): sys.exit(8)
    if scenario == 'pull_fail': sys.exit(3)
    (root / 'image-pulled').touch()
elif a[:2] == ['image', 'prune']:
    if scenario == 'image_prune_fail': sys.exit(8)
elif a[0] in ('build', 'builder'):
    raise SystemExit('Local builds/cache pruning are forbidden by the test')
elif a[:2] == ['image', 'save']:
    if scenario == 'image_fail': sys.exit(5)
    print('mock image archive')
elif a[0] == 'exec':
    if a[1] == 'newapi':
        if '--version' in a:
            if scenario != 'blank_version': print('rc.old' if s['image'] == 'old' else 'rc.new')
        elif scenario == 'key_missing_after': sys.exit(7)
    elif a[1] == 'newapi-redis':
        if scenario == 'redis_fail' and 'PING' not in a[-1]: sys.exit(6)
        # Execute the actual container shell body, including mktemp/trap/cat.
        script = a[-1].replace('/tmp/rayapi-backup-', str(root / 'rayapi-backup-'))
        env = dict(os.environ, REDISCLI_AUTH='fixture-password')
        sys.exit(subprocess.run(['sh', '-eu', '-c', script], env=env).returncode)
    elif 'mysqldump' in a[-1]:
        if scenario == 'dump_fail': sys.exit(2)
        print('CREATE DATABASE newapi;\n-- Dump completed on test')
    elif 'COUNT(*)' in a[-1]: print('1' if scenario == 'non_innodb' else '0')
    else: print('1024' if 'information_schema' in a[-1] else '1')
elif a[0] == 'compose':
    if 'config' in a:
        if '--quiet' in a:
            if scenario in ('override_invalid', 'override_invalid_existing'): sys.exit(9)
        else: print((root / 'config.json').read_text())
    elif 'pull' in a:
        if scenario == 'pull_fail': sys.exit(3)
    elif 'stop' in a:
        s['running'] = False; save()
    elif 'up' in a:
        (root / 'app-recreated').touch()
        s['image'] = 'old' if scenario == 'key_not_applied' else 'new'; s['running'] = True; save()
        if scenario == 'up_fail': sys.exit(4)
elif a[0] == 'start':
    s['running'] = True; save()
elif a[0] == 'logs': print('mock startup log')
'''

MOCK_GIT = r'''#!/bin/sh
case "$*" in
  *"remote get-url origin"*)
    if [ "$SCENARIO" = wrong_origin ]; then echo https://github.com/other/new-api.git; else echo https://github.com/luckkai1989/new-api.git; fi ;;
  *"branch --show-current"*)
    if [ "$SCENARIO" = wrong_branch ]; then echo feature; else echo main; fi ;;
  *"pull --ff-only origin main"*)
    [ "$SCENARIO" != git_pull_fail ] || exit 9
    touch "$TEST_ROOT/git-pulled" ;;
  *"rev-parse --verify refs/remotes/origin/main"*)
    if [ "$SCENARIO" = unpublished_commit ]; then printf 'a%.0s' $(seq 1 40); else printf 'b%.0s' $(seq 1 40); fi
    echo ;;
  *"rev-parse --verify HEAD"*)
    if [ -f "$TEST_ROOT/git-pulled" ]; then printf 'b%.0s' $(seq 1 40); else printf 'a%.0s' $(seq 1 40); fi
    echo ;;
  *"diff --cached --quiet"*|*"diff --quiet"*) [ "$SCENARIO" != dirty_source ] ;;
  *"status --porcelain --untracked-files=normal"*) [ "$SCENARIO" != untracked_source ] || echo '?? extra.go' ;;
  *) exit 1 ;;
esac
'''


class UpgradeTests(unittest.TestCase):
    def run_case(self, scenario, check=False, mode='--upgrade', menu=None, existing_backups=()):
        with tempfile.TemporaryDirectory(prefix='rayapi-upgrade-test-') as tmp:
            root = Path(tmp)
            deploy = root / 'deploy'
            deploy.mkdir()
            source_dir = root / 'source'
            (source_dir / 'service').mkdir(parents=True)
            (source_dir / 'Dockerfile').write_text('FROM scratch\n')
            (source_dir / 'service/upstream_monitor.go').write_text('package service\n')
            (deploy / 'docker-compose.yml').write_text('test fixture')
            (deploy / '.env').write_text('test fixture')
            (root / 'nginx').mkdir()
            backups = root / 'backups'
            backups.mkdir()
            for name, kind in existing_backups:
                old = backups / name
                old.mkdir()
                if kind == 'complete':
                    operation = old / 'operation.txt'
                    operation.write_text('backup\n')
                    (old / 'SHA256SUMS').write_text(
                        f'{hashlib.sha256(operation.read_bytes()).hexdigest()}  operation.txt\n')
                    (old / 'BACKUP_COMPLETE').touch()
                    (old / 'SERVICE_RESUMED').touch()
                elif kind == 'failed':
                    (old / 'BACKUP_COMPLETE').touch()
                elif kind != 'manual':
                    raise ValueError(kind)
            cfg = {'services': {
                'newapi': {
                    'image': 'calciumion/new-api:latest', 'container_name': 'newapi',
                    'environment': {'SQL_DSN': 'test:test@tcp(mysql:3306)/newapi?charset=utf8mb4',
                                    'UPSTREAM_MONITOR_ENCRYPTION_KEY': 'fixture-monitor-key-not-a-secret-123456'},
                    'healthcheck': {'test': ['CMD', 'true']},
                    'ports': [{'published': '19733', 'target': 3000, 'host_ip': '127.0.0.1'}],
                },
                'mysql': {'container_name': 'newapi-mysql', 'environment': {'MYSQL_DATABASE': 'newapi'}},
                'redis': {'container_name': 'newapi-redis'},
            }}
            if scenario == 'bad_port':
                cfg['services']['newapi']['ports'][0]['host_ip'] = '0.0.0.0'
            if scenario == 'missing_key':
                cfg['services']['newapi']['environment'].pop('UPSTREAM_MONITOR_ENCRYPTION_KEY')
            custom_content = 'services:\n  newapi:\n    image: rayapi/new-api:source-old\n    environment:\n      UPSTREAM_MONITOR_ENCRYPTION_KEY: ${UPSTREAM_MONITOR_ENCRYPTION_KEY}\n      KEEP_EXISTING_SETTING: yes\n'
            if scenario in ('custom_running', 'registry_custom', 'override_invalid_existing'):
                cfg['services']['newapi']['image'] = 'rayapi/new-api:source-old'
                (deploy / 'docker-compose.custom.yml').write_text(custom_content)
            digest = 'ghcr.io/luckkai1989/new-api@sha256:' + 'c' * 64
            image_content = 'services:\n  newapi:\n    image: ' + digest + '\n'
            if scenario in ('registry_running', 'registry_custom', 'unmanaged_image_override', 'override_invalid_existing'):
                digest = digest[:-64] + 'd' * 64
                image_content = 'services:\n  newapi:\n    image: ' + digest + '\n'
                cfg['services']['newapi']['image'] = digest
                if scenario == 'unmanaged_image_override': image_content += '    environment:\n      DO_NOT_DROP: yes\n'
                (deploy / 'docker-compose.image.yml').write_text(image_content)
            (root / 'config.json').write_text(json.dumps(cfg))
            (root / 'state').write_text(json.dumps({'running': True, 'image': 'custom-old' if scenario == 'custom_running' else 'old'}))
            bindir = root / 'bin'
            bindir.mkdir()
            for name, content in {
                'docker': MOCK_DOCKER,
                'git': MOCK_GIT,
                # Enforce the BusyBox filename constraint instead of allowing GNU suffixes.
                'mktemp': '#!/bin/sh\nfor arg do template="$arg"; done\ncase "$template" in *XXXXXX) exec /usr/bin/mktemp "$@" ;; *) echo "mktemp: Invalid argument" >&2; exit 1 ;; esac\n',
                'redis-cli': '#!/bin/sh\nfor arg do last="$arg"; done\nif [ "$last" = PING ]; then echo PONG; else printf "REDIS0012 mock snapshot\\n" > "$last"; fi\n',
                'redis-check-rdb': '#!/bin/sh\nif [ "$SCENARIO" = rdb_invalid ]; then exit 1; fi\ngrep -q "^REDIS" "$1"\n',
                'curl': '#!/bin/sh\nprintf \'{"success":true}\'\n',
                'df': '#!/bin/sh\nfree=999999999\ncase "$SCENARIO" in low_space) free=1048576 ;; mid_space) free=4194304 ;; esac\n[ "$SCENARIO" != low_space_after_pull ] || [ ! -f "$TEST_ROOT/image-pulled" ] || free=1048576\nprintf "Filesystem 1024-blocks Used Available Capacity Mounted\\nmock 999999999 0 %s 0 /\\n" "$free"\n',
            }.items():
                p = bindir / name
                p.write_text(content)
                p.chmod(0o700)
            source = Path(__file__).with_name('update-newapi.sh').read_text()
            source = source.replace('[[ $EUID -eq 0 ]]', '[[ 1 -eq 1 ]]')
            source = source.replace('DEPLOY=/opt/newapi', f'DEPLOY={deploy}')
            source = source.replace('BACKUP_ROOT=/opt/newapi-backups', f'BACKUP_ROOT={root}/backups')
            source = source.replace('SOURCE=/root/newapi-update/new-api', f'SOURCE={source_dir}')
            source = source.replace('/run/lock/rayapi-newapi-upgrade.lock', str(root / 'lock'))
            source = source.replace('cp -a /etc/nginx', f'cp -a {root}/nginx')
            source = source.replace('-C /opt newapi', f'-C {root} deploy')
            if scenario == 'legacy_template':
                source = source.replace('mktemp /tmp/rayapi-backup-XXXXXX)',
                                        'mktemp /tmp/rayapi-backup-XXXXXX.rdb)')
            script = root / 'script.sh'
            script.write_text(source)
            env = dict(os.environ, PATH=f'{bindir}:{os.environ["PATH"]}', TEST_ROOT=tmp, SCENARIO=scenario)
            argv = ['bash', str(script)] + (['--check'] if check else ([mode] if mode else []))
            if menu is None:
                result = subprocess.run(argv, env=env, capture_output=True, text=True, timeout=20)
            else:
                master, slave = pty.openpty()
                try:
                    os.write(master, (menu + '\n').encode())
                    result = subprocess.run(argv, env=env, stdin=slave, capture_output=True,
                                            text=True, timeout=20)
                finally:
                    os.close(master)
                    os.close(slave)
            calls = [json.loads(line) for line in (root / 'calls').read_text().splitlines()] if (root / 'calls').exists() else []
            state = json.loads((root / 'state').read_text())
            markers = {p.name for p in root.glob('backups/*/*')}
            self.last_backup_names = {p.name for p in backups.iterdir() if p.is_dir()}
            self.last_image_override = (deploy / 'docker-compose.image.yml').read_text() if (deploy / 'docker-compose.image.yml').exists() else None
            if scenario in ('custom_running', 'registry_custom', 'override_invalid_existing'):
                self.assertEqual((deploy / 'docker-compose.custom.yml').read_text(), custom_content)
            self.assertNotIn('fixture-monitor-key-not-a-secret-123456', result.stdout + result.stderr)
            self.assertFalse(any(c[0] in ('build', 'builder') for c in calls))
            self.assertFalse(list(root.glob('rayapi-backup-*')), 'Redis temporary snapshot leaked')
            for sums in root.glob('backups/*/SHA256SUMS'):
                verified = subprocess.run(['sha256sum', '-c', str(sums)], cwd=sums.parent,
                                          capture_output=True, text=True)
                self.assertEqual(verified.returncode, 0, verified.stdout + verified.stderr)
            return result, calls, state, markers

    def test_preflight_does_not_mutate(self):
        r, calls, _, markers = self.run_case('success', check=True)
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertFalse(any('pull' in c or 'stop' in c or 'up' in c for c in calls))
        self.assertFalse(any(c[:2] == ['builder', 'prune'] for c in calls))
        self.assertFalse(markers)

    def test_reject_public_binding(self):
        r, calls, _, _ = self.run_case('bad_port')
        self.assertNotEqual(r.returncode, 0)
        self.assertFalse(any('pull' in c or 'stop' in c for c in calls))

    def test_same_image_skips_restart(self):
        r, calls, s, _ = self.run_case('same')
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertTrue(s['running'])
        self.assertFalse(any('stop' in c or 'up' in c for c in calls))

    def test_pull_failure_keeps_original_online(self):
        r, calls, s, _ = self.run_case('pull_fail')
        self.assertNotEqual(r.returncode, 0)
        self.assertEqual(s, {'running': True, 'image': 'old'})
        self.assertFalse(any('stop' in c for c in calls))

    def test_dump_failure_restarts_original_without_upgrading(self):
        r, calls, s, m = self.run_case('dump_fail')
        self.assertNotEqual(r.returncode, 0)
        self.assertEqual(s, {'running': True, 'image': 'old'})
        self.assertIn(['start', 'newapi'], calls)
        self.assertFalse(any('up' in c for c in calls))
        self.assertNotIn('BACKUP_COMPLETE', m)

    def test_failed_new_version_never_downgrades(self):
        r, calls, s, m = self.run_case('up_fail')
        self.assertNotEqual(r.returncode, 0)
        self.assertEqual(s['image'], 'new')
        self.assertNotIn(['start', 'newapi'], calls)
        self.assertIn('BACKUP_COMPLETE', m)
        self.assertNotIn('UPGRADE_COMPLETE', m)
        self.assertFalse(any(c[:2] == ['image', 'prune'] for c in calls))

    def test_success_backs_up_and_only_recreates_app(self):
        r, calls, s, m = self.run_case('success')
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(s, {'running': True, 'image': 'new'})
        self.assertIn('UPGRADE_COMPLETE', m)
        self.assertIn('BACKUP_COMPLETE', m)
        up = next(c for c in calls if 'up' in c)
        self.assertIn('--no-deps', up)
        self.assertEqual(up[-1], 'newapi')
        prune = next(i for i, c in enumerate(calls) if c[:2] == ['image', 'prune'])
        tag = next(i for i, c in enumerate(calls) if c[:2] == ['image', 'tag'])
        self.assertEqual(calls[prune], ['image', 'prune', '-f'])
        self.assertLess(tag, prune)
        self.assertLess(calls.index(up), prune)

    def test_dangling_image_cleanup_failure_keeps_upgrade_success(self):
        r, calls, s, m = self.run_case('image_prune_fail')
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn('Dangling image cleanup failed', r.stdout)
        self.assertEqual(s['image'], 'new')
        self.assertIn('UPGRADE_COMPLETE', m)
        self.assertEqual(sum(c[:2] == ['image', 'prune'] for c in calls), 1)

    def test_custom_upgrade_pulls_verified_commit_before_outage(self):
        r, calls, s, m = self.run_case('success', mode='--upgrade-custom')
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(s['image'], 'new')
        self.assertIn('UPGRADE_COMPLETE', m)
        self.assertIn('source-commit.txt', m)
        self.assertIn('source-image-tag.txt', m)
        self.assertIn('source-image-digest.txt', m)
        self.assertIn(['pull', '--platform', 'linux/amd64', 'ghcr.io/luckkai1989/new-api:sha-' + 'b' * 40], calls)
        self.assertLess(next(i for i, c in enumerate(calls) if c[:1] == ['pull']),
                        next(i for i, c in enumerate(calls) if 'stop' in c))
        self.assertFalse(any('pull' in c for c in calls if c[:1] == ['compose']))
        self.assertEqual(sum(c[:2] == ['image', 'prune'] for c in calls), 1)
        self.assertEqual(self.last_image_override, 'services:\n  newapi:\n    image: ghcr.io/luckkai1989/new-api@sha256:' + 'c' * 64 + '\n')
        up = next(c for c in calls if 'up' in c)
        self.assertIn('--force-recreate', up)
        self.assertIn('--no-deps', up)
        self.assertIn('--pull', up)
        self.assertEqual(up[up.index('--pull') + 1], 'never')

    def test_custom_upgrade_needs_no_build_space_reserve(self):
        r, calls, s, _ = self.run_case('mid_space', mode='--upgrade-source')
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn('Disk preflight:', r.stdout)
        self.assertTrue(any(c[:1] == ['pull'] for c in calls))
        self.assertEqual(s['image'], 'new')

    def test_low_space_blocks_before_pull_or_outage(self):
        r, calls, s, m = self.run_case('low_space', mode='--upgrade-source')
        self.assertNotEqual(r.returncode, 0)
        self.assertFalse(m)
        self.assertEqual(s['image'], 'old')
        self.assertFalse(any(c[:1] == ['pull'] or 'stop' in c for c in calls))

    def test_space_consumed_by_pull_blocks_before_stop(self):
        r, calls, s, m = self.run_case('low_space_after_pull', mode='--upgrade-custom')
        self.assertNotEqual(r.returncode, 0)
        self.assertIn('Insufficient backup space after image pull', r.stdout)
        self.assertNotIn('BACKUP_COMPLETE', m)
        self.assertEqual(s['image'], 'old')
        self.assertFalse(any('stop' in c for c in calls))

    def test_untrusted_or_missing_image_never_stops_service(self):
        for scenario in ('pull_fail', 'wrong_revision', 'wrong_repository', 'missing_digest',
                         'foreign_digest', 'ambiguous_digest', 'digest_mismatch',
                         'wrong_platform', 'wrong_server_platform'):
            with self.subTest(scenario=scenario):
                r, calls, s, m = self.run_case(scenario, mode='--upgrade-custom')
                self.assertNotEqual(r.returncode, 0)
                self.assertEqual(s, {'running': True, 'image': 'old'})
                self.assertFalse(any('stop' in c or 'up' in c for c in calls))
                self.assertNotIn('BACKUP_COMPLETE', m)
                self.assertIsNone(self.last_image_override)

    def test_completed_backups_keep_only_latest_three(self):
        old = [(f'2025010{i}-000000-aaaaaa', 'complete') for i in range(1, 5)]
        old += [('20250105-000000-aaaaaa', 'failed'), ('manual-backup', 'manual')]
        r, _, _, _ = self.run_case('success', existing_backups=old)
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertNotIn(old[0][0], self.last_backup_names)
        self.assertNotIn(old[1][0], self.last_backup_names)
        self.assertIn(old[2][0], self.last_backup_names)
        self.assertIn(old[3][0], self.last_backup_names)
        self.assertIn(old[4][0], self.last_backup_names)
        self.assertIn('manual-backup', self.last_backup_names)
        self.assertEqual(len(self.last_backup_names), 5)

    def test_failed_upgrade_does_not_prune_completed_backups(self):
        old = [(f'2025010{i}-000000-aaaaaa', 'complete') for i in range(1, 5)]
        r, _, _, _ = self.run_case('up_fail', existing_backups=old)
        self.assertNotEqual(r.returncode, 0)
        self.assertTrue({name for name, _ in old} <= self.last_backup_names)

    def test_database_only_backup_also_keeps_three_completed_backups(self):
        old = [(f'2025010{i}-000000-aaaaaa', 'complete') for i in range(1, 4)]
        r, _, _, _ = self.run_case('success', mode='--backup-db', existing_backups=old)
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertNotIn(old[0][0], self.last_backup_names)
        self.assertIn(old[1][0], self.last_backup_names)
        self.assertIn(old[2][0], self.last_backup_names)

    def test_unchanged_custom_image_skips_restart(self):
        r, calls, s, m = self.run_case('same', mode='--upgrade-source')
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(s, {'running': True, 'image': 'old'})
        self.assertFalse(any('stop' in c or 'up' in c for c in calls))
        self.assertNotIn('BACKUP_COMPLETE', m)

    def test_source_git_pull_failure_leaves_original_running(self):
        r, calls, s, m = self.run_case('git_pull_fail', mode='--upgrade-source')
        self.assertNotEqual(r.returncode, 0)
        self.assertEqual(s, {'running': True, 'image': 'old'})
        self.assertFalse(m)
        self.assertFalse(any('build' in c or 'stop' in c for c in calls))

    def test_source_wrong_branch_is_rejected_before_pull(self):
        r, calls, s, m = self.run_case('wrong_branch', mode='--upgrade-source')
        self.assertNotEqual(r.returncode, 0)
        self.assertEqual(s, {'running': True, 'image': 'old'})
        self.assertFalse(m)
        self.assertFalse(any('build' in c or 'stop' in c for c in calls))

    def test_wrong_fork_or_unpublished_main_cannot_be_deployed(self):
        for scenario in ('wrong_origin', 'unpublished_commit'):
            with self.subTest(scenario=scenario):
                r, calls, s, m = self.run_case(scenario, mode='--upgrade-custom')
                self.assertNotEqual(r.returncode, 0)
                self.assertFalse(m)
                self.assertEqual(s, {'running': True, 'image': 'old'})
                self.assertFalse(any('pull' in c or 'stop' in c for c in calls))

    def test_dirty_source_is_rejected_before_backup(self):
        r, calls, s, m = self.run_case('dirty_source', mode='--upgrade-source')
        self.assertNotEqual(r.returncode, 0)
        self.assertEqual(s, {'running': True, 'image': 'old'})
        self.assertFalse(m)
        self.assertFalse(any('build' in c or 'stop' in c for c in calls))

    def test_untracked_source_is_rejected_before_backup(self):
        r, calls, s, m = self.run_case('untracked_source', mode='--upgrade-source')
        self.assertNotEqual(r.returncode, 0)
        self.assertEqual(s, {'running': True, 'image': 'old'})
        self.assertFalse(m)
        self.assertFalse(any('build' in c or 'stop' in c for c in calls))

    def test_existing_custom_deployment_can_upgrade_again(self):
        r, calls, s, m = self.run_case('custom_running', mode='--upgrade-source')
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(s['image'], 'new')
        self.assertIn('docker-compose.custom.yml', m)
        up = next(c for c in calls if 'up' in c)
        self.assertEqual(up.count('-f'), 3)

    def test_registry_deployment_can_upgrade_again(self):
        for scenario in ('registry_running', 'registry_custom'):
            with self.subTest(scenario=scenario):
                r, calls, s, m = self.run_case(scenario, mode='--upgrade-custom')
                self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
                self.assertEqual(s['image'], 'new')
                self.assertIn('docker-compose.image.yml', m)
                up = next(c for c in calls if 'up' in c)
                self.assertEqual(up.count('-f'), 3 if scenario == 'registry_custom' else 2)

    def test_registry_deployment_rejects_official_upgrade(self):
        r, calls, _, _ = self.run_case('registry_running')
        self.assertNotEqual(r.returncode, 0)
        self.assertFalse(any('pull' in c or 'stop' in c for c in calls))

    def test_unmanaged_image_override_is_not_overwritten(self):
        r, calls, _, _ = self.run_case('unmanaged_image_override', mode='--upgrade-custom')
        self.assertNotEqual(r.returncode, 0)
        self.assertFalse(any('pull' in c or 'stop' in c for c in calls))
        self.assertIn('DO_NOT_DROP', self.last_image_override)

    def test_monitor_key_missing_or_rotated_blocks_upgrade(self):
        for scenario in ('missing_key', 'key_changed'):
            with self.subTest(scenario=scenario):
                r, calls, _, m = self.run_case(scenario, mode='--upgrade-custom')
                self.assertNotEqual(r.returncode, 0)
                self.assertFalse(m)
                self.assertFalse(any('pull' in c or 'stop' in c for c in calls))

    def test_new_key_injection_recreates_even_if_image_unchanged(self):
        r, calls, _, m = self.run_case('key_not_applied', mode='--upgrade-custom')
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertTrue(any('up' in c for c in calls))
        self.assertIn('UPGRADE_COMPLETE', m)

    def test_invalid_override_before_migration_restores_original(self):
        for scenario in ('override_invalid', 'override_invalid_existing'):
            with self.subTest(scenario=scenario):
                r, calls, s, m = self.run_case(scenario, mode='--upgrade-custom')
                self.assertNotEqual(r.returncode, 0)
                self.assertEqual(s, {'running': True, 'image': 'old'})
                if scenario == 'override_invalid':
                    self.assertIsNone(self.last_image_override)
                else:
                    self.assertEqual(self.last_image_override, 'services:\n  newapi:\n    image: ghcr.io/luckkai1989/new-api@sha256:' + 'd' * 64 + '\n')
                self.assertIn('BACKUP_COMPLETE', m)
                self.assertIn(['start', 'newapi'], calls)
                self.assertFalse(any('up' in c for c in calls))

    def test_key_validation_failure_after_start_does_not_downgrade(self):
        r, calls, _, m = self.run_case('key_missing_after', mode='--upgrade-custom')
        self.assertNotEqual(r.returncode, 0)
        self.assertNotIn('UPGRADE_COMPLETE', m)
        self.assertNotIn(['start', 'newapi'], calls)

    def test_blank_application_version_reports_commit_and_digest(self):
        r, _, _, _ = self.run_case('blank_version', mode='--upgrade-custom')
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn('UPGRADE SUCCESS: unknown -> unknown', r.stdout)
        self.assertIn('Deployed GitHub source commit: ' + 'b' * 40, r.stdout)

    def test_official_upgrade_rejects_active_custom_override(self):
        r, calls, s, _ = self.run_case('custom_running')
        self.assertNotEqual(r.returncode, 0)
        self.assertEqual(s['image'], 'custom-old')
        self.assertFalse(any('pull' in c or 'stop' in c for c in calls))

    def test_online_database_backup_never_stops_or_pulls(self):
        r, calls, s, m = self.run_case('success', mode='--backup-db')
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(s, {'running': True, 'image': 'old'})
        self.assertIn('newapi.sql', m)
        self.assertIn('BACKUP_COMPLETE', m)
        self.assertNotIn('redis.rdb', m)
        self.assertFalse(any('stop' in c or 'up' in c or 'pull' in c for c in calls))

    def test_full_backup_resumes_original_and_contains_redis(self):
        r, calls, s, m = self.run_case('success', mode='--backup')
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(s, {'running': True, 'image': 'old'})
        self.assertTrue({'newapi.sql', 'redis.rdb', 'newapi-files.tar.gz',
                         'containers.inspect.json', 'compose.resolved.json',
                         'BACKUP_COMPLETE', 'SERVICE_RESUMED'} <= m)
        self.assertFalse(any('pull' in c or 'up' in c for c in calls))
        self.assertFalse(any(c[:2] == ['image', 'prune'] for c in calls))

    def test_image_archive_is_optional_and_created_before_stop(self):
        r, calls, _, m = self.run_case('success', mode='--backup-images')
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn('container-images.tar.gz', m)
        save = next(i for i, c in enumerate(calls) if c[:2] == ['image', 'save'])
        stop = next(i for i, c in enumerate(calls) if 'stop' in c)
        self.assertLess(save, stop)

    def test_image_export_failure_never_stops_service(self):
        r, calls, s, m = self.run_case('image_fail', mode='--backup-images')
        self.assertNotEqual(r.returncode, 0)
        self.assertTrue(s['running'])
        self.assertNotIn('BACKUP_COMPLETE', m)
        self.assertFalse(any('stop' in c for c in calls))

    def test_redis_failure_resumes_original(self):
        r, calls, s, m = self.run_case('redis_fail', mode='--backup')
        self.assertNotEqual(r.returncode, 0)
        self.assertEqual(s, {'running': True, 'image': 'old'})
        self.assertNotIn('BACKUP_COMPLETE', m)
        self.assertIn(['start', 'newapi'], calls)

    def test_online_backup_rejects_nontransactional_tables(self):
        r, calls, _, m = self.run_case('non_innodb', mode='--backup-db')
        self.assertNotEqual(r.returncode, 0)
        self.assertNotIn('BACKUP_COMPLETE', m)
        self.assertFalse(any('mysqldump' in c[-1] for c in calls))

    def test_no_argument_in_automation_does_not_upgrade(self):
        r, calls, _, _ = self.run_case('success', mode=None)
        self.assertEqual(r.returncode, 2)
        self.assertFalse(calls)

    def test_menu_exit_does_not_touch_containers(self):
        r, calls, _, _ = self.run_case('success', mode=None, menu='0')
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn('仅在线备份', r.stdout)
        self.assertFalse(calls)

    def test_menu_database_selection_does_not_upgrade(self):
        r, calls, _, m = self.run_case('success', mode=None, menu='3')
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn('newapi.sql', m)
        self.assertFalse(any('stop' in c or 'up' in c or 'pull' in c for c in calls))

    def test_menu_invalid_and_empty_input_can_retry(self):
        r, calls, _, _ = self.run_case('success', mode=None, menu='9\n\n0')
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(r.stdout.count('请输入 0-6'), 2)
        self.assertFalse(calls)

    def test_menu_check_selection_is_read_only(self):
        r, calls, _, m = self.run_case('success', mode=None, menu='5')
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertFalse(m)
        self.assertFalse(any('stop' in c or 'up' in c or 'pull' in c for c in calls))

    def test_menu_source_selection(self):
        r, calls, s, m = self.run_case('success', mode=None, menu='6')
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(s['image'], 'new')
        self.assertIn('source-commit.txt', m)
        self.assertTrue(any(c[:1] == ['pull'] for c in calls))

    def test_menu_q_exits(self):
        r, calls, _, _ = self.run_case('success', mode=None, menu='q')
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertFalse(calls)

    def test_old_mktemp_suffix_reproduces_failure_and_restarts_original(self):
        r, calls, s, m = self.run_case('legacy_template', mode='--backup')
        self.assertNotEqual(r.returncode, 0)
        self.assertIn('mktemp: Invalid argument', r.stdout + r.stderr)
        self.assertEqual(s, {'running': True, 'image': 'old'})
        self.assertNotIn('BACKUP_COMPLETE', m)
        self.assertIn(['start', 'newapi'], calls)

    def test_invalid_rdb_blocks_upgrade_and_cleans_temporary_file(self):
        r, calls, s, m = self.run_case('rdb_invalid')
        self.assertNotEqual(r.returncode, 0)
        self.assertEqual(s, {'running': True, 'image': 'old'})
        self.assertFalse(any('up' in c for c in calls))
        self.assertNotIn('BACKUP_COMPLETE', m)


if __name__ == '__main__':
    unittest.main(verbosity=2)
