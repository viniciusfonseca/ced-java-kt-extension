#!/bin/sh
set -eu

ced_source=$1
ced_build=$2
package=$3
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
mkdir "$temporary/workspace"

# Link the real broker and CUI from a CED build with extensions enabled.
case "$(uname -s)" in
    Darwin) set -- -framework Cocoa -framework QuartzCore -framework OpenGL ;;
    Linux) set -- $(pkg-config --libs x11 xext) -ldl ;;
    *) exit 1 ;;
esac
"${CC:-cc}" -std=c99 -D_DARWIN_C_SOURCE -D_POSIX_C_SOURCE=200809L -D_XOPEN_SOURCE=700 \
    -I"$ced_source/src" -I"$ced_source/third_party/cjson" -I"$ced_source/third_party/CUI/include" -I"$ced_source/third_party/CUI/src" \
    tests/ced_host.c "$ced_source/src/editor_bridge.c" "$ced_build/libced_extensions.a" \
    "$ced_build/third_party/CUI/libcui.a" "$ced_build/libced_core.a" "$ced_build/libced_json.a" \
    "$ced_build/libced_vterm.a" -pthread -lm "$@" -o "$temporary/ced-java-kotlin-integration"
"$temporary/ced-java-kotlin-integration" "$package" "$temporary/workspace"
