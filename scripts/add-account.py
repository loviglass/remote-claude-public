#!/usr/bin/env python3
"""Run as root on Ubuntu: add one managed account and write its invitation.

    python3 add-account.py --host server.example.com developer

The host is required with --host or REMOTE_CLAUDE_HOST. Linux-safe usernames
are accepted. useradd chooses the UID by default; --uid-min and --uid-max may
be supplied together to allocate from a controlled egress-routing range.
Credentials are not copied here: remote-claude-auth-sync.service creates them.
The bootstrap key permits only the one-time enrollment command.
"""
import argparse
import json
import os
import pathlib
import pwd
import re
import stat
import subprocess

BASE = pathlib.Path('/opt/remote-claude')
INVITES = pathlib.Path('/root/remote-claude-invitations')
SUDOERS = pathlib.Path('/etc/sudoers.d/remote-claude-jobs')
JOB_COMMANDS = ('/opt/remote-claude/remote-claude server job-image *, '
                '/opt/remote-claude/remote-claude server job-container *, '
                '/opt/remote-claude/remote-claude server job-list')
USERNAME = re.compile(r'[a-z_][a-z0-9_-]{0,31}')
HOST = re.compile(r'[A-Za-z0-9_.:%\[\]-]{1,253}')


def managed(entry):
    try:
        marker = os.stat(pathlib.Path(entry.pw_dir) / '.remote-claude-managed',
                         follow_symlinks=False)
        return stat.S_ISREG(marker.st_mode)
    except OSError:
        return False


def resumable(account, invitation):
    """Recognize only a complete, not-yet-enrolled managed account."""
    home = pathlib.Path(account.pw_dir)
    owned_files = [home / '.remote-claude-managed', home / '.ssh/enrollment.pending']
    try:
        owned_stats = [path.stat(follow_symlinks=False) for path in owned_files]
        invitation_stat = invitation.stat(follow_symlinks=False)
    except OSError:
        return False
    return (all(stat.S_ISREG(info.st_mode) and info.st_uid == account.pw_uid
                for info in owned_stats)
            and stat.S_ISREG(invitation_stat.st_mode) and invitation_stat.st_uid == 0)


def parser():
    result = argparse.ArgumentParser(description=__doc__)
    result.add_argument('name', help='new Linux username')
    result.add_argument('--host', default=os.environ.get('REMOTE_CLAUDE_HOST'),
                        help='public SSH host[:port] (or REMOTE_CLAUDE_HOST)')
    result.add_argument('--uid-min', type=int, help='lowest allocatable egress UID')
    result.add_argument('--uid-max', type=int, help='highest allocatable egress UID')
    result.add_argument('--resume', action='store_true',
                        help='resume only an existing managed account with pending enrollment')
    return result


