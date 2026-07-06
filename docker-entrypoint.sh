#!/bin/sh
set -eu

if [ "$(id -u)" = "0" ]; then
	avatar_store="$(printf '%s' "${ROUNDTABLE_AVATAR_STORE:-}" | tr '[:upper:]' '[:lower:]')"
	if [ "$avatar_store" = "local" ]; then
		avatar_dir="${ROUNDTABLE_AVATAR_LOCAL_DIR:-/app/data/avatars}"
		mkdir -p "$avatar_dir"
		chown -R roundtable:roundtable "$avatar_dir"
	fi
	exec setpriv --reuid=10001 --regid=10001 --init-groups "$@"
fi

exec "$@"
