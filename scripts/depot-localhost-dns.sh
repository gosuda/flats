#!/bin/sh
# Give a Depot runner the *.localhost wildcard the test suite assumes.
#
# GitHub-hosted runners and developer machines resolve <name>.localhost to
# loopback (systemd-resolved and macOS synthesize those records), and the Go
# tests dial http://<flat>.localhost:<port> directly. Depot runners have no
# systemd-resolved, so those lookups fail with NXDOMAIN. This script starts
# a minimal DNS stub that answers localhost names with loopback addresses
# and forwards everything else to the runner's original nameservers, then
# points /etc/resolv.conf at it.
#
# Run once per job, before any test step, as root:
#
#   sudo scripts/depot-localhost-dns.sh
#
# LOCALHOST_DNS_PORT overrides the listen port for local testing without
# root (the workflow relies on the default 53, the only port resolv.conf
# can name).

set -eu

port=${LOCALHOST_DNS_PORT:-53}
resolv_conf=${LOCALHOST_DNS_RESOLV_CONF:-/etc/resolv.conf}

upstreams=$(grep '^nameserver' "$resolv_conf" 2>/dev/null | awk '{print $2}' |
	grep -v -E '^(127\.|::1$)' || true)
[ -n "$upstreams" ] || {
	echo "depot-localhost-dns: no upstream nameserver in $resolv_conf" >&2
	exit 1
}

# shellcheck disable=SC2086
python3 - $port $upstreams <<'PY' >/tmp/localhost-dns.log 2>&1 &
import socket
import struct
import sys
import threading

port = int(sys.argv[1])
upstreams = sys.argv[2:]


def decode_name(pkt, off):
    labels = []
    jumped = False
    end = off
    for _ in range(32):
        length = pkt[off]
        if length == 0:
            off += 1
            if not jumped:
                end = off
            break
        if length & 0xC0:
            if not jumped:
                end = off + 2
            jumped = True
            off = ((length & 0x3F) << 8) | pkt[off + 1]
            continue
        off += 1
        labels.append(pkt[off:off + length].decode("ascii", "replace"))
        off += length
        if not jumped:
            end = off
    else:
        raise IndexError("name does not terminate")
    return ".".join(labels), end


def is_localhost(name):
    name = name.lower().rstrip(".")
    return name == "localhost" or name.endswith(".localhost")


def respond(query, question_end, rcode, answers):
    qid, flags = struct.unpack(">HH", query[:4])
    qdcount = struct.unpack(">H", query[4:6])[0]
    # Echo only the question section: the query tail may hold an OPT
    # pseudo-record, which belongs to the additional section, not here.
    question = query[12:question_end]
    header = struct.pack(">HHHHHH", qid, 0x8400 | (flags & 0x0100) | rcode,
                         qdcount, len(answers), 0, 0)
    return header + question + b"".join(answers)


def answer_rr(qtype):
    if qtype == 1:
        return struct.pack(">HHHIH4s", 0xC00C, 1, 1, 60, 4,
                           socket.inet_aton("127.0.0.1"))
    if qtype == 28:
        return struct.pack(">HHHIH16s", 0xC00C, 28, 1, 60, 16,
                           socket.inet_pton(socket.AF_INET6, "::1"))
    return None


def forward(query):
    for upstream in upstreams:
        try:
            relay = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
            relay.settimeout(2)
            relay.sendto(query, (upstream, 53))
            return relay.recvfrom(4096)[0]
        except OSError:
            continue
        finally:
            relay.close()
    qid = query[:2]


def answer(data):
    if len(data) < 12:
        return None
    qdcount = struct.unpack(">H", data[4:6])[0]
    if qdcount != 1:
        return forward(data)
    name, end = decode_name(data, 12)
    if end + 4 > len(data):
        return None
    question_end = end + 4
    qtype = struct.unpack(">H", data[end:end + 2])[0]
    if is_localhost(name):
        rr = answer_rr(qtype)
        if rr is None:
            return respond(data, question_end, 0, [])
        return respond(data, question_end, 0, [rr])
    return forward(data)


