#!/usr/bin/env python3
"""Install Remote Claude's server files on Ubuntu, without provisioning users.

Run as root from a source checkout after building the Linux amd64 server:

    python3 scripts/build-release.py --dev
    sudo python3 -I scripts/install-server.py \
      --claude-binary /path/to/already-installed/claude \
      [--ripgrep /path/to/rg]

Options:
  --source DIR         Source checkout (default: this script's repository).
  --server-binary PATH Built Linux server executable. Defaults to
                       SOURCE/dist/clients/VERSION/linux-amd64/remote-claude.
  --claude-binary PATH Required executable for the supported Claude Code
                       release; it is checked with --version before installation.
  --ripgrep PATH       Optional existing rg executable for server-side search.

The fixed product layout is /opt/remote-claude. This installs the server,
Claude, optional rg, helper scripts, job Dockerfile, shell and Windows pwsh
shims, and auth-sync systemd units. Binaries are replaced atomically and the
auth-sync timer is enabled. It never creates users or copies credentials;
use add-account.py separately after arranging managed-account enrollment.
"""
import argparse
import os
import pathlib
import platform
import re
import shutil
import stat
import subprocess
import tempfile

ROOT = pathlib.Path(__file__).resolve().parent.parent
BASE = pathlib.Path('/opt/remote-claude')
SYSTEMD = pathlib.Path('/etc/systemd/system')
HELPERS = ['add-account.py', 'auth-sync.py', 'configure-resources.py', 'pool-governor.py']
UNITS = ['remote-claude-auth-sync.service', 'remote-claude-auth-sync.timer']


def source_version(source):
    match = re.search(r'^const version = "([0-9.]+)"$',
                      (source / 'main.go').read_text(), re.MULTILINE)
    if not match:
        raise SystemExit(f'cannot determine version from {source / "main.go"}')
    return match.group(1)


def supported_claude(source):
    match = re.search(r'^const supportedClaudeVersion = "([0-9.]+)"$',
                      (source / 'claudeversion.go').read_text(), re.MULTILINE)
    if not match:
        raise SystemExit('cannot determine supported Claude Code version')
    return match.group(1)


def executable(path, label):
    try:
        info = path.stat()
    except OSError as error:
        raise SystemExit(f'{label} is missing or unreadable: {path}: {error}')
    if not stat.S_ISREG(info.st_mode) or not info.st_mode & 0o111:
        raise SystemExit(f'{label} must be an executable regular file: {path}')
    return path.resolve()


def atomic_file(source, destination, mode=0o755):
    """Copy bytes to a same-directory temporary file, then atomically replace."""
    destination.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix='.remote-claude-install-', dir=destination.parent)
    try:
        with os.fdopen(fd, 'wb') as output, source.open('rb') as input_file:
            shutil.copyfileobj(input_file, output)
            output.flush()
            os.fsync(output.fileno())
        os.chmod(temporary, mode)
        os.replace(temporary, destination)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass


def atomic_bytes(content, destination, mode=0o755):
    temporary = destination.parent / f'.remote-claude-install-{os.getpid()}'
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, mode)
    try:
        with os.fdopen(fd, 'wb') as stream:
            stream.write(content)
            stream.flush()
            os.fsync(stream.fileno())
        os.chmod(temporary, mode)
        os.replace(temporary, destination)
    finally:
        temporary.unlink(missing_ok=True)


def install(args):
    if platform.system() != 'Linux' or not hasattr(os, 'geteuid') or os.geteuid() != 0:
        raise SystemExit('install-server.py must run as root on Ubuntu Linux')
    os_release = {}
    try:
        for line in pathlib.Path('/etc/os-release').read_text().splitlines():
            if '=' in line:
                key, value = line.split('=', 1)
                os_release[key] = value.strip().strip('"')
    except OSError as error:
        raise SystemExit(f'cannot identify this Linux distribution: {error}')
    if os_release.get('ID') != 'ubuntu':
        raise SystemExit('install-server.py supports Ubuntu Linux only')

    source = args.source.resolve()
    version = source_version(source)
    server = args.server_binary or source / 'dist/clients' / version / 'linux-amd64/remote-claude'
    server = executable(server, 'server binary')
    claude = executable(args.claude_binary, 'Claude binary')
    required = [source / 'scripts' / name for name in HELPERS + UNITS + ['job-base.Dockerfile']]
    for path in required:
        if not path.is_file():
            raise SystemExit(f'required source file is missing: {path}')
    supported = supported_claude(source)
    try:
        result = subprocess.run([str(claude), '--version'], check=False, capture_output=True,
                                text=True, timeout=30)
    except (OSError, subprocess.TimeoutExpired) as error:
        raise SystemExit(f'cannot execute Claude binary for version validation: {error}')
    output = result.stdout.strip()
    if result.returncode != 0 or not output.startswith(supported + ' '):
        shown = output or result.stderr.strip() or f'exit status {result.returncode}'
        raise SystemExit(f'unsupported Claude Code version: expected {supported}, got: {shown}')
    ripgrep = executable(args.ripgrep, 'ripgrep') if args.ripgrep else None

    old_umask = os.umask(0o077)
    try:
        BASE.mkdir(parents=True, exist_ok=True)
        BASE.chmod(0o755)
        for binary_source, name in [(server, 'remote-claude'), (claude, 'claude')]:
            atomic_file(binary_source, BASE / name)
        if ripgrep:
            atomic_file(ripgrep, BASE / 'rg')
        for name in HELPERS:
            atomic_file(source / 'scripts' / name, BASE / name)
        atomic_file(source / 'scripts/job-base.Dockerfile', BASE / 'job-base.Dockerfile', 0o644)
        for name in UNITS:
            atomic_file(source / 'scripts' / name, SYSTEMD / name, 0o644)
        atomic_bytes(b'#!/bin/sh\nexec /opt/remote-claude/remote-claude shell "$@"\n', BASE / 'shell')
        windows = BASE / 'windows'
        windows.mkdir(exist_ok=True)
        windows.chmod(0o755)
        temporary = windows / f'.pwsh-{os.getpid()}'
        temporary.unlink(missing_ok=True)
        try:
            temporary.symlink_to(BASE / 'remote-claude')
            os.replace(temporary, windows / 'pwsh')
        finally:
            temporary.unlink(missing_ok=True)
    finally:
        os.umask(old_umask)
    subprocess.run(['systemctl', 'daemon-reload'], check=True)
    subprocess.run(['systemctl', 'enable', '--now', 'remote-claude-auth-sync.timer'], check=True)
    print(f'Installed Remote Claude {version} and Claude Code {supported} in {BASE}')
    print('No users or credentials were changed. The auth-sync timer is enabled.')


def main():
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--source', type=pathlib.Path, default=ROOT,
                        help='source checkout (default: repository containing this script)')
    parser.add_argument('--server-binary', type=pathlib.Path,
                        help='built Linux amd64 remote-claude executable')
    parser.add_argument('--claude-binary', type=pathlib.Path, required=True,
                        help='already-installed supported Claude Code executable')
    parser.add_argument('--ripgrep', type=pathlib.Path, help='optional existing rg executable')
    args = parser.parse_args()
    install(args)


if __name__ == '__main__':
    main()
