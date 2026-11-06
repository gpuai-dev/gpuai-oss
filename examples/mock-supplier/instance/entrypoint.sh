#!/bin/sh
# Installs the customer's SSH keys, runs the optional startup script in the
# background (the provisioning bootstrap when gpuctl's FRP tunnel is enabled),
# then runs sshd in the foreground.
set -eu

if [ -n "${SSH_PUBLIC_KEYS_B64:-}" ]; then
  echo "$SSH_PUBLIC_KEYS_B64" | base64 -d > /root/.ssh/authorized_keys
  chmod 600 /root/.ssh/authorized_keys
fi

if [ -n "${STARTUP_SCRIPT_B64:-}" ]; then
  echo "$STARTUP_SCRIPT_B64" | base64 -d > /tmp/startup.sh
  chmod +x /tmp/startup.sh
  (bash /tmp/startup.sh > /var/log/startup.log 2>&1 &) || true
fi

exec /usr/sbin/sshd -D -e
