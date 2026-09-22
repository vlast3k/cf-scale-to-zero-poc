#!/usr/bin/env bash
# Scale-to-Zero Demo — Quick helpers beyond what `cf` CLI does natively
#
# Prereq: cf login to lod-aws-0723 (or run ./s2z-ctl.sh login)
#
# Usage:
#   ./s2z-ctl.sh login           # cf login using .env credentials
#   ./s2z-ctl.sh invoke <1|2|3>  # Cold-start via coordinator (measures timing)
#   ./s2z-ctl.sh reset           # Stop all + clear billing counters
#   ./s2z-ctl.sh open            # Open coordinator UI in browser
#
# For basic ops, just use cf CLI directly:
#   cf stop s2z-demo-app-1
#   cf start s2z-demo-app-2
#   cf app s2z-demo-app-3

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd -P)"
REPO_ROOT="$(cd "$SCRIPT_DIR" && git rev-parse --show-toplevel 2>/dev/null || cd "$SCRIPT_DIR/../.." && pwd -P)"
ENV_FILE="$REPO_ROOT/.env"

DOMAIN="cfapps.lod-aws-0723.cfrt-sof.sapcloud.io"
COORDINATOR="https://s2z-coordinator.$DOMAIN"

cmd_login() {
  local api pass
  api=$(grep 'cf0723-api' "$ENV_FILE" | cut -d'"' -f2)
  pass=$(grep 'cf0723-pass' "$ENV_FILE" | cut -d'"' -f2)
  cf login -a "$api" -u vladi-admin -p "$pass" --skip-ssl-validation -o SAP -s dev
}

cmd_invoke() {
  local app="${1:-}"
  if [ -z "$app" ]; then echo "Usage: $0 invoke <1|2|3>"; exit 1; fi
  echo "Invoking s2z-demo-app-$app via coordinator (cold-start timing)..."
  curl -sk "$COORDINATOR/api/invoke?endpoint=s2z-demo-app-$app"
  echo ""
}

cmd_reset() {
  echo "Resetting demo..."
  cf stop s2z-demo-app-1
  cf stop s2z-demo-app-2
  cf stop s2z-demo-app-3
  echo ""
  echo "Apps stopped. Use the 🔄 Reset Demo button in the UI to clear billing counters."
}

cmd_open() {
  open "$COORDINATOR"
}

case "${1:-help}" in
  login)   cmd_login ;;
  invoke)  cmd_invoke "${2:-}" ;;
  reset)   cmd_reset ;;
  open)    cmd_open ;;
  *)
    echo "Usage: $0 {login|invoke|reset|open} [app#]"
    echo ""
    echo "  login           cf login using .env credentials"
    echo "  invoke <1|2|3>  Cold-start via coordinator (timing test)"
    echo "  reset           Stop all apps + prompt to clear billing"
    echo "  open            Open coordinator UI in browser"
    echo ""
    echo "For basic ops, just use cf CLI:"
    echo "  cf stop s2z-demo-app-1"
    echo "  cf start s2z-demo-app-2"
    echo "  cf apps"
    ;;
esac
