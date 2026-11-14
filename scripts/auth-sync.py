#!/usr/bin/env python3
"""Keep the shared Claude OAuth login of all managed VPS accounts fresh.

The managed accounts share one login, and its refresh token is single-use.
When several accounts' Claude processes refreshed it themselves, they did so
in the same minute (five minutes before the common expiry) and every loser
was logged out. This service is therefore the only refresher: it renews the
newest login well before Claude's own threshold and writes the result into
every account at once, so Claude only ever finds a fresh token on disk.

Root holds no credentials on this server and has no route through the
tunnel, so the refresh request runs in a child with the UID of the account
whose file holds the login. All user directories/files are opened without
following symlinks. Only the "claudeAiOauth" key of a file is replaced;
other entries (such as MCP logins) stay with their account.
"""
import hashlib
import json
import os
import pwd
import secrets
import stat
import sys
import time
import urllib.error
import urllib.request

# Account names are deliberately unrestricted: open_accounts admits only homes
# containing the managed marker created by scripts/add-account.py.
# Endpoint and client of the pinned Claude Code (2.1.292).
TOKEN_URL = 'https://platform.claude.com/v1/oauth/token'
CLIENT_ID = '9d1c250a-e61b-44d9-88ed-5944d1962f5e'
# Claude refreshes 5 minutes before expiry; renewing 2 hours before keeps
# every Claude process (and its daemon) from ever reaching that point.
REFRESH_AHEAD_MS = 2 * 3600 * 1000
RETRY_MS = 5 * 60 * 1000
STATE = os.environ.get('STATE_DIRECTORY', '/var/lib/remote-claude-auth') + '/state.json'


def log(*parts):
    print(*parts, flush=True)


def fingerprint(token):
    return hashlib.sha256(token.encode()).hexdigest()[:12]


def usable(oauth):
    return isinstance(oauth, dict) and bool(oauth.get('accessToken')) and bool(oauth.get('refreshToken'))


class Account:
    """One managed account; document is None when its file is missing or
    unreadable as JSON and must be created from scratch."""

    def __init__(self, name, uid, gid, directory):
        self.name, self.uid, self.gid, self.directory = name, uid, gid, directory
        self.document = None

    @property
    def oauth(self):
        return (self.document or {}).get('claudeAiOauth')

    def load(self):
        try:
            fd = os.open('.credentials.json', os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK,
                         dir_fd=self.directory)
        except FileNotFoundError:
            self.document = None
            return
        with os.fdopen(fd, 'rb') as stream:
            info = os.fstat(stream.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_uid != self.uid:
                raise ValueError('unexpected credential owner/type')
            raw = stream.read(1024 * 1024)
        try:
            document = json.loads(raw)
        except ValueError:
            document = None
        self.document = document if isinstance(document, dict) else None

    def store(self, oauth):
        document = dict(self.document or {})
        document['claudeAiOauth'] = oauth
        name = '.remote-claude-auth-' + secrets.token_hex(16)
        fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                     0o600, dir_fd=self.directory)
        try:
            if os.geteuid() == 0:
                os.fchown(fd, self.uid, self.gid)
            with os.fdopen(fd, 'w') as stream:
                json.dump(document, stream)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(name, '.credentials.json', src_dir_fd=self.directory, dst_dir_fd=self.directory)
        finally:
            try:
                os.unlink(name, dir_fd=self.directory)
            except FileNotFoundError:
                pass
        self.document = document


