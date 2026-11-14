#!/usr/bin/env python3
"""Build six provenance-bound client archives and their server update manifest.

Release mode is the default: it requires a completely clean Git worktree, HEAD
tagged exactly v<version>, and a version newer than every earlier release tag.
Use --dev only for local verification; development manifests are explicitly
marked and must not be published. The full HEAD commit is injected into every
binary and recorded in clients/manifest.json.

The builder uses host Go when available, otherwise golang:1.26.8 through Docker.
Only dist/ripgrep-cache survives output cleanup. Pinned ripgrep downloads are
verified by vendor_ripgrep.py; all other bundle inputs come from public source.
"""
import argparse
import gzip
import hashlib
import json
import os
import pathlib
import re
import shutil
import stat
import subprocess
import tarfile
import tempfile
import zipfile

ROOT = pathlib.Path(__file__).resolve().parent.parent
TARGETS = [('linux', 'amd64'), ('linux', 'arm64'), ('darwin', 'amd64'),
           ('darwin', 'arm64'), ('windows', 'amd64'), ('windows', 'arm64')]
PUBLIC_DOCS = ['README.md', 'QUICKSTART.ru.md', 'LICENSE', 'THIRD_PARTY_NOTICES.txt']
VERSION_RE = re.compile(r'[0-9]+(?:\.[0-9]+){1,3}')
COMMIT_RE = re.compile(r'[0-9a-f]{40}')


class BuildError(RuntimeError):
    pass


def git(root, *arguments):
    result = subprocess.run(['git', *arguments], cwd=root, check=False,
                            capture_output=True, text=True)
    if result.returncode:
        raise BuildError(result.stderr.strip() or f'git {arguments[0]} failed')
    return result.stdout.strip()


def source_version(root):
    match = re.search(r'^const version = "([0-9.]+)"$',
                      (root / 'main.go').read_text(), re.MULTILINE)
    if not match or not VERSION_RE.fullmatch(match.group(1)):
        raise BuildError('main.go does not contain a valid const version')
    return match.group(1)


def version_key(value):
    return tuple(int(part) for part in value.split('.'))


def provenance(root, development=False, required_tag=None):
    """Return (version, full HEAD commit), enforcing all publication gates."""
    commit = git(root, 'rev-parse', 'HEAD')
    if not COMMIT_RE.fullmatch(commit):
        raise BuildError('HEAD did not resolve to a full commit ID')
    version = source_version(root)
    if development:
        return version, commit
    if git(root, 'status', '--porcelain=v1', '--untracked-files=all'):
        raise BuildError('release builds require a clean Git worktree; use --dev only for local checks')
    expected = 'v' + version
    if required_tag is not None and required_tag != expected:
        raise BuildError(f'workflow tag {required_tag!r} does not match source version tag {expected!r}')
    if expected not in git(root, 'tag', '--points-at', 'HEAD').splitlines():
        raise BuildError(f'release HEAD must be tagged exactly {expected}')
    if git(root, 'rev-list', '-n', '1', expected) != commit:
        raise BuildError(f'{expected} does not identify HEAD')
    previous = []
    for tag in git(root, 'tag', '--list', 'v*').splitlines():
        candidate = tag[1:]
        if tag != expected and VERSION_RE.fullmatch(candidate):
            previous.append(candidate)
    if previous and version_key(version) <= max(map(version_key, previous)):
        raise BuildError(f'release version {version} must be newer than all previous release tags')
    return version, commit


def remove_tree(path):
    """Remove generated trees without following links, including Go module caches."""
    if path.is_symlink():
        path.unlink()
        return
    if not path.exists():
        return
    for current, directories, files in os.walk(path):
        os.chmod(current, 0o700)
        for name in files:
            candidate = pathlib.Path(current) / name
            if candidate.is_symlink():
                continue
            try:
                os.chmod(candidate, 0o600)
            except OSError:
                pass
    shutil.rmtree(path)


def clean_dist(dist):
    if dist.is_symlink() or (dist.exists() and not dist.is_dir()):
        raise BuildError(f'dist must be a real directory: {dist}')
    dist.mkdir(exist_ok=True)
    cache = dist / 'ripgrep-cache'
    if cache.is_symlink() or (cache.exists() and not cache.is_dir()):
        raise BuildError('dist/ripgrep-cache must be a real directory')
    for path in dist.iterdir():
        if path.name == 'ripgrep-cache':
            continue
        if path.is_dir() and not path.is_symlink():
            remove_tree(path)
        else:
            path.unlink()


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def go_build(root, dist, system, arch, commit, output, use_docker):
    arguments = ['build', '-trimpath', '-buildvcs=false', '-ldflags',
                 f'-s -w -X main.buildCommit={commit}', '-o', str(output), '.']
    environment = dict(os.environ, GOOS=system, GOARCH=arch, CGO_ENABLED='0',
                       GOFLAGS='-mod=readonly')
    if not use_docker:
        subprocess.run(['go', *arguments], cwd=root, env=environment, check=True)
        return
    cache = dist / '.go-cache'
    (cache / 'build').mkdir(parents=True, exist_ok=True)
    (cache / 'mod').mkdir(parents=True, exist_ok=True)
    container_output = pathlib.PurePosixPath('/src') / output.relative_to(root).as_posix()
    command = ['docker', 'run', '--rm', '-v', f'{root}:/src', '-v', f'{cache}:/cache',
               '-w', '/src', '-e', f'GOOS={system}', '-e', f'GOARCH={arch}',
               '-e', 'CGO_ENABLED=0', '-e', 'GOFLAGS=-mod=readonly',
               '-e', 'GOCACHE=/cache/build', '-e', 'GOMODCACHE=/cache/mod']
    if hasattr(os, 'getuid'):
        command += ['--user', f'{os.getuid()}:{os.getgid()}']
    command += ['golang:1.26.8', 'go', 'build', '-trimpath', '-buildvcs=false',
                '-ldflags', f'-s -w -X main.buildCommit={commit}',
                '-o', str(container_output), '.']
    subprocess.run(command, cwd=root, check=True)


