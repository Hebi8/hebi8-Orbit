#!/usr/bin/env bash
# Package hebi8/orbit macOS Desktop Client (hebi8-Orbit.app + DMG + ZIP).
# Works both on GitHub Actions (macos-latest) and locally on a Mac.
# Optional Apple Developer signing & notarization activates automatically when
# APPLE_CERTIFICATE_P12 / APPLE_SIGNING_IDENTITY / APPLE_NOTARY_* are provided.
set -euo pipefail

VERSION="${1:-0.2.0-dev}"
COMMIT="${2:-$(git rev-parse --short HEAD 2>/dev/null || echo none)}"
BUILT="${3:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
OUT_DIR="${4:-dist/macos}"

CLEAN_VER="${VERSION#v}"
LDFLAGS="-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.BuildDate=${BUILT}"

mkdir -p "$OUT_DIR"
APP_DIR="${OUT_DIR}/hebi8-Orbit.app"
rm -rf "$APP_DIR"
mkdir -p "${APP_DIR}/Contents/MacOS" "${APP_DIR}/Contents/Resources/bin"

if [[ ! -f "web/dist/index.html" ]] || [[ ! -d "web/dist/assets" ]]; then
  if command -v pnpm >/dev/null 2>&1; then
    echo "==> web/dist missing; building embedded Web workbench first"
    make build-web
  else
    echo "ERROR: web/dist/index.html (and assets) missing; run 'make build-web' before packaging hebi8-Orbit.app" >&2
    exit 1
  fi
fi
if [[ ! -f "web/dist/index.html" ]]; then
  echo "ERROR: web/dist/index.html still missing after build-web; aborting packaging" >&2
  exit 1
fi
if [[ ! -f "assets/icons/AppIcon.icns" ]]; then
  echo "ERROR: assets/icons/AppIcon.icns missing; aborting packaging" >&2
  exit 1
fi

echo "==> Building pure-Go desktop client + bundled runtime (darwin/arm64, version=${VERSION})"
(
  cd desktop
  CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "$LDFLAGS" -o "../${APP_DIR}/Contents/MacOS/h8o-client" .
)
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "$LDFLAGS" -o "${APP_DIR}/Contents/Resources/bin/h8o-hub" ./cmd/h8o-hub
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "$LDFLAGS" -o "${APP_DIR}/Contents/Resources/bin/h8o-runner" ./cmd/h8o-runner
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "$LDFLAGS" -o "${APP_DIR}/Contents/Resources/bin/h8o" ./cmd/h8o
cp "assets/icons/AppIcon.icns" "${APP_DIR}/Contents/Resources/AppIcon.icns"

cat > "${APP_DIR}/Contents/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleDevelopmentRegion</key>
  <string>en</string>
  <key>CFBundleDisplayName</key>
  <string>hebi8/orbit</string>
  <key>CFBundleExecutable</key>
  <string>h8o-client</string>
  <key>CFBundleIconFile</key>
  <string>AppIcon</string>
  <key>CFBundleIdentifier</key>
  <string>io.github.hebi8.h8o-client</string>
  <key>CFBundleInfoDictionaryVersion</key>
  <string>6.0</string>
  <key>CFBundleName</key>
  <string>hebi8/orbit</string>
  <key>CFBundlePackageType</key>
  <string>APPL</string>
  <key>CFBundleShortVersionString</key>
  <string>${CLEAN_VER}</string>
  <key>CFBundleVersion</key>
  <string>${CLEAN_VER}</string>
  <key>LSMinimumSystemVersion</key>
  <string>13.0</string>
  <key>NSHighResolutionCapable</key>
  <true/>
</dict>
</plist>
EOF

