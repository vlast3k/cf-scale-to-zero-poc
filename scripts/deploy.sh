#!/usr/bin/env bash
# Deploy the full scale-to-zero stack: broker + wake-proxy + target app.
#
# Prerequisites:
#   - cf logged in with admin access
#   - Go installed for cross-compilation
#   - A UAA client with cloud_controller.admin scope
#
# Usage: ./deploy.sh

set -euo pipefail

DOMAIN="${DOMAIN:?Set DOMAIN (e.g. cfapps.example.com)}"
CF_API="${CF_API:?Set CF_API (e.g. https://api.cf.example.com)}"
CF_ADMIN_USER="${CF_ADMIN_USER:?Set CF_ADMIN_USER}"
CF_ADMIN_PASS="${CF_ADMIN_PASS:?Set CF_ADMIN_PASS}"
BROKER_PASS="${BROKER_PASS:?Set BROKER_PASS}"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd -P)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd -P)"

echo "=== Scale-to-Zero Deployment ==="
echo "Domain: $DOMAIN"
echo "CF API: $CF_API"

# --- Build ---
echo ""
echo "▶ Building target app..."
cd "$REPO_ROOT/target-app"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o s2z-target .

echo "▶ Building broker..."
cd "$REPO_ROOT/broker"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o s2z-broker .

echo "▶ Building wake-proxy..."
cd "$REPO_ROOT/wake-proxy"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o s2z-wake-proxy .

# --- Push target app ---
echo ""
echo "▶ Pushing target app..."
cd "$REPO_ROOT/target-app"
cf push s2z-target -f manifest.yml -p .
cf map-route s2z-target "$DOMAIN" --hostname s2z-target
cf map-route s2z-target "$DOMAIN" --hostname s2z-target-backend  # secondary route

TARGET_GUID=$(cf app s2z-target --guid)
echo "  s2z-target guid=$TARGET_GUID"

# --- Push broker ---
echo ""
echo "▶ Pushing broker..."
BROKER_ROUTE="s2z-broker.$DOMAIN"
cd "$REPO_ROOT/broker"
cf push s2z-broker -f manifest.yml -p .
cf set-env s2z-broker CF_API "$CF_API"
cf set-env s2z-broker CF_ADMIN_USER "$CF_ADMIN_USER"
cf set-env s2z-broker CF_ADMIN_PASS "$CF_ADMIN_PASS"
cf set-env s2z-broker BROKER_PASS "$BROKER_PASS"
cf set-env s2z-broker BROKER_ROUTE "$BROKER_ROUTE"
cf map-route s2z-broker "$DOMAIN" --hostname s2z-broker
cf restage s2z-broker

# --- Get route GUID for the primary route ---
echo ""
echo "▶ Resolving route GUIDs..."
PRIMARY_ROUTE_GUID=$(cf curl "/v3/routes?hosts=s2z-target&domain_guids=$(cf curl '/v3/domains?names='"$DOMAIN" | python3 -c 'import sys,json; print(json.load(sys.stdin)["resources"][0]["guid"])')" | python3 -c 'import sys,json; print(json.load(sys.stdin)["resources"][0]["guid"])')
SPACE_GUID=$(cf target | grep 'space:' | awk '{print $NF}')
SPACE_GUID=$(cf curl "/v3/spaces?names=$SPACE_GUID" | python3 -c 'import sys,json; print(json.load(sys.stdin)["resources"][0]["guid"])')
echo "  Primary route GUID: $PRIMARY_ROUTE_GUID"
echo "  Space GUID: $SPACE_GUID"

# --- Push wake-proxy ---
echo ""
echo "▶ Pushing wake-proxy..."
BROKER_URL="https://$BROKER_ROUTE"
BROKER_TOKEN="will-be-set-after-bind"  # Replace with actual token from service binding

WAKE_CONFIG=$(cat <<EOF
{
  "s2z-target.$DOMAIN": {
    "app_guid": "$TARGET_GUID",
    "app_route": "s2z-target-backend.$DOMAIN",
    "route_guid": "$PRIMARY_ROUTE_GUID",
    "space_guid": "$SPACE_GUID",
    "app_port": 8080,
    "wake_timeout": 60,
    "health_path": "/health"
  }
}
EOF
)

cd "$REPO_ROOT/wake-proxy"
cf push s2z-wake-proxy -f manifest.yml -p .
cf set-env s2z-wake-proxy BROKER_URL "$BROKER_URL"
cf set-env s2z-wake-proxy BROKER_TOKEN "$BROKER_TOKEN"
cf set-env s2z-wake-proxy CF_API "$CF_API"
cf set-env s2z-wake-proxy CF_ADMIN_USER "$CF_ADMIN_USER"
cf set-env s2z-wake-proxy CF_ADMIN_PASS "$CF_ADMIN_PASS"
cf set-env s2z-wake-proxy WAKE_CONFIG "$WAKE_CONFIG"
cf map-route s2z-wake-proxy "$DOMAIN" --hostname s2z-wake-proxy
cf restage s2z-wake-proxy

echo ""
echo "=== Deployment Complete ==="
echo ""
echo "Components:"
echo "  Broker:      https://s2z-broker.$DOMAIN"
echo "  Wake-Proxy:  https://s2z-wake-proxy.$DOMAIN"
echo "  Target App:  https://s2z-target.$DOMAIN (primary)"
echo "               https://s2z-target-backend.$DOMAIN (secondary)"
echo ""
echo "To test:"
echo "  1. cf stop s2z-target"
echo "  2. Swap primary route to wake-proxy:"
echo "     cf curl /v3/routes/$PRIMARY_ROUTE_GUID/destinations -X PATCH \\"
echo "       -d '{\"destinations\":[{\"app\":{\"guid\":\"$(cf app s2z-wake-proxy --guid)\"},\"port\":8080,\"protocol\":\"http1\"}]}'"
echo "  3. curl https://s2z-target.$DOMAIN  # wake-proxy catches it, wakes app, forwards"