def main():
    args = parser().parse_args()
    if not USERNAME.fullmatch(args.name):
        raise SystemExit('name must be a safe Linux username: [a-z_][a-z0-9_-]{0,31}')
    if not args.host or not HOST.fullmatch(args.host):
        raise SystemExit('a valid --host or REMOTE_CLAUDE_HOST is required')
    if (args.uid_min is None) != (args.uid_max is None):
        raise SystemExit('--uid-min and --uid-max must be supplied together')
    if args.uid_min is not None:
        if not (1 <= args.uid_min <= args.uid_max <= 2**32 - 2):
            raise SystemExit('UID range must satisfy 1 <= uid-min <= uid-max <= 4294967294')
        egress_uids = range(args.uid_min, args.uid_max + 1)
    else:
        egress_uids = None

    try:
        existing = pwd.getpwnam(args.name)
    except KeyError:
        existing = None
    invitation = INVITES / f'remote-claude-{args.name}.json'
    if existing is not None:
        if not args.resume:
            raise SystemExit(f'{args.name} already exists; use --resume only to recover '
                             f'a pending enrollment, or retrieve {invitation} securely')
        if not resumable(existing, invitation):
            raise SystemExit(f'{args.name} is not a resumable managed account with '
                             'pending enrollment and a root-owned invitation')
        print(f'{args.name}: resuming pending enrollment, invitation {invitation}')
        return
    if args.resume:
        raise SystemExit(f'cannot resume {args.name}: account does not exist')
    INVITES.mkdir(mode=0o700, exist_ok=True)
    if invitation.exists():
        raise SystemExit(f'Stale invitation without an account: {invitation}')

    command = ['useradd', '--create-home', '--shell', '/bin/bash']
    if egress_uids is not None:
        taken = {entry.pw_uid for entry in pwd.getpwall()}
        uid = next((candidate for candidate in egress_uids if candidate not in taken), None)
        if uid is None:
            raise SystemExit('no free UID in the requested range')
        command += ['--uid', str(uid)]
    subprocess.run(command + [args.name], check=True)
    account = pwd.getpwnam(args.name)
    home = pathlib.Path(account.pw_dir)
    home.chmod(0o700)
    (home / '.remote-claude-managed').touch(mode=0o600)
    for relative in ['.ssh', '.claude', '.local/bin', '.local/share/remote-claude']:
        (home / relative).mkdir(parents=True, mode=0o700, exist_ok=True)

    # Skip first-run onboarding and copy only non-secret account metadata from
    # another explicitly managed account. Credential files are never read here.
    config = {'hasCompletedOnboarding': True}
    for entry in sorted(pwd.getpwall(), key=lambda item: item.pw_uid):
        if entry.pw_name == args.name or not managed(entry):
            continue
        try:
            fd = os.open(pathlib.Path(entry.pw_dir) / '.claude.json', os.O_RDONLY | os.O_NOFOLLOW)
            with os.fdopen(fd) as stream:
                existing = json.load(stream)
        except (OSError, ValueError):
            continue
        if isinstance(existing, dict) and 'oauthAccount' in existing:
            for key in ['lastOnboardingVersion', 'oauthAccount']:
                if key in existing:
                    config[key] = existing[key]
            break
    (home / '.claude.json').write_text(json.dumps(config))
    (home / '.claude.json').chmod(0o600)
    (home / '.local/bin/claude').symlink_to(BASE / 'claude')

    key_path = INVITES / f'remote-claude-{args.name}.bootstrap'
    subprocess.run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '',
                    '-C', f'{args.name} enrollment', '-f', str(key_path)], check=True)
    public = key_path.with_suffix('.bootstrap.pub')
    authorized = home / '.ssh/authorized_keys'
    authorized.write_text('restrict,command="/opt/remote-claude/remote-claude enroll" '
                          + public.read_text().strip() + '\n')
    authorized.chmod(0o600)
    (home / '.ssh/enrollment.pending').touch(mode=0o600)
    fingerprint = subprocess.check_output(
        ['ssh-keygen', '-lf', '/etc/ssh/ssh_host_ed25519_key.pub'], text=True).split()[1]
    document = {'host': args.host, 'user': args.name, 'fingerprint': fingerprint,
                'bootstrap_key': key_path.read_text()}
    invitation.write_text(json.dumps(document, indent=2) + '\n')
    invitation.chmod(0o600)
    key_path.unlink()
    public.unlink()

    for parent, dirs, files in os.walk(home):
        os.chown(parent, account.pw_uid, account.pw_gid)
        for child in dirs + files:
            os.chown(os.path.join(parent, child), account.pw_uid, account.pw_gid,
                     follow_symlinks=False)

    if SUDOERS.is_file():
        rules = SUDOERS.read_text()
        if not rules.endswith('\n'):
            rules += '\n'
        rules += f'{args.name} ALL=(root) NOPASSWD: {JOB_COMMANDS}\n'
        candidate = SUDOERS.with_name('.remote-claude-jobs.new')
        candidate.write_text(rules)
        candidate.chmod(0o440)
        try:
            subprocess.run(['visudo', '-cqf', str(candidate)], check=True)
            candidate.replace(SUDOERS)
        finally:
            candidate.unlink(missing_ok=True)
    print(f'{args.name}: UID {account.pw_uid}, invitation {invitation}')


if __name__ == '__main__':
    main()
