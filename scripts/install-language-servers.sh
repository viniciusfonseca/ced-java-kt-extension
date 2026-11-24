#!/bin/bash
set -euo pipefail

if [[ "$(uname -s)" != Darwin ]]; then
    printf 'This installer requires macOS.\n' >&2
    exit 1
fi

brew_bin="$(command -v brew || true)"
if [[ -z "$brew_bin" ]]; then
    for candidate in /opt/homebrew/bin/brew /usr/local/bin/brew; do
        if [[ -x "$candidate" ]]; then
            brew_bin="$candidate"
            break
        fi
    done
fi
if [[ -z "$brew_bin" ]]; then
    printf 'Install Homebrew from https://brew.sh/ and run this script again.\n' >&2
    exit 1
fi

# JDT LS and Maven use the OpenJDK dependency managed by Homebrew.
"$brew_bin" install --formula jdtls maven

# Stage the official formula before changing any existing cask link.
kotlin_formula=jetbrains/utils/kotlin-lsp
"$brew_bin" install --formula --skip-link "$kotlin_formula"
kotlin_prefix="$("$brew_bin" --prefix "$kotlin_formula")"
"$kotlin_prefix/bin/kotlin-lsp" --version

if "$brew_bin" list --cask kotlin-lsp >/dev/null 2>&1; then
    "$brew_bin" unlink --cask kotlin-lsp
    if ! "$brew_bin" link --formula "$kotlin_formula"; then
        "$brew_bin" link --cask kotlin-lsp
        exit 1
    fi
else
    "$brew_bin" link --formula "$kotlin_formula"
fi

brew_prefix="$("$brew_bin" --prefix)"
for command_name in jdtls kotlin-lsp mvn; do
    executable="$brew_prefix/bin/$command_name"
    if [[ ! -x "$executable" ]]; then
        printf 'Expected executable at %s\n' "$executable" >&2
        exit 1
    fi
    printf 'Available at %s\n' "$executable"
done

case "$brew_prefix" in
    /opt/homebrew|/usr/local)
        printf 'CED can find these tools when opened from Finder.\n'
        ;;
    *)
        printf 'Add %s/bin to PATH before launching CED from that shell.\n' "$brew_prefix"
        ;;
esac
printf 'Open a Java or Kotlin file and run Restart Language Server in CED.\n'