def open_accounts(names, descriptors):
    accounts = []
    for name in names:
        try:
            entry = pwd.getpwnam(name)
            home = os.open(entry.pw_dir, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            descriptors.append(home)
            if os.fstat(home).st_uid != entry.pw_uid:
                continue
            marker = os.stat('.remote-claude-managed', dir_fd=home, follow_symlinks=False)
            if not stat.S_ISREG(marker.st_mode):
                continue
            directory = os.open('.claude', os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=home)
            descriptors.append(directory)
            if os.fstat(directory).st_uid != entry.pw_uid:
                continue
            account = Account(name, entry.pw_uid, entry.pw_gid, directory)
            account.load()
            accounts.append(account)
        except (KeyError, OSError, ValueError, TypeError):
            continue
    return accounts


def request_refresh(oauth):
    """Exchange the refresh token. Returns (tokens, None) or (None, error)."""
    body = {'grant_type': 'refresh_token', 'refresh_token': oauth['refreshToken'],
            'client_id': oauth.get('clientId') or CLIENT_ID}
    if oauth.get('scopes'):
        body['scope'] = ' '.join(oauth['scopes'])
    request = urllib.request.Request(TOKEN_URL, json.dumps(body).encode(), method='POST', headers={
        'Content-Type': 'application/json', 'User-Agent': 'remote-claude-auth-sync'})
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.load(response), None
    except urllib.error.HTTPError as e:
        try:
            code = json.load(e).get('error') or ''
        except (ValueError, AttributeError):
            code = ''
        return None, f'HTTP {e.code} {code}'.strip()
    except (OSError, ValueError) as e:
        return None, type(e).__name__ + ': ' + str(e)


def refresh_as(account, oauth):
    """Run request_refresh in a child with the account's UID, so the request
    leaves through the tunnel like every other request of that account."""
    if os.geteuid() != 0:
        return request_refresh(oauth)
    reader, writer = os.pipe()
    pid = os.fork()
    if pid == 0:
        try:
            os.close(reader)
            os.setgroups([])
            os.setgid(account.gid)
            os.setuid(account.uid)
            result = request_refresh(oauth)
            with os.fdopen(writer, 'w') as stream:
                json.dump(result, stream)
        finally:
            os._exit(0)
    os.close(writer)
    with os.fdopen(reader) as stream:
        raw = stream.read()
    os.waitpid(pid, 0)
    try:
        tokens, error = json.loads(raw)
        return tokens, error
    except (ValueError, TypeError):
        return None, 'refresh child failed'


def renewed(oauth, tokens, now):
    """Claude's own layout of a refreshed login (see its token saver)."""
    result = dict(oauth)
    result['accessToken'] = tokens['access_token']
    result['refreshToken'] = tokens.get('refresh_token') or oauth['refreshToken']
    result['expiresAt'] = now + int(tokens['expires_in']) * 1000
    if tokens.get('refresh_token_expires_in'):
        result['refreshTokenExpiresAt'] = now + int(tokens['refresh_token_expires_in']) * 1000
    if tokens.get('scope'):
        result['scopes'] = tokens['scope'].split()
    return result


def read_state():
    try:
        with open(STATE) as stream:
            state = json.load(stream)
        return state if isinstance(state, dict) else {}
    except (OSError, ValueError):
        return {}


def write_state(state):
    try:
        os.makedirs(os.path.dirname(STATE), mode=0o700, exist_ok=True)
        with open(STATE + '.tmp', 'w') as stream:
            json.dump(state, stream)
        os.replace(STATE + '.tmp', STATE)
    except OSError as e:
        log('cannot save state:', e)


def synchronize(accounts, now, refresh=refresh_as, state=None):
    """One pass: renew the newest login when due, then give it to every
    account whose copy is older, missing or cleared by a failed refresh.
    Returns a process exit status."""
    state = {} if state is None else state
    holders = [a for a in accounts if usable(a.oauth)]
    if not holders:
        log('no usable Claude login on the server; log in again under a managed account')
        return 1
    source = max(holders, key=lambda a: int(a.oauth.get('expiresAt') or 0))
    best = source.oauth
    expires = int(best.get('expiresAt') or 0)
    token = fingerprint(best['refreshToken'])
    if expires - now < REFRESH_AHEAD_MS and state.get('dead') != token and now >= state.get('retryAt', 0):
        tokens, error = refresh(source, best)
        try:
            fresh = renewed(best, tokens, now) if tokens else None
        except (KeyError, TypeError, ValueError):
            fresh, error = None, 'malformed token response'
        if fresh:
            # The refresh token just used is now dead: the new login goes to
            # the source account first; if that fails, the loop below still
            # writes it to every other account.
            try:
                source.store(fresh)
            except OSError as e:
                log(f'cannot write login of {source.name}: {e}')
            best = fresh
            state.clear()
            log(f'refreshed login {token} -> {fingerprint(fresh["refreshToken"])} via {source.name}, '
                f'valid for {(fresh["expiresAt"] - now) // 60000} min')
        else:
            if 'invalid_grant' in error:
                state['dead'] = token
            state['retryAt'] = now + RETRY_MS
            left = (expires - now) // 60000
            log(f'refresh of login {token} via {source.name} failed ({error}); token valid for {left} min'
                + ('; log in again under a managed account' if state.get('dead') == token else ''))
    status = 0
    for account in accounts:
        try:
            # Read again right before writing: a login made meanwhile wins.
            account.load()
        except (OSError, ValueError) as e:
            log(f'cannot read login of {account.name}: {e}')
            status = 1
            continue
        current = account.oauth
        if usable(current) and (current['accessToken'] == best['accessToken']
                                or int(current.get('expiresAt') or 0) >= int(best['expiresAt'])):
            continue
        try:
            account.store(best)
            log(f'gave login {fingerprint(best["refreshToken"])} to {account.name}')
        except OSError as e:
            log(f'cannot write login of {account.name}: {e}')
            status = 1
    return status


def main():
    descriptors = []
    try:
        state = read_state()
        before = dict(state)
        names = sorted(entry.pw_name for entry in pwd.getpwall())
        status = synchronize(open_accounts(names, descriptors), int(time.time() * 1000), state=state)
        if state != before:
            write_state(state)
        return status
    finally:
        for fd in descriptors:
            os.close(fd)


if __name__ == '__main__':
    sys.exit(main())
