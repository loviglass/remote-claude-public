#!/usr/bin/env python3
"""Offline tests for generic SSH destinations and enrollment recovery."""
import os
import pathlib
import tempfile
import unittest
from types import SimpleNamespace
from unittest import mock

from test_release import load


add_account = load('add-account')
new_invitation = load('new-invitation')


class SSHDestination(unittest.TestCase):
    def test_hostname_port_and_ipv6_forms(self):
        cases = {
            'server.example.com': ('server.example.com', None),
            'server.example.com:2222': ('server.example.com', '2222'),
            '2001:db8::1': ('2001:db8::1', None),
            '[2001:db8::1]': ('2001:db8::1', None),
            '[2001:db8::1]:2222': ('2001:db8::1', '2222'),
        }
        for value, expected in cases.items():
            with self.subTest(value=value):
                self.assertEqual(new_invitation.ssh_destination(value), expected)

    def test_invalid_ports_and_brackets(self):
        for value in ['host:', 'host:ssh', 'host:0', 'host:65536', '[]', '[::1', '[::1]x']:
            with self.subTest(value=value), self.assertRaises(ValueError):
                new_invitation.ssh_destination(value)


class PendingInvitation(unittest.TestCase):
    def test_requires_owned_pending_state_and_root_invitation(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            home = root / 'home'
            (home / '.ssh').mkdir(parents=True)
            marker = home / '.remote-claude-managed'
            pending = home / '.ssh/enrollment.pending'
            marker.touch()
            pending.touch()
            invitation = root / 'invitation.json'
            invitation.write_text('secret')
            account = SimpleNamespace(pw_dir=str(home), pw_uid=os.getuid())
            original_stat = pathlib.Path.stat

            def root_invitation(path, *args, **kwargs):
                info = original_stat(path, *args, **kwargs)
                uid = 0 if path == invitation else info.st_uid
                return SimpleNamespace(st_mode=info.st_mode, st_uid=uid)

            with mock.patch.object(pathlib.Path, 'stat', root_invitation):
                self.assertTrue(add_account.resumable(account, invitation))
                pending.unlink()
                self.assertFalse(add_account.resumable(account, invitation))
                pending.touch()
                marker.unlink()
                marker.symlink_to(invitation)
                self.assertFalse(add_account.resumable(account, invitation))

            marker.unlink()
            marker.touch()
            self.assertFalse(add_account.resumable(account, invitation),
                             'a non-root-owned invitation must not be resumed')


if __name__ == '__main__':
    unittest.main()
