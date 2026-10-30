#!/bin/sh
# Builds ImpossiBLE's simulator library as a dylib to inject into an app at
# launch, so the app needs no change to talk to libre3sim from the iOS Simulator.
#
#   simulator/build-impossible.sh [out-dir]     (default: simulator/build)
set -eu

# Library 3.1.0, the protocol impossible.go speaks.
repo=https://github.com/mickeyl/ImpossiBLE
commit=58fe38d

here=$(cd "$(dirname "$0")" && pwd)
out=${1:-$here/build}
src=$out/ImpossiBLE
mkdir -p "$out"
if [ ! -d "$src/.git" ]; then
	git clone --quiet "$repo" "$src"
fi
git -C "$src" fetch --quiet origin
git -C "$src" checkout --quiet --force "$commit"
# Fixes proposed upstream, where ImpossiBLE behaves unlike CoreBluetooth in
# ways Loop's device drivers trip on: state callbacks off the central's
# queue (mickeyl/ImpossiBLE#2, #3), a scan filter written as a 128-bit UUID
# missing a 16-bit advert (#4), subscriptions and services surviving a
# disconnect (#5), and retrievePeripheralsWithIdentifiers: forgetting
# peripherals on relaunch (#6). And the socket path from IMPOSSIBLE_SOCKET, so
# several simulators can each reach their own provider.
for patch in "$here"/impossible-patches/*.patch; do
	git -C "$src" apply "$patch"
done

xcrun -sdk iphonesimulator clang -dynamiclib -fobjc-arc -fmodules \
	-target arm64-apple-ios15.0-simulator \
	-I "$src/Sources/ImpossiBLE/include" -I "$src/Sources/ImpossiBLE" \
	-framework Foundation -framework CoreBluetooth \
	-install_name @rpath/libImpossiBLE.dylib \
	"$src"/Sources/ImpossiBLE/*.m -o "$out/libImpossiBLE.dylib"
echo "$out/libImpossiBLE.dylib"
