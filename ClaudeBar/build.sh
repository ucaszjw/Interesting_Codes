#!/bin/bash
set -euo pipefail

APP_NAME="ClaudeBar"

echo "==> Compiling with Clang…"
clang \
    -o "$APP_NAME" \
    -O2 \
    -fobjc-arc \
    -framework AppKit \
    -framework Foundation \
    Sources/main.m Sources/Dashboard.m -Wno-unused-command-line-argument

echo "==> Creating $APP_NAME.app bundle…"
APP_BUNDLE="$APP_NAME.app"
rm -rf "$APP_BUNDLE"
mkdir -p "$APP_BUNDLE/Contents/MacOS"
mkdir -p "$APP_BUNDLE/Contents/Resources"

cp "$APP_NAME" "$APP_BUNDLE/Contents/MacOS/$APP_NAME"
cp Info.plist "$APP_BUNDLE/Contents/"
rm "$APP_NAME"

echo "==> Ad-hoc signing…"
codesign --force --deep --sign - "$APP_BUNDLE"

echo ""
echo "✅ Done! Bundle: $APP_BUNDLE"
echo "   Run: open \"$APP_BUNDLE\""
echo "   Or: cp -R \"$APP_BUNDLE\" /Applications/"