def write_archive(archive, label, members):
    """Write stable ordering, ownership and timestamps for repeatable packaging."""
    if archive.suffix == '.zip':
        with zipfile.ZipFile(archive, 'w', compression=zipfile.ZIP_DEFLATED,
                             compresslevel=9) as output:
            for path in members:
                info = zipfile.ZipInfo(f'{label}/{path.name}', date_time=(1980, 1, 1, 0, 0, 0))
                info.create_system = 3
                info.compress_type = zipfile.ZIP_DEFLATED
                info.external_attr = (stat.S_IFREG | (path.stat().st_mode & 0o777)) << 16
                output.writestr(info, path.read_bytes())
        return
    with archive.open('wb') as raw:
        with gzip.GzipFile(filename='', mode='wb', fileobj=raw, mtime=0) as compressed:
            with tarfile.open(fileobj=compressed, mode='w', format=tarfile.USTAR_FORMAT) as output:
                for path in members:
                    info = tarfile.TarInfo(f'{label}/{path.name}')
                    info.size = path.stat().st_size
                    info.mode = path.stat().st_mode & 0o777
                    info.mtime = 0
                    info.uid = info.gid = 0
                    info.uname = info.gname = ''
                    with path.open('rb') as source:
                        output.addfile(info, source)


def build(root=ROOT, development=False, required_tag=None):
    version, commit = provenance(root, development, required_tag)
    if development:
        print('DEVELOPMENT BUILD: dirty/untagged source is allowed; DO NOT PUBLISH.', flush=True)
    dist = root / 'dist'
    clean_dist(dist)
    use_docker = shutil.which('go') is None
    if use_docker and shutil.which('docker') is None:
        raise BuildError('Go is required on PATH, or Docker for golang:1.26.8')
    from vendor_ripgrep import bundle

    clients = dist / 'clients'
    manifest = {'version': version, 'commit': commit, 'development': development, 'files': {}}
    archives = []
    try:
        for system, arch in TARGETS:
            label = f'remote-claude-{version}-{system}-{arch}'
            stage = pathlib.Path(tempfile.mkdtemp(prefix='.stage-', dir=dist))
            try:
                executable_name = 'remote-claude.exe' if system == 'windows' else 'remote-claude'
                executable_path = stage / executable_name
                go_build(root, dist, system, arch, commit, executable_path, use_docker)
                executable_path.chmod(0o755)
                for name in PUBLIC_DOCS:
                    source = root / name
                    if not source.is_file():
                        raise BuildError(f'missing public bundle input: {name}')
                    shutil.copyfile(source, stage / name)
                    (stage / name).chmod(0o644)
                installers = ['install.cmd', 'install.ps1'] if system == 'windows' else ['install.sh']
                for name in installers:
                    shutil.copyfile(root / name, stage / name)
                    (stage / name).chmod(0o755)
                bundle(system, arch, stage, dist / 'ripgrep-cache')
                members = sorted(stage.iterdir(), key=lambda path: path.name)
                if any(not path.is_file() for path in members):
                    raise BuildError(f'unexpected non-file in staging directory: {stage}')

                platform_name = f'{system}-{arch}'
                served = clients / version / platform_name
                served.mkdir(parents=True)
                search_name = 'remote-claude-rg.exe' if system == 'windows' else 'remote-claude-rg'
                manifest['files'][platform_name] = {}
                for name in [executable_name, search_name]:
                    shutil.copyfile(stage / name, served / name)
                    (served / name).chmod(0o755)
                    manifest['files'][platform_name][name] = digest(stage / name)
                archive = dist / (label + ('.zip' if system == 'windows' else '.tar.gz'))
                write_archive(archive, label, members)
                archives.append(archive)
                print(label, flush=True)
            finally:
                shutil.rmtree(stage)
    finally:
        remove_tree(dist / '.go-cache')
    (clients / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
    (dist / 'SHA256SUMS').write_text(''.join(
        f'{digest(path)}  {path.name}\n' for path in archives))
    if development:
        print('Development artifacts are for local verification only, not publishing.', flush=True)
    return manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--dev', action='store_true',
                        help='allow dirty/untagged source and mark development=true (never publish)')
    parser.add_argument('--require-tag', help='require this workflow tag to match the source version')
    args = parser.parse_args()
    if args.dev and args.require_tag:
        parser.error('--require-tag cannot be combined with --dev')
    try:
        build(ROOT, args.dev, args.require_tag)
    except (BuildError, OSError, subprocess.CalledProcessError, ValueError) as error:
        raise SystemExit(f'build-release: {error}')


if __name__ == '__main__':
    main()
