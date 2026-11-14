#!/usr/bin/env python3
"""Offline release-gate and artifact-checker tests; no Go or downloads needed."""
import hashlib
import importlib.util
import json
import pathlib
import subprocess
import tempfile
import unittest


def load(name):
    spec = importlib.util.spec_from_file_location(name, pathlib.Path(__file__).with_name(name + '.py'))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


builder = load('build-release')
checker = load('check-release')


def git(root, *args):
    return subprocess.run(['git', *args], cwd=root, check=True, capture_output=True)


def repository(root, version='0.2.0'):
    git(root, 'init', '-q')
    git(root, 'config', 'user.email', 'release-tests@example.invalid')
    git(root, 'config', 'user.name', 'Release Tests')
    (root / 'main.go').write_text(f'package main\nconst version = "{version}"\n')
    git(root, 'add', 'main.go')
    git(root, 'commit', '-q', '-m', 'fixture')


class ReleaseGates(unittest.TestCase):
    def test_tag_cleanliness_commit_and_version_bump(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            repository(root)
            with self.assertRaisesRegex(builder.BuildError, 'tagged exactly'):
                builder.provenance(root)
            git(root, 'tag', 'v0.2.0')
            with self.assertRaisesRegex(builder.BuildError, 'workflow tag'):
                builder.provenance(root, required_tag='v0.1.0')
            version, commit = builder.provenance(root, required_tag='v0.2.0')
            self.assertEqual(version, '0.2.0')
            self.assertEqual(commit, git(root, 'rev-parse', 'HEAD').stdout.decode().strip())
            (root / 'untracked').write_text('dirty')
            with self.assertRaisesRegex(builder.BuildError, 'clean Git worktree'):
                builder.provenance(root)
            self.assertEqual(builder.provenance(root, True), (version, commit))
            (root / 'untracked').unlink()
            git(root, 'tag', 'v9.0.0')
            with self.assertRaisesRegex(builder.BuildError, 'newer than all previous'):
                builder.provenance(root)

    def test_release_checker_repeats_tag_gate(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            repository(root)
            with self.assertRaisesRegex(checker.CheckError, 'tagged exactly'):
                checker.expectations(root, False)
            self.assertEqual(checker.expectations(root, True)[0], '0.2.0')


class ArtifactChecker(unittest.TestCase):
    def make_artifacts(self, root):
        version = '0.2.0'
        dist = root / 'dist'
        clients = dist / 'clients'
        manifest = {'version': version,
                    'commit': git(root, 'rev-parse', 'HEAD').stdout.decode().strip(),
                    'development': True, 'files': {}}
        archives = []
        for system, arch in builder.TARGETS:
            label = f'remote-claude-{version}-{system}-{arch}'
            stage = pathlib.Path(tempfile.mkdtemp(dir=dist))
            executable = 'remote-claude.exe' if system == 'windows' else 'remote-claude'
            search = 'remote-claude-rg.exe' if system == 'windows' else 'remote-claude-rg'
            names = checker.COMMON | ({'install.cmd', 'install.ps1'} if system == 'windows'
                                      else {'install.sh'}) | {executable, search}
            for name in names:
                (stage / name).write_bytes(f'{label}/{name}\n'.encode())
                (stage / name).chmod(0o755 if name in {executable, search} else 0o644)
            platform = f'{system}-{arch}'
            served = clients / version / platform
            served.mkdir(parents=True)
            manifest['files'][platform] = {}
            for name in [executable, search]:
                data = (stage / name).read_bytes()
                (served / name).write_bytes(data)
                manifest['files'][platform][name] = hashlib.sha256(data).hexdigest()
            archive = dist / (label + ('.zip' if system == 'windows' else '.tar.gz'))
            builder.write_archive(archive, label, sorted(stage.iterdir(), key=lambda p: p.name))
            archives.append(archive)
            for path in stage.iterdir():
                path.unlink()
            stage.rmdir()
        (clients / 'manifest.json').write_text(json.dumps(manifest))
        (dist / 'SHA256SUMS').write_text(''.join(
            f'{builder.digest(path)}  {path.name}\n' for path in archives))
        return manifest

    def test_complete_fixture_and_tamper_detection(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            repository(root)
            (root / 'dist').mkdir()
            self.make_artifacts(root)
            manifest = checker.check(root, True)
            self.assertTrue(manifest['development'])
            served = root / 'dist/clients/0.2.0/linux-amd64/remote-claude'
            served.write_bytes(b'tampered')
            with self.assertRaisesRegex(checker.CheckError, 'served hash mismatch'):
                checker.check(root, True)

    def test_forbidden_names_and_stale_output(self):
        for name in ['label/private/invite.json', 'label/.git/config', 'label/.idea/workspace.xml',
                     'label/remote-claude-dev.json', 'label/.credentials.json', '../escape']:
            self.assertTrue(checker.forbidden(name), name)
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            repository(root)
            (root / 'dist').mkdir()
            self.make_artifacts(root)
            (root / 'dist/old-stage').mkdir()
            with self.assertRaisesRegex(checker.CheckError, 'stale or forbidden dist output'):
                checker.check(root, True)


if __name__ == '__main__':
    unittest.main()
