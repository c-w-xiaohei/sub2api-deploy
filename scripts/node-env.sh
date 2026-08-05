#!/usr/bin/env bash
set -euo pipefail

# Resolve a Node.js runtime for bundle scripts and exec the requested command.
# The bundle must not depend on the ambient PATH of the process that launched
# Pulumi, so resolution happens here at runtime: an explicit NODE_BIN_DIR
# override first, then the ambient PATH, then well-known install directories
# on the target host. Nothing about this host is recorded in Pulumi state.

if [[ $# -eq 0 ]]; then
  printf 'usage: node-env.sh COMMAND [ARGS...]\n' >&2
  exit 2
fi

if [[ -n "${NODE_BIN_DIR:-}" ]]; then
  if [[ ! -x "$NODE_BIN_DIR/node" ]]; then
    printf 'NODE_BIN_DIR does not contain a node executable: %s\n' "$NODE_BIN_DIR" >&2
    exit 1
  fi
  export PATH="$NODE_BIN_DIR${PATH:+:$PATH}"
elif ! command -v node >/dev/null 2>&1; then
  for candidate in "$HOME/servieces"/node-v*/bin /root/servieces/node-v*/bin; do
    if [[ -x "$candidate/node" ]]; then
      export PATH="$candidate${PATH:+:$PATH}"
      break
    fi
  done
fi

if ! command -v node >/dev/null 2>&1; then
  printf 'node executable not found; set NODE_BIN_DIR or install Node into a probed directory\n' >&2
  exit 1
fi

exec "$@"
