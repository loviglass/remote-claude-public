# A disposable stand-in for the VPS: sshd, FUSE/SSHFS and Git, nothing else.
FROM ubuntu:24.04
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      openssh-server sshfs fuse3 git ripgrep iproute2 tmux ca-certificates procps \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /run/sshd /opt/remote-claude \
    && useradd -m -s /bin/bash developer \
    && touch /home/developer/.remote-claude-managed \
    && ln -s /usr/bin/rg /opt/remote-claude/rg
COPY sshd_config /etc/ssh/sshd_config.d/e2e.conf
CMD ["/usr/sbin/sshd", "-D", "-e"]
