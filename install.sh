#!/bin/sh
set -eu
package_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
install_dir=${REMOTE_CLAUDE_BIN_DIR:-"$HOME/.local/bin"}
mkdir -p "$install_dir"
install -m 755 "$package_dir/remote-claude-rg" "$install_dir/remote-claude-rg.new"
mv "$install_dir/remote-claude-rg.new" "$install_dir/remote-claude-rg"
install -m 755 "$package_dir/remote-claude" "$install_dir/remote-claude.new"
mv "$install_dir/remote-claude.new" "$install_dir/remote-claude"
"$install_dir/remote-claude" version
if [ "$#" -gt 0 ]; then
    "$install_dir/remote-claude" setup "$1"
fi
case ":$PATH:" in
    *":$install_dir:"*) ;;
    *)
        printf '\nAdd this line to your shell configuration, then reopen the terminal:\n'
        printf 'export PATH="$HOME/.local/bin:$PATH"\n'
        ;;
esac
printf '\nStart from your project: %s/remote-claude .\n' "$install_dir"