def handle_udp(sock, data, addr):
    try:
        resp = answer(data)
    except (OSError, struct.error, IndexError, UnicodeError):
        return
    if resp is not None:
        try:
            sock.sendto(resp, addr)
        except OSError:
            pass


def handle_tcp(conn):
    try:
        conn.settimeout(5)
        length = conn.recv(2)
        if len(length) < 2:
            return
        size = struct.unpack(">H", length)[0]
        data = b""
        while len(data) < size:
            chunk = conn.recv(size - len(data))
            if not chunk:
                return
            data += chunk
        resp = answer(data)
        if resp is not None:
            conn.sendall(struct.pack(">H", len(resp)) + resp)
    except (OSError, struct.error, IndexError, UnicodeError):
        return
    finally:
        conn.close()


sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
sock.bind(("127.0.0.1", port))
tcp = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
tcp.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
tcp.bind(("127.0.0.1", port))
tcp.listen(64)


def serve_tcp():
    while True:
        conn, _ = tcp.accept()
        threading.Thread(target=handle_tcp, args=(conn,),
                         daemon=True).start()


threading.Thread(target=serve_tcp, daemon=True).start()
while True:
    data, addr = sock.recvfrom(4096)
    threading.Thread(target=handle_udp, args=(sock, data, addr),
                     daemon=True).start()
PY
dns_pid=$!

cleanup() {
	kill "$dns_pid" 2>/dev/null || true
}
# The stub must outlive this script: later job steps use it. Kill it
# only when it never became healthy.
i=0
while [ "$i" -lt 50 ]; do
	if python3 -c 'import socket,sys
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.settimeout(1)
s.sendto(b"\x12\x34\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00\x05probe\x09localhost\x00\x00\x01\x00\x01", ("127.0.0.1", int(sys.argv[1])))
data, _ = s.recvfrom(512)
sys.exit(0 if data[2] & 0x80 and not data[3] & 0x0F and data[-4:] == b"\x7f\x00\x00\x01" else 1)' \
		"$port" 2>/dev/null; then
		break
	fi
	i=$((i + 1))
	sleep 0.1
done
if [ "$i" -ge 50 ]; then
	echo "depot-localhost-dns: stub on 127.0.0.1:$port never answered" >&2
	sed -n '1,20p' /tmp/localhost-dns.log >&2 || true
	cleanup
	exit 1
fi

# Rewrite resolv.conf only after the stub proves healthy: a rewrite
# without a working stub would break every lookup on the runner.
kept=$(grep -v '^nameserver' "$resolv_conf" 2>/dev/null || true)
if [ -L "$resolv_conf" ]; then
	rm "$resolv_conf"
fi
{
	echo "nameserver 127.0.0.1"
	# shellcheck disable=SC2086
	for ns in $upstreams; do echo "nameserver $ns"; done
	printf '%s\n' "$kept"
} > "$resolv_conf.tmp"
mv "$resolv_conf.tmp" "$resolv_conf"

if [ "$resolv_conf" = /etc/resolv.conf ]; then
	probe_v4=$(python3 -c 'import socket; print(socket.gethostbyname("depot-probe.localhost"))')
	probe_v6=$(python3 -c 'import socket; print(socket.getaddrinfo("depot-probe.localhost", 80, socket.AF_INET6)[0][4][0])')
	[ "$probe_v4" = "127.0.0.1" ] || {
		echo "depot-localhost-dns: A probe returned $probe_v4" >&2
		cleanup
		exit 1
	}
	[ "$probe_v6" = "::1" ] || {
		echo "depot-localhost-dns: AAAA probe returned $probe_v6" >&2
		cleanup
		exit 1
	}
fi
echo "depot-localhost-dns: *.localhost -> 127.0.0.1 / ::1 via 127.0.0.1:$port"
cat "$resolv_conf"
