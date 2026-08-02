#!/usr/bin/env bash
set -euo pipefail
if [[ -f runtime/deploy-state.json ]]; then
  bash scripts/application-release.sh
else
  bash scripts/infra-reconcile.sh
fi
