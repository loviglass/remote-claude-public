#!/usr/bin/env python3
"""Create a managed account and download its invitation over SSH.

Run from a source checkout on an administrator's computer:

    python3 scripts/new-invitation.py --host server.example.com developer

--host (or REMOTE_CLAUDE_HOST) is required. --admin-user selects the SSH
administrator (default root); non-root administrators must have non-interactive
sudo. By default only the account and auth-sync enrollment are prepared.
--resources additionally configures resource limits and server-mode jobs,
which requires Docker and tmux on the server. HTTP CONNECT proxy options match
the client. The secret invitation is written as
private/remote-claude-NAME.json with mode 0600 and is never printed.
"""
import argparse
import base64
import os
import pathlib
import re
import shlex
import subprocess

ROOT = pathlib.Path(__file__).resolve().parent.parent
USERNAME = re.compile(r'[a-z_][a-z0-9_-]{0,31}')
HOST = re.compile(r'[A-Za-z0-9_.:%\[\]-]{1,253}')


def ssh_destination(value):
    """Split an invitation host into an OpenSSH destination and optional port."""
    if value.startswith('['):
        closing = value.find(']')
        if closing < 0:
            raise ValueError('unclosed bracket in host')
        hostname = value[1:closing]
        suffix = value[closing + 1:]
        if not suffix:
            port = None
        elif not suffix.startswith(':') or not suffix[1:].isdigit():
            raise ValueError('bracketed host must end at ] or include a numeric port')
        else:
            port = suffix[1:]
    elif value.count(':') == 1:
        hostname, port = value.rsplit(':', 1)
        if not port.isdigit():
            raise ValueError('host port must be numeric')
    else:
        # Raw IPv6 addresses have no port; use [address]:port to specify one.
        hostname, port = value, None
    if not hostname or (port is not None and not 1 <= int(port) <= 65535):
        raise ValueError('host must be non-empty and its port must be between 1 and 65535')
    return hostname, port


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('name', help='new Linux username')
    parser.add_argument('--host', default=os.environ.get('REMOTE_CLAUDE_HOST'),
                        help='server SSH host[:port] (or REMOTE_CLAUDE_HOST)')
    parser.add_argument('--admin-user', default='root', help='SSH administrator (default: root)')
    parser.add_argument('--resources', action='store_true',
                        help='configure resource limits and server-mode jobs (requires Docker/tmux)')
    parser.add_argument('--resume', action='store_true',
                        help='recover an existing managed account with pending enrollment')
    parser.add_argument('--proxy', action='store_true', help='SSH through an HTTP CONNECT proxy')
    parser.add_argument('--proxy-host', default='127.0.0.1')
    parser.add_argument('--proxy-port', type=int, default=10809)
    args = parser.parse_args()
    for label, value in [('name', args.name), ('admin user', args.admin_user)]:
        if not USERNAME.fullmatch(value):
            parser.error(f'{label} must be a safe Linux username')
    if not args.host or not HOST.fullmatch(args.host):
        parser.error('a valid --host or REMOTE_CLAUDE_HOST is required')
    if not HOST.fullmatch(args.proxy_host) or not 1 <= args.proxy_port <= 65535:
        parser.error('invalid HTTP CONNECT proxy address')

    try:
        ssh_host, ssh_port = ssh_destination(args.host)
    except ValueError as error:
        parser.error(str(error))
    ssh = ['ssh', '-o', 'BatchMode=yes']
    if ssh_port is not None:
        ssh += ['-p', ssh_port]
    if args.proxy:
        proxy_host = args.proxy_host
        if ':' in proxy_host and not proxy_host.startswith('['):
            proxy_host = f'[{proxy_host}]'
        proxy = f'nc -X connect -x {shlex.quote(proxy_host)}:{args.proxy_port} %h %p'
        ssh += ['-o', f'ProxyCommand={proxy}']
    target = f'{args.admin_user}@{ssh_host}'

    def remote(command, **kwargs):
        if args.admin_user != 'root':
            command = f'sudo -n sh -c {shlex.quote(command)}'
        return subprocess.run(ssh + [target, command], check=True, **kwargs)

    scripts = ['add-account.py', 'auth-sync.py', 'configure-resources.py']
    upload = 'tmp=$(mktemp -d /tmp/remote-claude-invitation.XXXXXX)\n' \
             'trap \'rm -rf "$tmp"\' EXIT\n'
    upload += ''.join(
        f'base64 -d > "$tmp/{name}" <<"EOF"\n'
        + base64.b64encode((ROOT / 'scripts' / name).read_bytes()).decode() + '\nEOF\n'
        for name in scripts)
    upload += ''.join(
        f'install -m 755 "$tmp/{name}" /opt/remote-claude/{name}\n'
        for name in scripts)
    remote('set -eu\n' + upload)
    name = shlex.quote(args.name)
    host = shlex.quote(args.host)
    resume = ' --resume' if args.resume else ''
    commands = [f'python3 -I /opt/remote-claude/add-account.py --host {host}{resume} {name}',
                'systemctl start remote-claude-auth-sync.service']
    if args.resources:
        commands.append(f'python3 -I /opt/remote-claude/configure-resources.py {name}')
    commands.append(f'runuser -u {name} -- sh -c \'test -s "$HOME/.claude/.credentials.json"\'')
    remote('set -e; ' + '; '.join(commands))
    remote_path = f'/root/remote-claude-invitations/remote-claude-{args.name}.json'
    invitation = remote(f'cat {shlex.quote(remote_path)}', capture_output=True).stdout
    out = ROOT / 'private' / f'remote-claude-{args.name}.json'
    out.parent.mkdir(mode=0o700, exist_ok=True)
    fd = os.open(out, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'wb') as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(invitation)
    print(f'Invitation: {out}')
    print('Send it to the user privately together with the client archive.')


if __name__ == '__main__':
    main()
