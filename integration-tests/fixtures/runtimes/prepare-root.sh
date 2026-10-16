#!/bin/sh
set -eu

# Test platform pipeline, not part of Brewlet's provisioner or CLI.
if getent passwd app >/dev/null; then
    userdel app
fi
if getent group app >/dev/null; then
    groupdel app
fi
groupadd --gid 65532 app
useradd --uid 65532 --gid app --home-dir /home/app --create-home --shell /usr/sbin/nologin app
if [ -e /app ]; then
    test -d /app
    test -z "$(find /app -mindepth 1 -maxdepth 1 -print -quit)"
fi
find / -xdev -type f -perm /6000 -exec chmod ug-s '{}' +