# Signing: Developer ID if secrets present, otherwise clean ad-hoc signing
KEYCHAIN=""
TMP_SECRETS=""
cleanup() {
  if [[ -n "$KEYCHAIN" ]]; then
    security delete-keychain "$KEYCHAIN" >/dev/null 2>&1 || true
  fi
  if [[ -n "$TMP_SECRETS" ]]; then
    rm -rf "$TMP_SECRETS" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

SIGN_IDENTITY=""
if [[ -n "${APPLE_CERTIFICATE_P12:-}" ]]; then
  echo "==> Importing Developer ID certificate into temporary keychain"
  TMP_SECRETS="$(mktemp -d)"
  chmod 700 "$TMP_SECRETS"
  KEYCHAIN="${TMP_SECRETS}/h8o-sign.keychain-db"
  KC_PASS="$(openssl rand -hex 24)"
  printf '%s' "$APPLE_CERTIFICATE_P12" | base64 --decode > "${TMP_SECRETS}/cert.p12"
  security create-keychain -p "$KC_PASS" "$KEYCHAIN"
  security set-keychain-settings -lut 21600 "$KEYCHAIN"
  security unlock-keychain -p "$KC_PASS" "$KEYCHAIN"
  security import "${TMP_SECRETS}/cert.p12" -f pkcs12 -k "$KEYCHAIN" -P "${APPLE_CERTIFICATE_PASSWORD:-}" -T /usr/bin/codesign
  rm -f "${TMP_SECRETS}/cert.p12"
  security set-key-partition-list -S apple-tool:,apple:,codesign: -s -k "$KC_PASS" "$KEYCHAIN" >/dev/null 2>&1 || true
  # shellcheck disable=SC2046
  security list-keychains -d user -s "$KEYCHAIN" $(security list-keychains -d user | tr -d '"')

  if [[ -n "${APPLE_SIGNING_IDENTITY:-}" ]]; then
    SIGN_IDENTITY="$APPLE_SIGNING_IDENTITY"
  else
    SIGN_IDENTITY="$(security find-identity -v -p codesigning "$KEYCHAIN" \
      | grep '"Developer ID Application: ' | head -n 1 | awk '{print $2}' || true)"
  fi
  if [[ -z "$SIGN_IDENTITY" ]]; then
    echo "ERROR: Imported certificate does not contain a Developer ID Application identity" >&2
    exit 1
  fi

  echo "==> Signing embedded binaries and hebi8-Orbit.app with Developer ID"
  for bin in \
    "${APP_DIR}/Contents/Resources/bin/h8o-hub" \
    "${APP_DIR}/Contents/Resources/bin/h8o-runner" \
    "${APP_DIR}/Contents/Resources/bin/h8o" \
    "${APP_DIR}/Contents/MacOS/h8o-client"; do
    codesign --force --options runtime --timestamp --keychain "$KEYCHAIN" --sign "$SIGN_IDENTITY" "$bin"
  done
  codesign --force --options runtime --timestamp --keychain "$KEYCHAIN" --sign "$SIGN_IDENTITY" "$APP_DIR"
  codesign --verify --deep --strict "$APP_DIR"
else
  echo "==> Ad-hoc signing hebi8-Orbit.app (no Developer ID secrets configured)"
  codesign --force --deep --sign - "$APP_DIR"
fi

ZIP_PATH="${OUT_DIR}/h8o-client-${VERSION}-darwin-arm64.zip"
DMG_PATH="${OUT_DIR}/h8o-client-${VERSION}-darwin-arm64.dmg"

DMG_STAGE="$(mktemp -d)"
cp -R "$APP_DIR" "${DMG_STAGE}/hebi8-Orbit.app"
ln -s /Applications "${DMG_STAGE}/Applications"
hdiutil create -volname "hebi8-Orbit" -srcfolder "$DMG_STAGE" -ov -format UDZO "$DMG_PATH"
rm -rf "$DMG_STAGE"

if [[ -n "$SIGN_IDENTITY" ]]; then
  codesign --force --timestamp --keychain "$KEYCHAIN" --sign "$SIGN_IDENTITY" "$DMG_PATH"
  if [[ -n "${APPLE_API_KEY_P8_BASE64:-}" && -n "${APPLE_API_KEY_ID:-}" && -n "${APPLE_API_ISSUER_ID:-}" ]]; then
    echo "==> Submitting DMG for Apple notarization (App Store Connect API key)"
    API_KEY_FILE="${TMP_SECRETS}/AuthKey_${APPLE_API_KEY_ID}.p8"
    printf '%s' "$APPLE_API_KEY_P8_BASE64" | base64 --decode > "$API_KEY_FILE"
    chmod 600 "$API_KEY_FILE"
    xcrun notarytool submit "$DMG_PATH" \
      --key "$API_KEY_FILE" \
      --key-id "$APPLE_API_KEY_ID" \
      --issuer "$APPLE_API_ISSUER_ID" \
      --wait
    rm -f "$API_KEY_FILE"
    xcrun stapler staple "$DMG_PATH"
    xcrun stapler staple "$APP_DIR"
    xcrun stapler validate "$DMG_PATH"
    xcrun stapler validate "$APP_DIR"
  elif [[ -n "${APPLE_NOTARY_APPLE_ID:-}" && -n "${APPLE_NOTARY_PASSWORD:-}" && -n "${APPLE_NOTARY_TEAM_ID:-}" ]]; then
    echo "==> Submitting DMG for Apple notarization (Apple ID)"
    xcrun notarytool submit "$DMG_PATH" \
      --apple-id "$APPLE_NOTARY_APPLE_ID" \
      --password "$APPLE_NOTARY_PASSWORD" \
      --team-id "$APPLE_NOTARY_TEAM_ID" \
      --wait
    xcrun stapler staple "$DMG_PATH"
    xcrun stapler staple "$APP_DIR"
    xcrun stapler validate "$DMG_PATH"
    xcrun stapler validate "$APP_DIR"
  fi
fi

# Package ZIP after optional notarization/stapling so both DMG and ZIP carry stapled tickets
ditto -c -k --keepParent "$APP_DIR" "$ZIP_PATH"

echo "==> Packaged:"
ls -lh "$ZIP_PATH" "$DMG_PATH"
