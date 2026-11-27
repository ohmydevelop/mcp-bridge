#!/bin/sh
set -eu
umask 077

source_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
state="$HOME/.mcp-bridge"
app="$HOME/Library/Application Support/mcp-bridge"
plist="$HOME/Library/LaunchAgents/net.mcp-bridge.agent.plist"
domain="gui/$(id -u)"
label=net.mcp-bridge.agent

# Loaded for foreground debugging, launchd and doctor alike.
load_environment() {
    BRIDGE_MASTER_KEY=$(cat "$state/master.key")
    token=$(cat "$state/http.token")
    BRIDGE_TOKENS=$(printf '{"agent":"%s"}' "$token")
    PATH=$(cat "$app/path")
    export PATH BRIDGE_MASTER_KEY BRIDGE_TOKENS
    BRIDGE_PYTHON_HINT_FILE="$app/python-path"
    export BRIDGE_PYTHON_HINT_FILE
}

refresh_python() {
    candidate=$(unset BRIDGE_PYTHON_HINT_FILE; "$app/mcp-bridge" python-path) || return 1
    tmp=$(mktemp "$app/.python-path.XXXXXX") || return 1
    if printf '%s\n' "$candidate" > "$tmp" && mv -f "$tmp" "$app/python-path"; then
        printf 'Handler Python: %s\n' "$candidate"
    else
        rm -f "$tmp"
        return 1
    fi
}

case "${1:-status}" in
refresh-python)
    refresh_python
    ;;
install)
    mkdir -p "$state" "$app" "$(dirname "$plist")"
    chmod 700 "$state" "$app"
    if [ ! -f "$state/master.key" ]; then
        if [ -n "${BRIDGE_MASTER_KEY:-}" ]; then
            printf '%s\n' "$BRIDGE_MASTER_KEY" | grep -Eq '^[[:xdigit:]]{64}$' || { echo "Invalid BRIDGE_MASTER_KEY" >&2; exit 1; }
            printf '%s\n' "$BRIDGE_MASTER_KEY" > "$state/master.key"
        elif [ -e "$state/bridge.sqlite" ]; then
            echo "Existing database: export its original BRIDGE_MASTER_KEY before installing." >&2
            exit 1
        else
            openssl rand -hex 32 > "$state/master.key"
        fi
    fi
    [ -e "$state/http.token" ] || openssl rand -hex 32 > "$state/http.token"
    chmod 600 "$state/master.key" "$state/http.token"
    if [ "$source_dir" != "$app" ]; then
        install -m 700 "$source_dir/mcp-bridge" "$app/mcp-bridge.next"
        mv -f "$app/mcp-bridge.next" "$app/mcp-bridge"
        install -m 700 "$source_dir/macos-service.sh" "$app/macos-service.sh"
    fi
    printf '%s\n' "$PATH" > "$app/path"
    if ! refresh_python; then
        echo "Python probe failed. Run refresh-python after installing Python." >&2
    fi

    # Build the launch specification with literal arguments, including spaces in home.
    plutil -create xml1 "$plist.next"
    plutil -insert Label -string "$label" "$plist.next"
    plutil -insert ProgramArguments -xml '<array/>' "$plist.next"
    plutil -insert ProgramArguments.0 -string "$app/macos-service.sh" "$plist.next"
    plutil -insert ProgramArguments.1 -string serve "$plist.next"
    plutil -insert RunAtLoad -bool YES "$plist.next"
    plutil -insert KeepAlive -xml '<dict><key>SuccessfulExit</key><false/></dict>' "$plist.next"
    plutil -insert StandardErrorPath -string "$state/daemon.log" "$plist.next"
    mv -f "$plist.next" "$plist"
    if launchctl print "$domain/$label" >/dev/null 2>&1; then launchctl bootout "$domain/$label"; fi
    launchctl bootstrap "$domain" "$plist"
    printf 'Installed %s\nRun "%s/macos-service.sh" status\n' "$app/mcp-bridge" "$app"
    ;;
serve)
    load_environment
    exec "$app/mcp-bridge" serve -data "$state" -listen 127.0.0.1:8088
    ;;
status)
    launchctl print "$domain/$label" >/dev/null || exit 1
    exec "$app/mcp-bridge" doctor -data "$state"
    ;;
uninstall)
    if launchctl print "$domain/$label" >/dev/null 2>&1; then launchctl bootout "$domain/$label"; fi
    rm -f "$plist" "$app/mcp-bridge" "$app/path" "$app/python-path" "$app/macos-service.sh"
    printf 'Service removed. Data and keys retained in %s\n' "$state"
    ;;
*) echo "Usage: $0 install|status|refresh-python|serve|uninstall" >&2; exit 2 ;;
esac
