#!/bin/sh
set -eu

state_dir=${STATE_DIR:-/var/lib/opencode2api}
config_path=${CONFIG_PATH:-$state_dir/config.json}
config_seed_path=${CONFIG_SEED_PATH:-}
# Empty means "defer to config.json". Only an explicitly provided value becomes
# a command-line override, so the file stays authoritative as documented.
listen_address=${LISTEN_ADDRESS:-}
webui_listen_address=${WEBUI_LISTEN_ADDRESS:-}

if [ "$(id -u)" = "0" ]; then
    mkdir -p "$state_dir"
    chown opencode2api:opencode2api "$state_dir"
    exec su-exec opencode2api:opencode2api "$0" "$@"
fi

if [ "$#" -gt 0 ]; then
    exec "$@"
fi

# The example config binds the API to loopback, which is unreachable from
# outside a container through published ports. Rewrite only that exact default
# while seeding; any other address is left exactly as the operator wrote it.
seed_container_listen() {
    if grep -q '"listen"[[:space:]]*:[[:space:]]*"127.0.0.1:8081"' "$1"; then
        sed -i 's|"listen"[[:space:]]*:[[:space:]]*"127.0.0.1:8081"|"listen": "0.0.0.0:8081"|' "$1"
        printf '%s\n' "Seeded $1 with listen 0.0.0.0:8081 so it is reachable through published ports."
    fi
    # Seeds from older releases still carry the split-port 8080 API default;
    # move that exact value to the shared 8081 listener too.
    if grep -q '"listen"[[:space:]]*:[[:space:]]*"127.0.0.1:8080"' "$1"; then
        sed -i 's|"listen"[[:space:]]*:[[:space:]]*"127.0.0.1:8080"|"listen": "0.0.0.0:8081"|' "$1"
        printf '%s\n' "Seeded $1 with listen 0.0.0.0:8081 (legacy 8080 API default moved to the shared port)."
    fi
}

# The example ships with no upstream credential and anonymous access disabled,
# which config validation rejects. Only the untouched example seed opts into
# the anonymous lane so a fresh container boots to the WebUI, where keys or a
# Go/Zen OAuth connection can be configured. Operator-provided seeds are never
# modified beyond the listen rewrite above.
seed_example_anonymous() {
    if grep -q '"anonymous"[[:space:]]*:[[:space:]]*false' "$1"; then
        sed -i 's|"anonymous"[[:space:]]*:[[:space:]]*false|"anonymous": true|' "$1"
        printf '%s\n' "Enabled anonymous access in the seeded $1 so the gateway can start without keys; anonymous traffic may be blocked upstream. Add a Zen/Go key or connect Go/Zen OAuth in the WebUI."
    fi
}

if [ ! -f "$config_path" ]; then
    mkdir -p "$(dirname "$config_path")"
    if [ -n "$config_seed_path" ] && [ -f "$config_seed_path" ]; then
        cp "$config_seed_path" "$config_path"
        seed_container_listen "$config_path"
        printf '%s\n' "Initialized $config_path from $config_seed_path."
    else
        cp /app/config.example.json "$config_path"
        seed_container_listen "$config_path"
        seed_example_anonymous "$config_path"
        printf '%s\n' \
            "config.json not found; created $config_path. Defaults: WebUI login admin / 3178848 and local API key 3178848 — change both before exposing the service."
    fi
fi

set -- /usr/local/bin/opencode2api -config "$config_path"
if [ -n "$listen_address" ]; then
    set -- "$@" -listen "$listen_address"
fi
if [ -n "$webui_listen_address" ]; then
    set -- "$@" -web-listen "$webui_listen_address"
fi
exec "$@"
