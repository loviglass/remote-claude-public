#!/usr/bin/env python3
"""Independently verify release provenance, served hashes and archive contents.

Use the same --dev flag as build-release.py for local development artifacts.
Without it, this checker repeats the clean-worktree, exact v<version> tag and
version-bump gates. It accepts exactly six current-version archives, six served
platform pairs, SHA256SUMS and clients/manifest.json; stale output, private
material, IDE/Git metadata, unexpected archive members and unsafe paths fail.
"""
import argparse
import hashlib
import json
import os
import pathlib
import re
import stat
import subprocess
import tarfile
import zipfile

ROOT = pathlib.Path(__file__).resolve().parent.parent
TARGETS = [('linux', 'amd64'), ('linux', 'arm64'), ('darwin', 'amd64'),
           ('darwin', 'arm64'), ('windows', 'amd64'), ('windows', 'arm64')]
COMMON = {'README.md', 'QUICKSTART.ru.md', 'LICENSE', 'THIRD_PARTY_NOTICES.txt',
          'RIPGREP-COPYING', 'RIPGREP-LICENSE-MIT', 'RIPGREP-UNLICENSE'}
HASH = re.compile(r'[0-9a-f]{64}')
COMMIT = re.compile(r'[0-9a-f]{40}')
VERSION = re.compile(r'[0-9]+(?:\.[0-9]+){1,3}')


class CheckError(RuntimeError):
    pass


def require(condition, message):
    if not condition:
        raise CheckError(message)


def git(root, *arguments):
    result = subprocess.run(['git', *arguments], cwd=root, check=False,
                            capture_output=True, text=True)
    if result.returncode:
        raise CheckError(result.stderr.strip() or f'git {arguments[0]} failed')
    return result.stdout.strip()


def version_key(value):
    return tuple(int(part) for part in value.split('.'))


def expectations(root, development):
    match = re.search(r'^const version = "([0-9.]+)"$',
                      (root / 'main.go').read_text(), re.MULTILINE)
    require(match and VERSION.fullmatch(match.group(1)), 'invalid version in main.go')
    version = match.group(1)
    commit = git(root, 'rev-parse', 'HEAD')
    require(COMMIT.fullmatch(commit), 'HEAD is not a full commit ID')
    if not development:
        require(not git(root, 'status', '--porcelain=v1', '--untracked-files=all'),
                'release checker requires a clean Git worktree')
        expected = 'v' + version
        require(expected in git(root, 'tag', '--points-at', 'HEAD').splitlines(),
                f'release HEAD must be tagged exactly {expected}')
        require(git(root, 'rev-list', '-n', '1', expected) == commit,
                f'{expected} does not identify HEAD')
        previous = [tag[1:] for tag in git(root, 'tag', '--list', 'v*').splitlines()
                    if tag != expected and VERSION.fullmatch(tag[1:])]
        require(not previous or version_key(version) > max(map(version_key, previous)),
                f'release version {version} is not newer than previous tags')
    return version, commit


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def read_sums(path):
    result = {}
    for line in path.read_text().splitlines():
        match = re.fullmatch(r'([0-9a-f]{64})  ([^/]+)', line)
        require(match, f'malformed SHA256SUMS line: {line!r}')
        require(match.group(2) not in result, f'duplicate SHA256SUMS entry: {match.group(2)}')
        result[match.group(2)] = match.group(1)
    return result


def forbidden(name):
    path = pathlib.PurePosixPath(name)
    lowered = [part.lower() for part in path.parts]
    if path.is_absolute() or '..' in path.parts or any(not part for part in path.parts):
        return True
    if any(part in {'.git', '.idea', '.vscode', 'private', '__pycache__'} for part in lowered):
        return True
    if any(part.startswith('.git') or part in {'.env', '.credentials.json', 'verification.json',
                                               'admin.ru.md'} for part in lowered):
        return True
    if any('credential' in part or 'secret' in part or 'invitation' in part for part in lowered):
        return True
    if any(part.endswith(('.pem', '.key', '.pyc')) or part.startswith(('id_rsa', 'id_ed25519'))
           for part in lowered):
        return True
    return any(re.fullmatch(r'remote-claude-.*\.json', part) for part in lowered)


