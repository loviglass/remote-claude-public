package main

import "testing"

func TestFuseConnectionID(t *testing.T) {
	info := []byte(`22 1 252:1 / / rw,relatime shared:1 - ext4 /dev/vda1 rw
40 22 0:348 / /home/u/.local/share/remote-claude/projects/a/worktree rw,nosuid,nodev,relatime shared:200 - fuse.sshfs bridge:/ rw,user_id=2003,group_id=2003
41 22 0:52 / /home/u/.local/share/remote-claude/projects/b/worktree rw - tmpfs tmpfs rw
42 22 0:99 / /home/u/with\040space rw - fuse.sshfs bridge:/ rw
43 22 0:351 / /home/u/.local/share/remote-claude/projects/c/worktree rw - fuse.sshfs bridge:/ rw
44 22 0:352 / /home/u/.local/share/remote-claude/projects/c/worktree rw - fuse.sshfs bridge:/ rw
`)
	cases := []struct {
		dir, id string
		ok      bool
	}{
		{"/home/u/.local/share/remote-claude/projects/a/worktree", "348", true},
		{"/home/u/.local/share/remote-claude/projects/b/worktree", "", false},
		{"/home/u/with space", "99", true},
		{"/home/u/.local/share/remote-claude/projects/c/worktree", "352", true},
		{"/home/u/missing", "", false},
	}
	for _, c := range cases {
		id, ok := fuseConnectionID(info, c.dir)
		if id != c.id || ok != c.ok {
			t.Errorf("%s: got %q %v, want %q %v", c.dir, id, ok, c.id, c.ok)
		}
	}
	if id, _ := fuseConnectionID([]byte("1 0 1:2 / /m rw - fuse x rw\n"), "/m"); id != "1048578" {
		t.Errorf("non-zero major: got %s", id)
	}
}
