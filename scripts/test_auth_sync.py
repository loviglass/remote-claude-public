#!/usr/bin/env python3
"""Tests for auth-sync.py: python3 -I scripts/test_auth_sync.py"""
import importlib.util
import json
import os
import pathlib
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('auth_sync', pathlib.Path(__file__).with_name('auth-sync.py'))
sync = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sync)
sync.log = lambda *parts: None

NOW = 1_800_000_000_000
HOUR = 3600 * 1000


def login(name, expires):
    return {'accessToken': 'access-' + name, 'refreshToken': 'refresh-' + name, 'expiresAt': expires,
            'refreshTokenExpiresAt': expires + 1000 * HOUR, 'scopes': ['user:inference', 'user:profile'],
            'subscriptionType': 'max', 'rateLimitTier': 'default_claude_max_20x'}


class Synchronize(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.TemporaryDirectory()
        self.accounts, self.fds = [], []
        for name in ['dev1', 'dev2', 'dev3']:
            path = pathlib.Path(self.root.name, name)
            path.mkdir()
            fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY)
            self.fds.append(fd)
            self.accounts.append(sync.Account(name, os.getuid(), os.getgid(), fd))
        self.requests = []

    def tearDown(self):
        for fd in self.fds:
            os.close(fd)
        self.root.cleanup()

    def write(self, index, document):
        pathlib.Path(self.root.name, self.accounts[index].name, '.credentials.json').write_text(json.dumps(document))
        self.accounts[index].load()

    def read(self, index):
        return json.loads(pathlib.Path(self.root.name, self.accounts[index].name, '.credentials.json').read_text())

    def refresh(self, account, oauth):
        self.requests.append((account.name, oauth['refreshToken']))
        return {'access_token': 'access-new', 'refresh_token': 'refresh-new', 'expires_in': 28800,
                'refresh_token_expires_in': 2592000, 'scope': 'user:inference user:profile'}, None

    def test_fresh_login_is_copied_without_refresh(self):
        self.write(0, {'claudeAiOauth': login('a', NOW + 7 * HOUR)})
        self.write(1, {'claudeAiOauth': login('old', NOW - HOUR), 'mcpOAuth': {'server': 'kept'}})
        self.assertEqual(sync.synchronize(self.accounts, NOW, self.refresh), 0)
        self.assertEqual(self.requests, [])
        self.assertEqual(self.read(1), {'claudeAiOauth': login('a', NOW + 7 * HOUR), 'mcpOAuth': {'server': 'kept'}})
        self.assertEqual(self.read(2), {'claudeAiOauth': login('a', NOW + 7 * HOUR)})

    def test_cleared_file_is_repaired(self):
        # Claude clears both tokens on disk after a rejected refresh.
        self.write(0, {'claudeAiOauth': login('a', NOW + 7 * HOUR)})
        self.write(1, {'claudeAiOauth': {**login('x', 0), 'accessToken': '', 'refreshToken': ''}})
        self.write(2, 'not an object')
        sync.synchronize(self.accounts, NOW, self.refresh)
        self.assertEqual(self.read(1)['claudeAiOauth'], login('a', NOW + 7 * HOUR))
        self.assertEqual(self.read(2)['claudeAiOauth'], login('a', NOW + 7 * HOUR))

    def test_due_login_is_refreshed_once_and_given_to_everyone(self):
        self.write(0, {'claudeAiOauth': login('a', NOW + HOUR)})
        self.write(1, {'claudeAiOauth': login('a', NOW + HOUR)})
        self.write(2, {'claudeAiOauth': login('b', NOW + HOUR // 2)})
        state = {}
        self.assertEqual(sync.synchronize(self.accounts, NOW, self.refresh, state), 0)
        self.assertEqual(self.requests, [('dev1', 'refresh-a')])
        for index in range(3):
            oauth = self.read(index)['claudeAiOauth']
            self.assertEqual(oauth['accessToken'], 'access-new')
            self.assertEqual(oauth['refreshToken'], 'refresh-new')
            self.assertEqual(oauth['expiresAt'], NOW + 28800 * 1000)
            self.assertEqual(oauth['subscriptionType'], 'max')
        sync.synchronize(self.accounts, NOW + 60000, self.refresh, state)
        self.assertEqual(len(self.requests), 1)

    def test_rejected_refresh_token_is_not_retried(self):
        self.write(0, {'claudeAiOauth': login('a', NOW + HOUR)})
        state = {}
        rejected = lambda account, oauth: (self.requests.append(1), (None, 'HTTP 400 invalid_grant'))[1]
        sync.synchronize(self.accounts, NOW, rejected, state)
        sync.synchronize(self.accounts, NOW + 10 * 60000, rejected, state)
        self.assertEqual(len(self.requests), 1)
        self.assertEqual(self.read(1)['claudeAiOauth'], login('a', NOW + HOUR))
        # A new login made under any account is refreshed normally.
        self.write(2, {'claudeAiOauth': login('c', NOW + 90 * 60000)})
        sync.synchronize(self.accounts, NOW + 11 * 60000, self.refresh, state)
        self.assertEqual(self.requests[-1], ('dev3', 'refresh-c'))
        self.assertEqual(self.read(0)['claudeAiOauth']['accessToken'], 'access-new')

    def test_network_failure_waits_before_retry(self):
        self.write(0, {'claudeAiOauth': login('a', NOW + HOUR)})
        state = {}
        failing = lambda account, oauth: (self.requests.append(1), (None, 'URLError: timeout'))[1]
        sync.synchronize(self.accounts, NOW, failing, state)
        sync.synchronize(self.accounts, NOW + 60000, failing, state)
        sync.synchronize(self.accounts, NOW + sync.RETRY_MS, failing, state)
        self.assertEqual(len(self.requests), 2)

    def test_no_login_reports_failure(self):
        self.write(0, {'claudeAiOauth': {'accessToken': '', 'refreshToken': ''}})
        self.assertEqual(sync.synchronize(self.accounts, NOW, self.refresh), 1)


if __name__ == '__main__':
    unittest.main()
