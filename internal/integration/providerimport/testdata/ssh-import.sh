#!/bin/sh
set -eu

trace=${PROVIDER_IMPORT_TRACE:?}
test_binary=${PROVIDER_IMPORT_TEST_BINARY:?}

# The production transport always supplies this exact hardened argv prefix.
[ "$#" -eq 48 ] || exit 64
set -- "$@"
expected='-T -a -x -o BatchMode=yes -o NumberOfPasswordPrompts=0 -o RequestTTY=no -o ForwardAgent=no -o ForwardX11=no -o ForwardX11Trusted=no -o ClearAllForwardings=yes -o Tunnel=no -o ExitOnForwardFailure=yes -o StrictHostKeyChecking=yes -o UpdateHostKeys=no -o PermitLocalCommand=no -o ForkAfterAuthentication=no -o ControlMaster=no -o ControlPath=none -o RemoteCommand=none -o SessionType=default -o StdinNull=no -o ConnectTimeout=10 -o LogLevel=ERROR'
i=1
for want in $expected; do eval "got=\${$i}"; [ "$got" = "$want" ] || exit 64; i=$((i+1)); done
eval "flag=\${$i}"; [ "$flag" = -E ] || exit 64; i=$((i+1))
eval "client_log=\${$i}"
case "$client_log" in /tmp/sub2api-ssh-*|${TMPDIR:-/tmp}/sub2api-ssh-*) ;; *) exit 64;; esac
[ -f "$client_log" ] || exit 64
i=$((i+1)); eval "separator=\${$i}"; [ "$separator" = -- ] || exit 64
i=$((i+1)); eval "alias=\${$i}"; [ "$alias" = edge ] || exit 64
i=$((i+1)); eval "remote=\${$i}"
[ "$remote" = "/usr/local/libexec/sub2api-host stdio" ] || exit 64
exec env SUB2API_PROVIDER_IMPORT_HELPER=1 \
  PROVIDER_IMPORT_ROOT="${PROVIDER_IMPORT_ROOT:?}" \
  PROVIDER_IMPORT_MACHINE_ID="${PROVIDER_IMPORT_MACHINE_ID:?}" \
  PROVIDER_IMPORT_REQUEST_DIGEST="${PROVIDER_IMPORT_REQUEST_DIGEST:?}" \
  "$test_binary" -test.run '^TestProviderImportHelper$'
