#!/bin/sh
# Every shimmed tool's run script is this same one line. The shim reads
# shim.yaml from $DESKBOX_TOOL_DIR, which the desk sets to this folder.
exec tcs-shim
