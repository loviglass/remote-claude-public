#!/usr/bin/env python3
"""Interactive end-to-end check: a real terminal, a dropped connection and the
reattach to the same Claude process.

usage: interactive.py CLIENT_BINARY CONFIG_HOME PROJECT CONTAINER
"""
import os, pty, re, select, signal, struct, subprocess, sys, time, fcntl, termios

client, config_home, project, container = sys.argv[1:5]
env = dict(os.environ, XDG_CONFIG_HOME=config_home, TERM="xterm-256color", COLORTERM="truecolor")
pid, fd = pty.fork()
if pid == 0:
    if os.environ.get("E2E_STDERR"):
        f = os.open(os.environ["E2E_STDERR"], os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        os.dup2(f, 2)
    os.execvpe(client, [client, project], env)

buf = b""
def expect(pattern, timeout=30):
    global buf
    deadline = time.time() + timeout
    rx = re.compile(pattern.encode())
    while time.time() < deadline:
        m = rx.search(buf)
        if m:
            buf = buf[m.end():]
            return m
        r, _, _ = select.select([fd], [], [], 0.2)
        if r:
            try:
                data = os.read(fd, 65536)
            except OSError:
                break
            buf += data
    sys.exit(f"FAIL: expected {pattern!r}; recent output: {buf[-600:]!r}")

def send(line):
    os.write(fd, line.encode() + b"\r")

def resize(rows, cols):
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
    os.kill(pid, signal.SIGWINCH)

results = {}
t = time.time()
m = expect(r"FAKE-CLAUDE READY pid=(\d+)")
claude_pid = m.group(1)
results["start_to_claude_s"] = round(time.time() - t, 2)

t = time.time(); send("hello"); expect(r"echo:hello")
results["keystroke_echo_ms"] = round((time.time() - t) * 1000)

t = time.time(); send("run:echo from-terminal > via-command.txt && echo done-$((40+2))"); expect(r"done-42")
results["command_ms"] = round((time.time() - t) * 1000)
assert open(os.path.join(project, "via-command.txt")).read() == "from-terminal\n"

resize(40, 132); time.sleep(0.3); send("size")
expect(r"size:40 132")
results["resize"] = "ok"

# Drop every SSH connection on the server side, as a network failure would.
t = time.time()
subprocess.run(["docker", "exec", container, "pkill", "-f", "sshd: developer"], check=False)
try:
    expect(r"reattached to the running Claude session", timeout=60)
except SystemExit as e:
    try:
        print("child status", os.waitpid(pid, os.WNOHANG))
    except Exception as x:
        print("wait", x)
    raise
results["reattach_s"] = round(time.time() - t, 2)
send("after-drop"); expect(r"echo:after-drop")
send("run:echo $PPID > /dev/null; echo still-$((1+1))"); expect(r"still-2")

# The same Claude process kept running across the drop.
alive = subprocess.run(["docker", "exec", container, "kill", "-0", claude_pid]).returncode == 0
results["same_claude_process"] = alive
send("quit")
_, status = os.waitpid(pid, 0)
results["client_exit"] = os.waitstatus_to_exitcode(status)
print(results)
if not alive or results["client_exit"] != 0:
    sys.exit("FAIL")
