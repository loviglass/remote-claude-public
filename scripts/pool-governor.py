#!/usr/bin/env python3
"""Keep server-mode jobs from starving the rest of the VPS. Run as root.

CPU and I/O need no supervision: the pool's low weights give other workloads
priority whenever they compete and leave idle capacity to the jobs. Memory has
no such weight, so this loop moves the pool's soft limit with the host: jobs
may fill whatever is free beyond a reserve, are throttled as other workloads
grow, and the heaviest developer's jobs are stopped if the host still runs
short. The soft limit is divided between developers who have a job right now.
The static MemoryMax of the pool holds even when this service is down.
"""
import pathlib
import subprocess
import sys
import time

POOL = pathlib.Path('/sys/fs/cgroup/remoteclaude.slice')
INTERVAL = 5
RESERVE_SHARE = 0.10   # of host memory kept available for everything else
FLOOR = 1 << 30        # the pool is never squeezed below this
EVICT_PAUSE = 30       # seconds for memory to settle after stopping jobs


def plan(total, available, used, ceiling, active):
    """Return the pool soft limit, each active developer's share of it and
    whether the host is short enough to stop jobs."""
    reserve = max(total * RESERVE_SHARE, FLOOR)
    high = int(max(FLOOR, min(ceiling, used + available - reserve)))
    return high, high // max(active, 1), available < reserve / 2 and used > FLOOR


def meminfo():
    fields = {}
    for line in pathlib.Path('/proc/meminfo').read_text().splitlines():
        key, value = line.split(':')
        fields[key] = int(value.split()[0]) * 1024
    return fields['MemTotal'], fields['MemAvailable']


def number(path, default):
    try:
        text = path.read_text().strip()
    except OSError:
        return default
    return default if text == 'max' else int(text)


def write(path, value):
    try:
        path.write_text(str(value))
    except OSError as error:
        print(f'{path}: {error}', file=sys.stderr, flush=True)


def developers():
    """Map each developer slice to (processes, memory). A slice holding only
    its idle tmux server has no job."""
    found = {}
    for directory in POOL.glob('remoteclaude-u*.slice'):
        processes = sum(len(p.read_text().split()) for p in directory.glob('*/cgroup.procs'))
        found[directory] = (processes, number(directory / 'memory.current', 0))
    return found


def step(last_eviction):
    total, available = meminfo()
    found = developers()
    active = [d for d, (processes, _) in found.items() if processes > 1]
    high, share, short = plan(total, available, number(POOL / 'memory.current', 0),
                              number(POOL / 'memory.max', total // 2), len(active))
    write(POOL / 'memory.high', high)
    for directory in found:
        write(directory / 'memory.high', share if directory in active else 'max')
    if short and active and time.monotonic() - last_eviction > EVICT_PAUSE:
        heaviest = max(active, key=lambda d: found[d][1])
        uid = heaviest.name[len('remoteclaude-u'):-len('.slice')]
        print(f'host has {available >> 20} MiB available; stopping server jobs of UID {uid}',
              file=sys.stderr, flush=True)
        # Containers belong to Docker and would outlive their terminals.
        running = subprocess.run(['docker', 'ps', '--quiet', '--filter', f'label=remote-claude.uid={uid}'],
                                 capture_output=True, text=True).stdout.split()
        if running:
            subprocess.run(['docker', 'kill', *running])
        subprocess.run(['systemctl', 'restart', f'remote-claude-jobs@{uid}.service'])
        return time.monotonic()
    return last_eviction


if __name__ == '__main__':
    last_eviction = -EVICT_PAUSE
    while True:
        if POOL.is_dir():
            try:
                last_eviction = step(last_eviction)
            except (OSError, ValueError, KeyError) as error:
                print(error, file=sys.stderr, flush=True)
        time.sleep(INTERVAL)
