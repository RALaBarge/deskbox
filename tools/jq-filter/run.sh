#!/bin/sh
# tcs-shim vendored into this folder (not relying on a host-wide install):
# /usr/local/bin is not reliably bound into the sandbox (see executor.go's
# sandboxArgs comment on /usr/local -> /var/usrlocal on ostree/Bazzite), but
# $DESKBOX_TOOL_DIR is always bound. shim.yaml is still read from there too.
exec "$DESKBOX_TOOL_DIR/tcs-shim"
