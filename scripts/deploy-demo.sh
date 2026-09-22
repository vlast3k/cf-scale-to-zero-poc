#!/usr/bin/env bash
# Deploy Scale-to-Zero Demo (3 target apps + 1 coordinator)
# Run from OCI container with BOSH/CF access
#
# Prerequisites:
#   - cf logged in to the target landscape
#   - UAA client 'scale-to-zero-proxy' exists with cloud_controller.admin
#   - Go available for cross-compilation
#
# Usage: ./deploy-demo.sh

set -euo pipefail

# --- Configuration ---
DOMAIN="${DOMAIN:-cfapps.lod-aws-0723.cfrt-sof.sapcloud.io}"
CF_API="${CF_API:-https://api.cf.lod-aws-0723.cfrt-sof.sapcloud.io}"
CF_CLIENT_ID="${CF_CLIENT_ID:-scale-to-zero-proxy}"
CF_CLIENT_SECRET="${CF_CLIENT_SECRET:-s2z-proxy-secret-2026}"

APPS=("s2z-demo-app-1" "s2z-demo-app-2" "s2z-demo-app-3")
COORDINATOR="s2z-coordinator"

echo "=== Scale-to-Zero Demo Deployment ==="
echo "Domain: $DOMAIN"
echo "CF API: $CF_API"
echo ""

# --- Step 1: Build demo apps ---
echo "▶ Building demo apps..."
cd "$(dirname "$0")/demo-apps"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o demo-app .
echo "  ✓ demo-app binary built"

# --- Step 2: Push demo apps ---
echo ""
echo "▶ Pushing demo apps..."
for APP in "${APPS[@]}"; do
    echo "  Pushing $APP..."
    cf push "$APP" -f manifest.yml -p . --var app_name="$APP" 2>&1 | tail -3
    cf map-route "$APP" "$DOMAIN" --hostname "$APP"
    echo "  ✓ $APP deployed at https://$APP.$DOMAIN"
done

# --- Step 3: Get app GUIDs ---
echo ""
echo "▶ Resolving app GUIDs..."
declare -A GUIDS
for APP in "${APPS[@]}"; do
    GUID=$(cf app "$APP" --guid)
    GUIDS[$APP]="$GUID"
    echo "  $APP = $GUID"
done

# --- Step 4: Build coordinator ---
echo ""
echo "▶ Building coordinator..."
cd "$(dirname "$0")/demo-coordinator"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o coordinator .
echo "  ✓ coordinator binary built"

# --- Step 5: Generate ENDPOINTS JSON ---
ENDPOINTS="["
FIRST=true
for APP in "${APPS[@]}"; do
    if [ "$FIRST" = true ]; then FIRST=false; else ENDPOINTS+=","; fi
    ENDPOINTS+="{\"name\":\"$APP\",\"guid\":\"${GUIDS[$APP]}\",\"backend_url\":\"https://$APP.$DOMAIN\"}"
done
ENDPOINTS+="]"
echo "  ENDPOINTS=$ENDPOINTS"

# --- Step 6: Push coordinator ---
echo ""
echo "▶ Pushing coordinator..."
cf push "$COORDINATOR" -f manifest.yml -p .
cf set-env "$COORDINATOR" CF_API "$CF_API"
cf set-env "$COORDINATOR" CF_CLIENT_ID "$CF_CLIENT_ID"
cf set-env "$COORDINATOR" CF_CLIENT_SECRET "$CF_CLIENT_SECRET"
cf set-env "$COORDINATOR" ENDPOINTS "$ENDPOINTS"
cf map-route "$COORDINATOR" "$DOMAIN" --hostname "$COORDINATOR"
cf restage "$COORDINATOR"

echo ""
echo "=== Deployment Complete ==="
echo ""
echo "Coordinator: https://$COORDINATOR.$DOMAIN"
echo ""
echo "Target apps:"
for APP in "${APPS[@]}"; do
    echo "  - https://$APP.$DOMAIN (guid: ${GUIDS[$APP]})"
done
echo ""
echo "To stop target apps for demo:"
for APP in "${APPS[@]}"; do
    echo "  cf stop $APP"
done
