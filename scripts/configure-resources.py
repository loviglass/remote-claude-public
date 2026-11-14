#!/usr/bin/env python3
"""Enable budgets for NEW remote-claude sessions. Does not move or kill live work.
Run as root on Ubuntu. Per-user budgets include all managed sessions/children.

Server-mode jobs get one pool for all users instead: a low-priority system
slice supervised by pool-governor.py. Each job is a container that Docker
places in the user's part of that slice; its terminal is held by a per-user
tmux service. This script builds the default job image. With no names, only
accounts carrying the managed marker are configured.
"""
import argparse
import os
import pathlib
import pwd
import shutil
import stat
import subprocess

SYSTEM = pathlib.Path('/etc/systemd/system')
GOVERNOR = pathlib.Path('/opt/remote-claude/pool-governor.py')
# Weights only matter under contention: idle CPU and I/O stay usable, busy
# neighbours win. MemoryMax is the hard size of the pool; the governor lowers
# the soft limit below it as the host fills up.
POOL = {
    'remoteclaude.slice': '[Unit]\nDescription=Remote Claude server-mode pool\n[Slice]\nCPUWeight=10\nIOWeight=10\nMemoryMax=50%\nTasksMax=4096\n',
    # exit-empty keeps the tmux server, and with it the cgroup placement, alive
    # between jobs. One job's OOM kill must not take the others down with it.
    'remote-claude-jobs@.service': '[Unit]\nDescription=Remote Claude server-mode jobs of UID %i\n[Service]\nType=forking\nUser=%i\nWorkingDirectory=~\nSlice=remoteclaude-u%i.slice\nEnvironment=LANG=C.UTF-8\nExecStart=/usr/bin/tmux -L remote-claude-jobs start-server \\; set-option -g exit-empty off\nRestart=always\nRestartSec=2\nOOMPolicy=continue\nMemoryAccounting=yes\nTasksMax=1024\n[Install]\nWantedBy=multi-user.target\n',
    'remote-claude-pool-governor.service': f'[Unit]\nDescription=Remote Claude server-mode pool governor\n[Service]\nExecStart=/usr/bin/python3 {GOVERNOR}\nRestart=always\nRestartSec=5\n[Install]\nWantedBy=multi-user.target\n',
}
BASE_IMAGE = pathlib.Path('/opt/remote-claude/job-base.Dockerfile')


def is_managed(account):
    try:
        marker = os.stat(pathlib.Path(account.pw_dir) / '.remote-claude-managed',
                         follow_symlinks=False)
        return stat.S_ISREG(marker.st_mode)
    except OSError:
        return False


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('names', nargs='*', metavar='NAME',
                        help='managed account to configure (default: every marked account)')
    args = parser.parse_args()
    if not hasattr(os, 'geteuid') or os.geteuid() != 0:
        raise SystemExit('configure-resources.py must run as root')
    for required in [GOVERNOR, BASE_IMAGE]:
        if not required.is_file():
            raise SystemExit(f'Install {required.name} at {required} first')
    for tool in ['tmux', 'docker']:
        if not shutil.which(tool):
            raise SystemExit(f'Server mode needs {tool}')
    users = args.names or sorted(entry.pw_name for entry in pwd.getpwall() if is_managed(entry))
    accounts = []
    for name in users:
        try:
            account = pwd.getpwnam(name)
        except KeyError:
            raise SystemExit(f'Unknown account: {name}')
        if not is_managed(account):
            raise SystemExit(f'Not a managed account: {name}')
        accounts.append(account)

    for name, content in POOL.items():
        dest = SYSTEM / name
        if dest.exists() and dest.read_text() != content:
            raise SystemExit(f'Existing custom policy needs review: {dest}')
        dest.write_text(content)
    subprocess.run(['systemctl', 'daemon-reload'], check=True)
    subprocess.run(['systemctl', 'enable', '--now', 'remote-claude-pool-governor.service'], check=True)

    for account in accounts:
        name = account.pw_name
        base = pathlib.Path(account.pw_dir)
        config = base / '.config/systemd/user'
        config.mkdir(parents=True, exist_ok=True)
        policies = {
            'remote-claude.slice': '[Unit]\nDescription=Remote Claude aggregate budget\n[Slice]\nCPUAccounting=yes\nMemoryAccounting=yes\nCPUWeight=100\nCPUQuota=150%\nMemoryHigh=1536M\nMemoryMax=2048M\nTasksMax=256\n',
        }
        for relative, content in policies.items():
            dest = config / relative
            if dest.exists() and dest.read_text() != content:
                raise SystemExit(f'Existing custom policy needs review: {dest}')
            dest.write_text(content)
            os.chown(dest, account.pw_uid, account.pw_gid)
        for directory in [base / '.config', base / '.config/systemd', config]:
            os.chown(directory, account.pw_uid, account.pw_gid)
        subprocess.run(['loginctl', 'enable-linger', name], check=True)
        subprocess.run(['systemctl', 'start', f'user@{account.pw_uid}.service'], check=True)
        subprocess.run(['runuser', '-u', name, '--', 'env', f'XDG_RUNTIME_DIR=/run/user/{account.pw_uid}',
                        f'DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/{account.pw_uid}/bus',
                        'systemctl', '--user', 'daemon-reload'], check=True)
        marker = base / '.local/share/remote-claude/resources.enabled'
        marker.parent.mkdir(parents=True, exist_ok=True)
        marker.touch(mode=0o600)
        os.chown(marker, account.pw_uid, account.pw_gid)
        subprocess.run(['systemctl', 'enable', '--now',
                        f'remote-claude-jobs@{account.pw_uid}.service'], check=True)
        print(f'{name}: new bridged sessions share 2 GiB RAM / 1.5 CPU; server mode enabled in the shared pool')
    with BASE_IMAGE.open('rb') as dockerfile:
        subprocess.run(['docker', 'build', '--tag', 'remote-claude-job-base:latest', '-'],
                       stdin=dockerfile, check=True)


if __name__ == '__main__':
    main()