def archive_contents(archive, expected, binary_names):
    """Return archived executable bytes after strict regular-file/member checks."""
    found = {}
    if archive.suffix == '.zip':
        with zipfile.ZipFile(archive) as source:
            infos = source.infolist()
            names = [info.filename for info in infos]
            require(len(names) == len(set(names)), f'duplicate archive member in {archive.name}')
            require(not any(forbidden(name) for name in names), f'forbidden member in {archive.name}')
            require(set(names) == expected, f'unexpected archive members in {archive.name}: {names}')
            for info in infos:
                mode = info.external_attr >> 16
                require(not info.is_dir() and stat.S_ISREG(mode),
                        f'non-regular archive member: {archive.name}:{info.filename}')
                if pathlib.PurePosixPath(info.filename).name in binary_names:
                    found[pathlib.PurePosixPath(info.filename).name] = source.read(info)
    else:
        with tarfile.open(archive, 'r:gz') as source:
            members = source.getmembers()
            names = [member.name for member in members]
            require(len(names) == len(set(names)), f'duplicate archive member in {archive.name}')
            require(not any(forbidden(name) for name in names), f'forbidden member in {archive.name}')
            require(set(names) == expected, f'unexpected archive members in {archive.name}: {names}')
            for member in members:
                require(member.isfile(), f'non-regular archive member: {archive.name}:{member.name}')
                if pathlib.PurePosixPath(member.name).name in binary_names:
                    extracted = source.extractfile(member)
                    require(extracted is not None, f'cannot read {archive.name}:{member.name}')
                    found[pathlib.PurePosixPath(member.name).name] = extracted.read()
    require(set(found) == binary_names, f'missing archived binaries in {archive.name}')
    return found


def check(root=ROOT, development=False):
    version, commit = expectations(root, development)
    dist = root / 'dist'
    clients = dist / 'clients'
    manifest_path = clients / 'manifest.json'
    require(manifest_path.is_file(), 'clients/manifest.json is missing')
    manifest = json.loads(manifest_path.read_text())
    require(isinstance(manifest, dict), 'manifest must be an object')
    require(set(manifest) == {'version', 'commit', 'development', 'files'},
            'manifest has missing or unknown top-level fields')
    require(manifest['version'] == version, 'manifest version does not match main.go')
    require(manifest['commit'] == commit, 'manifest commit does not match HEAD')
    require(manifest['development'] is development,
            f'manifest development must be {str(development).lower()}')
    require(isinstance(manifest['files'], dict), 'manifest files must be an object')

    expected_archives = {}
    expected_platforms = set()
    allowed_dist = {'clients', 'SHA256SUMS', 'ripgrep-cache'}
    for system, arch in TARGETS:
        label = f'remote-claude-{version}-{system}-{arch}'
        archive_name = label + ('.zip' if system == 'windows' else '.tar.gz')
        expected_archives[archive_name] = label
        expected_platforms.add(f'{system}-{arch}')
        allowed_dist.add(archive_name)
    actual_dist = {path.name for path in dist.iterdir()}
    require(actual_dist <= allowed_dist, f'stale or forbidden dist output: {sorted(actual_dist - allowed_dist)}')
    require(all(name in actual_dist for name in expected_archives), 'one or more release archives are missing')
    require('SHA256SUMS' in actual_dist and 'clients' in actual_dist, 'incomplete dist output')
    require(not (dist / 'ripgrep-cache').is_symlink(), 'ripgrep-cache must not be a symlink')
    require(set(manifest['files']) == expected_platforms, 'manifest platforms do not match the six targets')
    require({path.name for path in clients.iterdir()} == {'manifest.json', version},
            'stale or unexpected clients output')
    require({path.name for path in (clients / version).iterdir()} == expected_platforms,
            'stale or unexpected served platform output')

    sums = read_sums(dist / 'SHA256SUMS')
    require(set(sums) == set(expected_archives), 'SHA256SUMS must list exactly the six archives')
    for archive_name, label in expected_archives.items():
        archive = dist / archive_name
        require(sha(archive) == sums[archive_name], f'archive checksum mismatch: {archive_name}')
        system, arch = label.rsplit('-', 2)[-2:]
        executable = 'remote-claude.exe' if system == 'windows' else 'remote-claude'
        search = 'remote-claude-rg.exe' if system == 'windows' else 'remote-claude-rg'
        installers = {'install.cmd', 'install.ps1'} if system == 'windows' else {'install.sh'}
        member_names = COMMON | installers | {executable, search}
        expected = {f'{label}/{name}' for name in member_names}
        archived = archive_contents(archive, expected, {executable, search})
        platform_name = f'{system}-{arch}'
        served = clients / version / platform_name
        require({path.name for path in served.iterdir()} == {executable, search},
                f'unexpected served files for {platform_name}')
        files = manifest['files'][platform_name]
        require(set(files) == {executable, search}, f'wrong manifest files for {platform_name}')
        for name in [executable, search]:
            require(HASH.fullmatch(files[name]), f'invalid served hash for {platform_name}/{name}')
            require(sha(served / name) == files[name], f'served hash mismatch for {platform_name}/{name}')
            archived_hash = hashlib.sha256(archived[name]).hexdigest()
            require(archived_hash == files[name],
                    f'archive/served binary mismatch for {archive_name}:{platform_name}/{name}: '
                    f'{archived_hash} != {files[name]}')
    print(f'OK: {version} commit {commit} development={str(development).lower()}; '
          'six archives, served hashes and member allowlists verified')
    return manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--dev', action='store_true', help='validate development artifacts')
    args = parser.parse_args()
    try:
        check(ROOT, args.dev)
    except (CheckError, OSError, ValueError, KeyError, tarfile.TarError, zipfile.BadZipFile) as error:
        raise SystemExit(f'check-release: {error}')


if __name__ == '__main__':
    main()
