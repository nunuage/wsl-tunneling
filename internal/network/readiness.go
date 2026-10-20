package network

// networkReadinessScript uses only local state and the proxy's TCP DNS listener.
// A PID or configured route alone cannot establish that Windows interop has
// connected the forwarder to gvproxy. Never depend on an Internet endpoint here.
func networkReadinessScript() string {
	return `check_readiness_tools() {
	if ! timeout -s KILL 1 bash -c ':' >/dev/null 2>&1; then
		echo "a timeout command supporting '-s KILL' (GNU coreutils) is required for tunnel readiness checks" >&2
		return 1
	fi
}
forwarder_alive() {
	[ -f "$STATE/vm.pid" ] || return 1
	IFS= read -r GPID < "$STATE/vm.pid" || return 1
	case "$GPID" in ''|*[!0-9]*) return 1 ;; esac
	[ "$GPID" -gt 1 ] || return 1
	kill -0 "$GPID" >/dev/null 2>&1
}
forwarder_running() {
	local command_path
	forwarder_alive || return 1
	IFS= read -r -d '' command_path < "/proc/$GPID/cmdline" || return 1
	[ "$command_path" = "$STATE/gvforwarder" ]
}
network_configured() {
	local link
	link=$(ip -o link show up dev "$IFACE" 2>/dev/null) || return 1
	case "$link" in *" mtu 1500 "*"link/ether 5a:94:ef:e4:0c:ee "*) ;; *) return 1 ;; esac
	ip -4 -o addr show dev "$IFACE" 2>/dev/null | awk -v address="$DEVICE/24" '
		$4 == address { found = 1 }
		END { exit !found }
	' || return 1
	ip -4 route show default | awk -v gateway="$GATEWAY" -v iface="$IFACE" '
		NR == 1 { good = ($1 == "default" && $2 == "via" && $3 == gateway && $4 == "dev" && $5 == iface) }
		END { exit !good }
	' || return 1
	[ "$(cat /etc/resolv.conf 2>/dev/null)" = "$RESOLV_CONTENT" ]
}
gateway_ready() {
	# Bound even a dropped SYN. This opens TCP locally, without a DNS query or
	# an upstream request; timeout kills the probe process group on expiry.
	timeout -s KILL 1 bash -c 'exec 3<>"/dev/tcp/$1/53"' -- "$GATEWAY" >/dev/null 2>&1
}
wait_for_network() {
	local deadline=$((SECONDS + $1)) required=$2 stable=0
	while [ "$SECONDS" -lt "$deadline" ]; do
		forwarder_alive || return 2
		# A freshly forked nohup child may not have exec'd gvforwarder yet.
		if forwarder_running; then
			if ! network_configured; then
				stable=0
				configure_network || true
				write_resolv_conf || true
			elif gateway_ready; then
				[ "$SECONDS" -lt "$deadline" ] || return 1
				stable=$((stable + 1))
				if [ "$stable" -ge "$required" ]; then
					return 0
				fi
			else
				stable=0
			fi
		else
			stable=0
		fi
		sleep 0.2
	done
	return 1
}`
}

// Share the repair operations across launch and post-interop validation so DNS
// suffixes and network settings cannot diverge between the two sessions.
func networkConfigurationScript() string {
	return `RESOLV_CONTENT=$(printf 'nameserver %s\n' "$GATEWAY"; if [ -n "$DNS_SEARCH" ]; then printf 'search %s\n' "$DNS_SEARCH"; fi)
write_resolv_conf() {
	if [ -L /etc/resolv.conf ] || [ "$(cat /etc/resolv.conf 2>/dev/null || true)" != "$RESOLV_CONTENT" ]; then
		rm -f /etc/resolv.conf
		printf '%s\n' "$RESOLV_CONTENT" > /etc/resolv.conf
	fi
	if [ "$(cat /mnt/wsl/resolv.conf 2>/dev/null || true)" != "$RESOLV_CONTENT" ]; then
		cp -f /etc/resolv.conf /mnt/wsl/resolv.conf 2>/dev/null || true
	fi
}
configure_network() {
	if ! ip link show "$IFACE" >/dev/null 2>&1; then
		ip tuntap add dev "$IFACE" mode tap
	fi
	ip link set dev "$IFACE" address 5a:94:ef:e4:0c:ee mtu 1500 up
	ip addr replace "$DEVICE/24" dev "$IFACE"
	ip route replace default via "$GATEWAY" dev "$IFACE"
}`
}

