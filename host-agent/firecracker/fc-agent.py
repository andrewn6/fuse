#!/usr/bin/env python3
"""Fuse Firecracker host agent.

Speaks the contract documented in ~/fc/README.md (POST /v1/vm, upload/exec,
snapshots, ...). Fuse's `firecracker` provider talks to this agent. One
firecracker process per VM; state persisted under ~/fc/agent-state/vms/<vm_id>/.

Auth: Authorization: Bearer $FC_AGENT_TOKEN
Transport for guest ops (upload/exec/start-agent): SSH to root@<guest_ip>.
"""
from __future__ import annotations

import base64
import fcntl
import copy
import hashlib
import hmac
import ipaddress
import http.client
import json
import os
import platform
import pty
import re
import selectors
import shlex
import shutil
import signal
import socket
import struct
import subprocess
import sys
import termios
import threading
import time
import traceback
import uuid
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

FC_DIR = Path(os.environ.get("FC_DIR", "/home/ubuntu/fc"))
STATE_DIR = FC_DIR / "agent-state"
VMS_DIR = STATE_DIR / "vms"
KERNEL = FC_DIR / "vmlinux.bin"
BASE_ROOTFS = Path(os.environ.get("BASE_ROOTFS", str(FC_DIR / "rootfs-fused.ext4")))
# Named rootfs images for bring-your-own-image (see internal/fusefile's
# ResourceSpec.Image): <IMAGES_DIR>/<name>.ext4. There is no OCI pull here —
# an operator bakes and places a rootfs there (e.g. with fc-bake-rootfs.sh)
# before a Fusefile can reference it by name.
IMAGES_DIR = Path(os.environ.get("IMAGES_DIR", str(FC_DIR / "images")))
# Snapshot artifacts live here, NOT under the vm dir, so a snapshot outlives the
# vm that produced it. destroy_vm rm -rf's the whole vm dir, so anything stored
# there dies with its builder, which makes a build artifact (fuse build)
# unusable by definition. Legacy snapshots under a vm dir stay readable; see
# snapshot_rootfs.
SNAPSHOTS_DIR = Path(os.environ.get("SNAPSHOTS_DIR", str(STATE_DIR / "snapshots")))
SSH_KEY = FC_DIR / "ubuntu.id_rsa"
TOKEN = os.environ.get("FC_AGENT_TOKEN")
PORT = int(os.environ.get("FC_AGENT_PORT", "8090"))
# Request body ceilings. Ordinary control-plane calls are a few short fields;
# uploads carry a base64 file and get their own, larger, ceiling. Raise
# FC_AGENT_MAX_UPLOAD_BYTES on a host that needs to push bigger payloads into
# guests. Header and request-line sizes are already bounded by http.server
# (64 KiB per line, 100 headers).
MAX_BODY_BYTES = int(os.environ.get("FC_AGENT_MAX_BODY_BYTES", str(1 << 20)))       # 1 MiB
MAX_UPLOAD_BYTES = int(os.environ.get("FC_AGENT_MAX_UPLOAD_BYTES", str(64 << 20)))  # 64 MiB
# Socket timeout for an HTTP conversation. Bounds a client that opens a
# connection and then dribbles (or never finishes) its request. Attach clears
# it for its own socket, since an interactive session is idle by design.
REQUEST_TIMEOUT = float(os.environ.get("FC_AGENT_REQUEST_TIMEOUT", "60"))
# Peer-to-peer artifact transfer bounds. An artifact pull is the one path where
# this agent reads an unbounded stream from a machine that is not the
# orchestrator, so every dimension of it has to be capped: total bytes (a
# hostile or broken peer must not be able to fill the disk), per-socket-op time
# (a peer that stops sending must not pin a thread), and total wall clock (a
# peer that dribbles a byte at a time stays under the per-op timeout forever).
MAX_ARTIFACT_BYTES = int(os.environ.get("FC_AGENT_MAX_ARTIFACT_BYTES", str(64 << 30)))          # 64 GiB
ARTIFACT_IO_TIMEOUT = float(os.environ.get("FC_AGENT_ARTIFACT_IO_TIMEOUT", "60"))               # per socket op
ARTIFACT_TRANSFER_TIMEOUT = float(os.environ.get("FC_AGENT_ARTIFACT_TRANSFER_TIMEOUT", "1800")) # whole transfer
ARTIFACT_CHUNK_BYTES = 1 << 20  # 1 MiB
# Partial pulls stage here, deliberately NOT under SNAPSHOTS_DIR: nothing that
# has not been verified against its digest may ever be visible in the store,
# not even briefly. Default is a sibling of the store so the commit stays a
# same-filesystem rename; override both together if you move one.
ARTIFACT_TMP_DIR = Path(
    os.environ.get("FC_AGENT_ARTIFACT_TMP_DIR", str(SNAPSHOTS_DIR.parent / "artifact-pull-tmp"))
)
FC_BIN = os.environ.get("FC_BIN", "/usr/local/bin/firecracker")
# Port the in-guest agent listens on; per-VM host ports DNAT to this.
FUSED_PORT = int(os.environ.get("FUSED_PORT", "9550"))
HOST_PORT_BASE = int(os.environ.get("FUSE_HOST_PORT_BASE", "19550"))
# Public host resolution. The agent hands the orchestrator a bare "host:port"
# authority for each VM, so a malformed host silently produces environments
# nobody can reach. Prefer IPv4 (consumers of the url treat it as an opaque
# host:port), bracket IPv6 when that is all the host has, and fail loudly on a
# value that is neither an IP nor a plausible hostname.
_HOSTNAME_RE = re.compile(
    r"^(?=.{1,253}$)[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?"
    r"(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$"
)


def host_authority(host: str, port: int | str) -> str:
    """Compose a host:port authority, bracketing host when it is IPv6."""
    try:
        if isinstance(ipaddress.ip_address(host), ipaddress.IPv6Address):
            return f"[{host}]:{port}"
    except ValueError:
        pass
    return f"{host}:{port}"


def _probe(cmd: list[str]) -> str:
    try:
        return subprocess.run(cmd, capture_output=True, text=True, timeout=10).stdout.strip()
    except (OSError, subprocess.SubprocessError):
        return ""


def probe_public_host() -> str:
    """Best-effort public address of this host, IPv4 first, IPv6 as fallback."""
    for cmd in (
        ["curl", "-4", "-fsS", "ifconfig.me"],
        ["bash", "-lc", "hostname -I | tr ' ' '\\n' | awk '/:/ {next} NF {print; exit}'"],
        ["curl", "-6", "-fsS", "ifconfig.me"],
        ["bash", "-lc", "hostname -I | awk '{print $1}'"],
    ):
        out = _probe(cmd)
        if not out:
            continue
        try:
            ipaddress.ip_address(out)
        except ValueError:
            continue
        return out
    return ""


def resolve_public_host() -> str:
    """PUBLIC_HOST if set (IP or hostname), else a probed IP. Never garbage."""
    override = os.environ.get("PUBLIC_HOST", "").strip()
    if override:
        try:
            ipaddress.ip_address(override)
            return override
        except ValueError:
            pass
        if _HOSTNAME_RE.match(override):
            return override
        raise SystemExit(
            f"PUBLIC_HOST is neither an IP address nor a hostname: {override!r}"
        )
    host = probe_public_host()
    if not host:
        raise SystemExit(
            "could not resolve a public IP for this host; "
            "set PUBLIC_HOST=<ip> in the agent env"
        )
    return host


PUBLIC_HOST = resolve_public_host()

if not TOKEN:
    print("FC_AGENT_TOKEN must be set", file=sys.stderr)
    sys.exit(1)

VMS_DIR.mkdir(parents=True, exist_ok=True)
SNAPSHOTS_DIR.mkdir(parents=True, exist_ok=True)
ARTIFACT_TMP_DIR.mkdir(parents=True, exist_ok=True)
SSH_CONTROL_DIR = STATE_DIR / "ssh-control"
SSH_CONTROL_DIR.mkdir(parents=True, exist_ok=True)

_vm_locks: dict[str, threading.Lock] = {}
_locks_guard = threading.Lock()
_create_lock = threading.Lock()


def vm_lock(vm_id: str) -> threading.Lock:
    with _locks_guard:
        if vm_id not in _vm_locks:
            _vm_locks[vm_id] = threading.Lock()
        return _vm_locks[vm_id]


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


def sanitize_name(name: str) -> str:
    s = re.sub(r"[^a-z0-9-]+", "-", name.lower()).strip("-")
    return s or "vm"


def run(cmd: list[str], check: bool = True, input_bytes: bytes | None = None) -> subprocess.CompletedProcess:
    return subprocess.run(cmd, capture_output=True, check=check, input=input_bytes)


def sudo(cmd: list[str], check: bool = True) -> subprocess.CompletedProcess:
    return run(["sudo", "-n"] + cmd, check=check)


# -- Firecracker HTTP client over unix socket ---------------------------------

class UnixHTTPConnection(http.client.HTTPConnection):
    def __init__(self, path: str):
        super().__init__("localhost")
        self._p = path

    def connect(self):
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.connect(self._p)
        self.sock = s


def fc_api(sock_path: str, method: str, path: str, body: dict | None = None, timeout: float = 5.0) -> tuple[int, bytes]:
    # Uses sudo-owned socket; simplest is curl to avoid permission issues.
    # method/path are always literals from call sites (see start_firecracker's
    # `steps`), never guest- or request-derived; validate anyway so this stays
    # true even if a future caller forwards something less trusted.
    if method not in ("GET", "PUT", "POST", "DELETE", "PATCH"):
        raise ValueError(f"invalid firecracker API method: {method!r}")
    if not re.fullmatch(r"/[A-Za-z0-9/_-]*", path):
        raise ValueError(f"invalid firecracker API path: {path!r}")
    data = json.dumps(body) if body is not None else None
    args = ["sudo", "-n", "curl", "-sS", "--unix-socket", sock_path,
            "-X", method, f"http://localhost{path}",
            "-H", "Content-Type: application/json",
            "-w", "\n__HTTP_CODE__%{http_code}"]
    if data is not None:
        args += ["-d", data]
    try:
        cp = subprocess.run(args, capture_output=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        return 599, b"timeout"
    out = cp.stdout
    marker = b"\n__HTTP_CODE__"
    idx = out.rfind(marker)
    if idx < 0:
        return 599, out + cp.stderr
    code = int(out[idx + len(marker):].strip() or b"0")
    return code, out[:idx]


# -- Networking ---------------------------------------------------------------

def pick_index() -> int:
    used = set()
    for d in VMS_DIR.iterdir() if VMS_DIR.exists() else []:
        meta_p = d / "meta.json"
        if meta_p.exists():
            try:
                used.add(json.loads(meta_p.read_text())["index"])
            except Exception:
                pass
    for i in range(1, 250):
        if i not in used:
            return i
    raise RuntimeError("no free VM index")


def tap_name(idx: int) -> str:
    return f"fcv{idx}"


def setup_tap(idx: int, iface: str) -> tuple[str, str, str]:
    tap = tap_name(idx)
    host_ip = f"10.200.{idx}.1"
    guest_ip = f"10.200.{idx}.2"
    sudo(["ip", "link", "del", tap], check=False)
    sudo(["ip", "tuntap", "add", tap, "mode", "tap"])
    sudo(["ip", "addr", "add", f"{host_ip}/30", "dev", tap])
    sudo(["ip", "link", "set", tap, "up"])
    sudo(["sysctl", "-w", "net.ipv4.ip_forward=1"])
    # NAT rules (idempotent)
    for chk, add in [
        (["iptables", "-t", "nat", "-C", "POSTROUTING", "-o", iface, "-j", "MASQUERADE"],
         ["iptables", "-t", "nat", "-A", "POSTROUTING", "-o", iface, "-j", "MASQUERADE"]),
        (["iptables", "-C", "FORWARD", "-i", tap, "-o", iface, "-j", "ACCEPT"],
         ["iptables", "-I", "FORWARD", "-i", tap, "-o", iface, "-j", "ACCEPT"]),
        (["iptables", "-C", "FORWARD", "-i", iface, "-o", tap, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"],
         ["iptables", "-I", "FORWARD", "-i", iface, "-o", tap, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"]),
    ]:
        if sudo(chk, check=False).returncode != 0:
            sudo(add)
    return tap, host_ip, guest_ip


def teardown_tap(tap: str) -> None:
    sudo(["ip", "link", "del", tap], check=False)


def host_iface() -> str:
    return subprocess.check_output(
        "ip -o route get 8.8.8.8 | awk '{print $5}'", shell=True
    ).decode().strip()


def add_agent_forward(host_port: int, guest_ip: str, iface: str) -> None:
    """DNAT <host>:host_port -> guest_ip:FUSED_PORT, plus FORWARD allow."""
    rules = [
        ["iptables", "-t", "nat", "-I", "PREROUTING", "-i", iface, "-p", "tcp",
         "--dport", str(host_port), "-j", "DNAT",
         "--to-destination", f"{guest_ip}:{FUSED_PORT}"],
        # Also accept traffic arriving via lo (so 127.0.0.1:<host_port> works).
        ["iptables", "-t", "nat", "-I", "OUTPUT", "-o", "lo", "-p", "tcp",
         "--dport", str(host_port), "-j", "DNAT",
         "--to-destination", f"{guest_ip}:{FUSED_PORT}"],
        ["iptables", "-I", "FORWARD", "-p", "tcp", "-d", guest_ip,
         "--dport", str(FUSED_PORT), "-j", "ACCEPT"],
        ["iptables", "-I", "INPUT", "-p", "tcp", "--dport", str(host_port),
         "-j", "ACCEPT"],
    ]
    for r in rules:
        sudo(r, check=False)


def del_agent_forward(host_port: int, guest_ip: str) -> None:
    iface = host_iface()
    rules = [
        ["iptables", "-t", "nat", "-D", "PREROUTING", "-i", iface, "-p", "tcp",
         "--dport", str(host_port), "-j", "DNAT",
         "--to-destination", f"{guest_ip}:{FUSED_PORT}"],
        ["iptables", "-t", "nat", "-D", "OUTPUT", "-o", "lo", "-p", "tcp",
         "--dport", str(host_port), "-j", "DNAT",
         "--to-destination", f"{guest_ip}:{FUSED_PORT}"],
        ["iptables", "-D", "FORWARD", "-p", "tcp", "-d", guest_ip,
         "--dport", str(FUSED_PORT), "-j", "ACCEPT"],
        ["iptables", "-D", "INPUT", "-p", "tcp", "--dport", str(host_port),
         "-j", "ACCEPT"],
    ]
    for r in rules:
        sudo(r, check=False)


def _free_host_port() -> int:
    """Picks a free host port by binding to port 0 and reading it back, then
    closing the socket. There is an inherent (small) race between this and
    the DNAT rule being installed; acceptable for this host agent's level of
    simplicity, matching fc-expose.sh's own lack of allocation/conflict
    detection."""
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def add_expose_forward(host_port: int, guest_ip: str, guest_port: int) -> None:
    """Publishes <host_port> -> guest_ip:guest_port via fc-expose.sh."""
    subprocess.run(
        ["bash", str(FC_DIR / "fc-expose.sh"), str(host_port), guest_ip, str(guest_port)],
        check=True,
    )


def del_expose_forward(host_port: int, guest_ip: str, guest_port: int) -> None:
    """Removes a forward previously installed by add_expose_forward. Best
    effort (mirrors del_agent_forward): a vm being destroyed should not fail
    to tear down over a stale firewall rule."""
    subprocess.run(
        ["bash", str(FC_DIR / "fc-expose.sh"), "-d", str(host_port), guest_ip, str(guest_port)],
        check=False,
    )


# -- SSH ----------------------------------------------------------------------

SSH_BASE = [
    "ssh", "-i", str(SSH_KEY),
    "-o", "StrictHostKeyChecking=no",
    "-o", "UserKnownHostsFile=/dev/null",
    "-o", "LogLevel=ERROR",
    "-o", "ConnectTimeout=5",
    "-o", "BatchMode=yes",
    # Reuse one multiplexed connection per guest instead of paying a fresh
    # TCP+SSH handshake on every call (readiness poll, uploads, exec, agent
    # start easily add up to 8-10 calls per create). Short-lived on purpose:
    # ControlPersist expires the master on its own, so a stale socket left
    # by a destroyed VM is harmless (a new VM at the same guest ip just
    # starts a fresh master once the old one has timed out).
    "-o", "ControlMaster=auto",
    "-o", "ControlPersist=60s",
]


def _ssh_control_path(guest_ip: str) -> str:
    return str(SSH_CONTROL_DIR / f"{guest_ip}.sock")


def ssh_exec(guest_ip: str, remote_cmd: str, stdin: bytes | None = None, timeout: float = 60.0) -> tuple[int, bytes, bytes]:
    cmd = SSH_BASE + ["-o", f"ControlPath={_ssh_control_path(guest_ip)}", f"root@{guest_ip}", remote_cmd]
    try:
        cp = subprocess.run(cmd, input=stdin, capture_output=True, timeout=timeout)
    except subprocess.TimeoutExpired as e:
        return 124, b"", f"timeout: {e}".encode()
    return cp.returncode, cp.stdout, cp.stderr


def wait_for_ssh(guest_ip: str, timeout: float = 30.0) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        rc, _, _ = ssh_exec(guest_ip, "true", timeout=4.0)
        if rc == 0:
            return True
        time.sleep(0.3)
    return False


# -- VM lifecycle -------------------------------------------------------------

def vm_dir(vm_id: str) -> Path:
    # vm_id is expected to already be sanitize_name()'d by every caller, but
    # this is the one choke point all vm filesystem paths pass through, so it
    # re-checks: resolve and confirm the result stays under VMS_DIR rather
    # than trusting callers not to pass something like "../../etc" through.
    vms_root = os.path.realpath(str(VMS_DIR))
    resolved = os.path.realpath(os.path.join(vms_root, vm_id))
    if not resolved.startswith(vms_root + os.sep):
        raise HTTPError(400, f"invalid vm id: {vm_id!r}")
    return Path(resolved)


def load_meta(vm_id: str) -> dict | None:
    p = vm_dir(vm_id) / "meta.json"
    if not p.exists():
        return None
    try:
        return json.loads(p.read_text())
    except Exception:
        return None


def save_meta(meta: dict) -> None:
    p = vm_dir(meta["vm_id"]) / "meta.json"
    tmp = p.with_suffix(".tmp")
    tmp.write_text(json.dumps(meta, indent=2))
    tmp.replace(p)


def list_vms() -> list[dict]:
    return [m for d in sorted(VMS_DIR.iterdir()) if (m := load_meta(d.name))]


def pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
        return True
    except Exception:
        return False


def start_firecracker(meta: dict) -> None:
    d = vm_dir(meta["vm_id"])
    sock = d / "fc.sock"
    log = d / "fc.log"
    sudo(["rm", "-f", str(sock)], check=False)
    # Launch firecracker under sudo, detach with setsid; capture pid from shell.
    cmd = (
        f"sudo -n setsid {shlex.quote(FC_BIN)} --api-sock {shlex.quote(str(sock))} "
        f">{shlex.quote(str(log))} 2>&1 & echo $!"
    )
    pid_out = subprocess.check_output(["bash", "-lc", cmd]).decode().strip()
    # The shell pid isn't the firecracker pid; discover via ss/lsof on the sock.
    # The socket usually appears in well under 50ms, so poll fast instead of
    # eating a fixed startup delay before the first check.
    for _ in range(100):
        if sock.exists():
            break
        time.sleep(0.02)
    if not sock.exists():
        raise RuntimeError(f"firecracker failed to open {sock} (see {log})")
    # sudo/setsid can leave its own short-lived process visible alongside the
    # actual detached firecracker child right as the socket appears; settle
    # briefly so pgrep sees the final process tree, then take the highest
    # (most recently forked, i.e. deepest/real) pid among matches rather than
    # the first -- otherwise destroy_vm can end up killing the wrapper and
    # leaking the real firecracker process.
    time.sleep(0.05)
    pgrep = subprocess.run(
        ["pgrep", "-f", f"firecracker --api-sock {sock}"],
        capture_output=True, text=True,
    )
    pids = [int(p) for p in pgrep.stdout.split()]
    real_pid = max(pids) if pids else int(pid_out)
    meta["pid"] = real_pid
    meta["sock"] = str(sock)
    # Make sock readable by our user so we could inspect; curl uses sudo anyway.
    sudo(["chmod", "666", str(sock)], check=False)

    # Configure boot, drive, net, machine-config, start.
    boot_args = (
        "console=ttyS0 reboot=k panic=1 pci=off "
        f"ip={meta['guest_ip']}::{meta['host_ip']}:255.255.255.252::eth0:off"
    )
    steps = [
        ("/boot-source", {"kernel_image_path": str(KERNEL), "boot_args": boot_args}),
        ("/drives/rootfs", {"drive_id": "rootfs", "path_on_host": meta["rootfs"],
                             "is_root_device": True, "is_read_only": False}),
        ("/network-interfaces/eth0", {"iface_id": "eth0", "host_dev_name": meta["tap"],
                                        "guest_mac": meta["mac"]}),
        ("/machine-config", {"vcpu_count": meta["cpus"], "mem_size_mib": meta["memory_mb"]}),
        ("/actions", {"action_type": "InstanceStart"}),
    ]
    for path, body in steps:
        code, resp = fc_api(str(sock), "PUT", path, body)
        if code >= 300:
            raise RuntimeError(f"firecracker API {path} -> {code}: {resp!r}")


def stop_firecracker(meta: dict) -> None:
    pid = meta.get("pid")
    if pid and pid_alive(pid):
        sudo(["kill", "-9", str(pid)], check=False)
    sock = meta.get("sock")
    if sock:
        sudo(["rm", "-f", sock], check=False)


def create_vm(req: dict, source_rootfs: Path | None = None) -> dict:
    name = req.get("name") or f"vm-{uuid.uuid4().hex[:8]}"
    vm_id = sanitize_name(name)

    # Resolve the source rootfs before any allocation, so an unknown named
    # image fails fast with no vm dir/tap/forward left to roll back. Unset
    # (the common case today) is byte-for-byte the existing BASE_ROOTFS path.
    # A caller-supplied source_rootfs (fork_vm, seeding from a snapshot) wins
    # over the image lookup.
    if source_rootfs is None:
        # seed_snapshot boots a prepared rootfs straight out of the host-global
        # snapshot store, with no live source vm involved. That is what a build
        # artifact (fuse build) is, and it is why this differs from fork_vm,
        # which resolves against a running source. It wins over image: the two
        # name the same slot and cannot both apply, and the orchestrator rejects
        # requests carrying both.
        seed = req.get("seed_snapshot") or ""
        if seed:
            source_rootfs = snapshot_rootfs("", seed)
    if source_rootfs is None:
        image = req.get("image") or ""
        source_rootfs = BASE_ROOTFS
        if image:
            # image names the request into a filesystem path, so resolve it and
            # confirm it stays under IMAGES_DIR; "../../etc/shadow" would else
            # reach a file outside it and get copied into the caller's guest.
            images_root = os.path.realpath(IMAGES_DIR)
            resolved = os.path.realpath(os.path.join(images_root, f"{image}.ext4"))
            if not resolved.startswith(images_root + os.sep):
                raise HTTPError(400, f"invalid base image name: {image!r}")
            source_rootfs = Path(resolved)
            if not source_rootfs.exists():
                raise HTTPError(400, f"base image {image!r} not found at {source_rootfs}; bake and place a rootfs there before use")

    d = vm_dir(vm_id)
    if d.exists():
        raise HTTPError(409, f"vm {vm_id} already exists")
    d.mkdir(parents=True)
    (d / "snapshots").mkdir()

    idx = pick_index()
    iface = host_iface()
    tap, host_ip, guest_ip = setup_tap(idx, iface)
    mac = f"06:00:AC:10:{idx:02x}:02"
    host_port = HOST_PORT_BASE + idx
    add_agent_forward(host_port, guest_ip, iface)
    # Guard against a stale multiplexed connection from a previous VM that
    # held this same guest ip (indices, and so ips, get reused).
    Path(_ssh_control_path(guest_ip)).unlink(missing_ok=True)

    rootfs = d / "rootfs.ext4"
    # Per-VM rootfs copy (so writes don't affect other VMs). Reflink when the
    # filesystem supports copy-on-write (near-instant); same fallback pattern
    # as snapshot_create's rootfs copy below.
    sudo(["cp", "--reflink=auto", str(source_rootfs), str(rootfs)], check=False)
    if not rootfs.exists():
        shutil.copyfile(source_rootfs, rootfs)
    sudo(["chmod", "666", str(rootfs)], check=False)

    meta = {
        "vm_id": vm_id,
        "name": name,
        "index": idx,
        "cpus": int(req.get("cpus", 1)),
        "memory_mb": int(req.get("memory_mb", 512)),
        "storage_gb": int(req.get("storage_gb", 0)),
        "region": req.get("region", ""),
        "tap": tap,
        "host_ip": host_ip,
        "guest_ip": guest_ip,
        "mac": mac,
        "rootfs": str(rootfs),
        "host_port": host_port,
        "url": host_authority(PUBLIC_HOST, host_port),
        "created_at": now_iso(),
        "snapshots": [],
    }
    save_meta(meta)
    try:
        start_firecracker(meta)
        save_meta(meta)
        if not wait_for_ssh(guest_ip, timeout=30.0):
            # Non-fatal: VM booted but SSH didn't come up in time. Still report created.
            meta["ssh_ready"] = False
        else:
            meta["ssh_ready"] = True
            # Fix up guest networking: add default route + DNS if missing.
            ssh_exec(guest_ip, (
                "ip route show default | grep -q . || ip route add default via "
                f"{host_ip}; grep -q 1.1.1.1 /etc/resolv.conf 2>/dev/null || "
                "echo nameserver 1.1.1.1 > /etc/resolv.conf"
            ))
        save_meta(meta)
    except Exception:
        # Roll back on failure.
        stop_firecracker(meta)
        del_agent_forward(host_port, guest_ip)
        teardown_tap(tap)
        shutil.rmtree(d, ignore_errors=True)
        raise
    return meta


def destroy_vm(vm_id: str) -> None:
    meta = load_meta(vm_id)
    if not meta:
        raise HTTPError(404, "vm not found")
    stop_firecracker(meta)
    if "host_port" in meta:
        del_agent_forward(meta["host_port"], meta["guest_ip"])
    for endpoint in meta.get("expose_endpoints", []):
        del_expose_forward(endpoint["host_port"], meta["guest_ip"], endpoint["port"])
    teardown_tap(meta["tap"])
    Path(_ssh_control_path(meta["guest_ip"])).unlink(missing_ok=True)
    # Snapshots are NOT under vm_dir any more (see SNAPSHOTS_DIR), so this
    # removes the vm's own rootfs and metadata only. A build artifact this vm
    # produced deliberately survives its builder.
    sudo(["rm", "-rf", str(vm_dir(vm_id))], check=False)


# -- Snapshots (disk-only) ----------------------------------------------------
# TODO: Implement Firecracker memory snapshots (Pause + CreateSnapshot with
# snapshot_type=Full) to capture RAM + vCPU state. Current disk-only snapshots
# still require a full cold boot on restore (~5-15s). Memory snapshots would
# enable sub-second (<200ms) resume, eliminating cold start latency.

def snapshot_rootfs(vm_id: str, snapshot_id: str) -> Path:
    """Resolve a snapshot's rootfs, preferring the host-global store.

    snapshot_id names the request into a filesystem path, so both candidates are
    realpath'd and confirmed to stay under their root: "../../etc/shadow", or
    "../../<other-vm>/snapshots/<id>", must not resolve outside the store or
    reach a rootfs belonging to a different vm.

    Snapshots taken before SNAPSHOTS_DIR existed still live under their vm dir,
    so that location stays readable. Nothing writes there any more. Pass an
    empty vm_id to restrict the lookup to the store, which is what a seed with
    no live source vm needs.
    """
    store_root = os.path.realpath(str(SNAPSHOTS_DIR))
    resolved = os.path.realpath(os.path.join(store_root, snapshot_id, "rootfs.ext4"))
    if not resolved.startswith(store_root + os.sep):
        raise HTTPError(400, f"invalid snapshot id: {snapshot_id!r}")
    if os.path.exists(resolved):
        return Path(resolved)

    if not vm_id:
        raise HTTPError(404, "snapshot not found")

    legacy_root = os.path.realpath(os.path.join(str(vm_dir(vm_id)), "snapshots"))
    legacy = os.path.realpath(os.path.join(legacy_root, snapshot_id, "rootfs.ext4"))
    if not legacy.startswith(legacy_root + os.sep):
        raise HTTPError(400, f"invalid snapshot id: {snapshot_id!r}")
    if not os.path.exists(legacy):
        raise HTTPError(404, "snapshot not found")
    return Path(legacy)


def file_digest(path: Path | str) -> str:
    """Hex sha256 of a file, read in bounded chunks.

    This is an INTEGRITY check on these exact bytes and nothing more. It is
    NOT a cross-build identity: two builds of the same recipe produce
    different rootfs bytes (timestamps, inode ordering, package caches), so
    the same layer key legitimately yields a different digest every time. Do
    not reach for it as a cache key or a dedup key; the layer key is the cache
    key. The only question this digest can answer is "are the bytes I just
    received over the wire the bytes the other host meant to send".

    A snapshot rootfs is normally mode 666 (it inherits the vm rootfs mode),
    but fall back to sudo on a host where it is not, rather than failing the
    snapshot over a permission bit.
    """
    h = hashlib.sha256()
    try:
        with open(path, "rb", buffering=0) as f:
            for chunk in iter(lambda: f.read(ARTIFACT_CHUNK_BYTES), b""):
                h.update(chunk)
        return h.hexdigest()
    except PermissionError:
        cp = sudo(["sha256sum", "--", str(path)])
        return cp.stdout.decode().split()[0]


def snapshot_create(vm_id: str, comment: str) -> dict:
    meta = load_meta(vm_id)
    if not meta:
        raise HTTPError(404, "vm not found")
    snap_id = f"snap-{int(time.time())}-{uuid.uuid4().hex[:6]}"
    snap_dir = SNAPSHOTS_DIR / snap_id
    snap_dir.mkdir(parents=True)
    # Quiesce guest FS then copy.
    ssh_exec(meta["guest_ip"], "sync; sync", timeout=10.0)
    snap_rootfs = snap_dir / "rootfs.ext4"
    sudo(["cp", "--reflink=auto", meta["rootfs"], str(snap_rootfs)], check=False)
    if not snap_rootfs.exists():
        shutil.copyfile(meta["rootfs"], snap_rootfs)
    # Hashing is inline and blocking, so the snapshot is not reported ready
    # until its digest is known. A digest that arrives later is a digest that
    # some caller has already raced past.
    digest = file_digest(snap_rootfs)
    # origin_vm_id records provenance only. It is deliberately NOT a lifetime
    # link: the origin vm may be destroyed while this artifact stays usable.
    record = {
        "snapshot_id": snap_id,
        "comment": comment,
        "created_at": now_iso(),
        "origin_vm_id": vm_id,
        "digest": digest,
    }
    (snap_dir / "meta.json").write_text(json.dumps(record))
    meta.setdefault("snapshots", []).append(record)
    save_meta(meta)
    return record


def snapshot_list(vm_id: str) -> list[dict]:
    meta = load_meta(vm_id)
    if not meta:
        raise HTTPError(404, "vm not found")
    return meta.get("snapshots", [])


def snapshot_restore(vm_id: str, snapshot_id: str) -> None:
    meta = load_meta(vm_id)
    if not meta:
        raise HTTPError(404, "vm not found")
    # snapshot_rootfs applies the same path-containment guard this call site used
    # to inline, and additionally finds artifacts in the host-global store.
    # Restore stays scoped to this vm's own snapshots plus the store; it cannot
    # be steered at "../../<other-vm>/snapshots/<id>".
    snap_rootfs = snapshot_rootfs(vm_id, snapshot_id)
    # Stop firecracker, swap rootfs, recreate the TAP (the dead fc process
    # may still be holding it briefly), restart with same config.
    stop_firecracker(meta)
    time.sleep(0.3)
    if "host_port" in meta:
        del_agent_forward(meta["host_port"], meta["guest_ip"])
    teardown_tap(meta["tap"])
    iface = host_iface()
    tap, host_ip, guest_ip = setup_tap(meta["index"], iface)
    meta["tap"], meta["host_ip"], meta["guest_ip"] = tap, host_ip, guest_ip
    if "host_port" in meta:
        add_agent_forward(meta["host_port"], guest_ip, iface)
    Path(_ssh_control_path(guest_ip)).unlink(missing_ok=True)
    # Copy into a fresh inode and rename over the live rootfs instead of
    # overwriting it in place. The just-killed guest can leave dirty host
    # page-cache pages tied to the old rootfs inode; an in-place cp can
    # leave those stale pages readable at offsets it doesn't touch, which
    # is the source of NUL/stale-byte corruption on restore. A brand-new
    # inode has no such pages to leak.
    tmp_rootfs = f"{meta['rootfs']}.restore-tmp"
    sudo(["cp", str(snap_rootfs), tmp_rootfs])
    sudo(["mv", tmp_rootfs, meta["rootfs"]])
    sudo(["chmod", "666", meta["rootfs"]], check=False)
    start_firecracker(meta)
    save_meta(meta)
    wait_for_ssh(meta["guest_ip"], timeout=30.0)


def fork_vm(src_vm_id: str, req: dict) -> dict:
    """Create a brand-new VM seeded from one of src_vm_id's snapshots.

    This is create_vm with the snapshot's rootfs standing in for the base
    image, so the new VM gets its own index/tap/IPs/MAC/host port and its own
    rootfs copy. The source VM is untouched and keeps running.

    Sizing (cpus/memory/storage/region) defaults to the source's, so a fork
    without overrides reproduces the source's shape.
    """
    src_meta = load_meta(src_vm_id)
    if not src_meta:
        raise HTTPError(404, "source vm not found")
    snapshot_id = req.get("snapshot_id") or ""
    if not snapshot_id:
        raise HTTPError(400, "snapshot_id required")
    # Path containment and existence are snapshot_rootfs's job now, and it also
    # finds artifacts in the host-global store whose origin vm is long gone.
    snap_rootfs = snapshot_rootfs(src_vm_id, snapshot_id)

    fork_req = dict(req)
    for field in ("cpus", "memory_mb", "storage_gb", "region"):
        if not fork_req.get(field) and src_meta.get(field):
            fork_req[field] = src_meta[field]
    # The rootfs comes from the snapshot, so any "image" in the request is
    # meaningless here; drop it so create_vm cannot try to resolve it.
    fork_req.pop("image", None)

    with _create_lock:
        return create_vm(fork_req, source_rootfs=snap_rootfs)


# -- Content-addressed artifacts (host-to-host) -------------------------------
# A build artifact produced on host A is useful on host B, and there is no
# object storage anywhere in this system to stage it in, so hosts hand
# artifacts to each other directly. Two consequences drive everything below.
#
# First, the serving host is the ONLY thing vouching for the bytes, so the
# receiving host verifies the digest itself and nothing unverified is ever
# allowed to appear in the snapshot store, not even briefly.
#
# Second, B fetching from A is a new trust edge. Until now the only credential
# any agent honoured was `Bearer $FC_AGENT_TOKEN`, held by the orchestrator.
# Handing B the token for A to make the fetch work would give B every
# capability on A (create vms, exec in guests, read every artifact) in order to
# grant it one read of one blob. Instead the orchestrator, which already stores
# every host's agent token, mints a capability grant that A verifies with its
# own token as the HMAC key:
#
#   grant = "v1." + digest + "." + expiry_unix + "." + nonce + "." + mac
#   mac   = HMAC-SHA256(key=<A's FC_AGENT_TOKEN>,
#                       msg="fuse-artifact-grant/v1\n" + digest + "\n"
#                           + expiry_unix + "\n" + nonce)
#
# Properties worth stating, because they are the whole reason for the scheme:
# B never learns A's token (HMAC is one-way); the grant authorizes exactly one
# digest on exactly one endpoint; it is worthless once expired; and A needs no
# call to the orchestrator to verify it.

ARTIFACT_GRANT_HEADER = "X-Fuse-Artifact-Grant"
GRANT_VERSION = "v1"
GRANT_DOMAIN = "fuse-artifact-grant/v1"
# A grant is ~150 chars. Bounding it keeps a hostile header out of the hmac
# path entirely; http.server already caps a header line at 64 KiB.
MAX_GRANT_BYTES = 512
_DIGEST_RE = re.compile(r"[0-9a-f]{64}")
_SNAP_ID_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}")
# The nonce is 16 random bytes hex (32 chars). The accepted range is slack for
# a minter with a different width; the mac covers the nonce either way, so its
# length is a format matter, not a security one.
_GRANT_NONCE_RE = re.compile(r"[0-9a-fA-F]{16,64}")


def artifact_grant_mac(digest: str, expiry: str, nonce: str, key: str | None = None) -> str:
    """HMAC over the grant's own fields, domain-separated.

    The domain string is in the preimage so a mac minted for some other future
    use of this token can never be replayed as an artifact grant.
    """
    msg = f"{GRANT_DOMAIN}\n{digest}\n{expiry}\n{nonce}".encode()
    return hmac.new((key or TOKEN or "").encode(), msg, hashlib.sha256).hexdigest()


def verify_artifact_grant(grant: str, digest: str) -> bool:
    """True if `grant` authorizes reading exactly `digest` from this host now.

    Returns a bare bool on purpose: the caller answers every failure with an
    identical 403. Which check failed (unknown digest vs expired vs forged
    mac) is exactly the oracle an attacker would use to probe, so it never
    leaves this function.
    """
    if not grant or len(grant) > MAX_GRANT_BYTES:
        return False
    parts = grant.split(".")
    if len(parts) != 5:
        return False
    version, g_digest, expiry, nonce, mac = parts
    if version != GRANT_VERSION:
        return False
    # The digest is bound twice: to the url being served (here) and to the mac
    # (below). A grant for one artifact cannot be pointed at another.
    if not hmac.compare_digest(g_digest, digest):
        return False
    if not _DIGEST_RE.fullmatch(g_digest) or not _GRANT_NONCE_RE.fullmatch(nonce):
        return False
    try:
        expires_at = int(expiry)
    except ValueError:
        return False
    if expires_at <= time.time():
        return False
    # compare_digest, not ==, because a byte-at-a-time comparison of the mac is
    # forgeable given enough attempts with a timer. Non-negotiable.
    return hmac.compare_digest(artifact_grant_mac(g_digest, expiry, nonce), mac)


def artifact_rootfs(digest: str) -> Path:
    """Resolve a content digest to a local artifact rootfs.

    The digest arrives over the wire from another host, so it is never used to
    build a path: the store is listed and digests are compared as strings. The
    path that comes back out is still realpath'd and confirmed to stay under
    SNAPSHOTS_DIR, exactly as snapshot_rootfs does, because a store entry can
    itself be a symlink pointing somewhere else. This file has had
    path-traversal findings before; both halves of that guard stay.
    """
    if not _DIGEST_RE.fullmatch(digest):
        raise HTTPError(404, "artifact not found")
    store_root = os.path.realpath(str(SNAPSHOTS_DIR))
    try:
        entries = sorted(os.listdir(store_root))
    except OSError:
        raise HTTPError(404, "artifact not found")
    for entry in entries:
        try:
            with open(os.path.join(store_root, entry, "meta.json"), "rb") as f:
                rec = json.loads(f.read(MAX_BODY_BYTES))
        except (OSError, ValueError):
            continue
        if not isinstance(rec, dict) or rec.get("digest") != digest:
            continue
        resolved = os.path.realpath(os.path.join(store_root, entry, "rootfs.ext4"))
        if not resolved.startswith(store_root + os.sep):
            continue
        if os.path.exists(resolved):
            return Path(resolved)
    raise HTTPError(404, "artifact not found")


def _artifact_dest(snapshot_id: str) -> Path:
    """Where a pulled artifact commits, containment-checked like every other
    store path. snapshot_id comes from the orchestrator rather than from a
    peer, but it still names a directory, so it gets the same treatment."""
    store_root = os.path.realpath(str(SNAPSHOTS_DIR))
    resolved = os.path.realpath(os.path.join(store_root, snapshot_id))
    if not resolved.startswith(store_root + os.sep):
        raise HTTPError(400, f"invalid snapshot id: {snapshot_id!r}")
    return Path(resolved)


def _peer_connection(peer_url: str) -> http.client.HTTPConnection:
    """Connection to a peer agent. Only the scheme/host/port of peer_url are
    used; agents always serve this endpoint at the root of their listener."""
    u = urlparse(peer_url)
    if u.scheme not in ("http", "https") or not u.hostname:
        raise HTTPError(400, f"invalid peer url: {peer_url!r}")
    cls = http.client.HTTPSConnection if u.scheme == "https" else http.client.HTTPConnection
    return cls(u.hostname, u.port, timeout=ARTIFACT_IO_TIMEOUT)


def pull_artifact(digest: str, peer_url: str, grant: str, snapshot_id: str = "") -> dict:
    """Fetch an artifact from a peer agent and commit it only if it verifies.

    Every failure mode (peer refusal, short read, wrong bytes, full disk,
    stall) has to end with nothing in SNAPSHOTS_DIR: a rootfs that is subtly
    not what its digest claims would be seeded into guests forever after, and
    no later step re-checks it. So the stream lands in a temp file outside the
    store, is hashed as it arrives, and only a matching digest earns the
    rename into place.
    """
    if not _DIGEST_RE.fullmatch(digest):
        raise HTTPError(400, "digest must be a lowercase hex sha256")
    if not grant:
        raise HTTPError(400, "grant required")
    # Committing under the id the orchestrator already knows is what makes a
    # pulled artifact indistinguishable from a locally created one: a later
    # create with seed_snapshot=<id> resolves it through snapshot_rootfs with
    # no special case. The digest-derived fallback keeps the endpoint usable
    # for a caller that has no id to impose.
    snapshot_id = snapshot_id or f"art-{digest[:16]}"
    if not _SNAP_ID_RE.fullmatch(snapshot_id):
        raise HTTPError(400, f"invalid snapshot id: {snapshot_id!r}")
    dest_dir = _artifact_dest(snapshot_id)

    tmp = ARTIFACT_TMP_DIR / f"pull-{digest[:16]}-{uuid.uuid4().hex}.tmp"
    deadline = time.monotonic() + ARTIFACT_TRANSFER_TIMEOUT
    running = hashlib.sha256()
    total = 0
    conn = _peer_connection(peer_url)
    try:
        # B presents the grant and NOTHING else. It has no bearer token for the
        # peer and must never be given one.
        conn.request("GET", f"/v1/artifacts/{digest}", headers={ARTIFACT_GRANT_HEADER: grant})
        resp = conn.getresponse()
        if resp.status != 200:
            raise HTTPError(422, f"peer returned {resp.status} for artifact {digest}")

        declared = -1
        raw_len = resp.getheader("Content-Length")
        if raw_len is not None:
            try:
                declared = int(raw_len)
            except ValueError:
                raise HTTPError(422, "peer sent an invalid Content-Length")
        if declared > MAX_ARTIFACT_BYTES:
            raise HTTPError(
                422,
                f"artifact of {declared} bytes exceeds the {MAX_ARTIFACT_BYTES} byte limit",
            )
        # Refuse before writing a byte if the staging filesystem obviously
        # cannot hold it. The streaming loop still handles ENOSPC, since the
        # peer may lie about the length and other things are writing here too.
        if declared >= 0:
            free = shutil.disk_usage(ARTIFACT_TMP_DIR).free
            if declared + (64 << 20) > free:
                raise HTTPError(
                    422, f"artifact of {declared} bytes does not fit in {free} bytes free"
                )

        with open(tmp, "wb", buffering=0) as f:
            while True:
                if time.monotonic() > deadline:
                    raise HTTPError(
                        422, f"artifact transfer exceeded {ARTIFACT_TRANSFER_TIMEOUT}s"
                    )
                chunk = resp.read(ARTIFACT_CHUNK_BYTES)
                if not chunk:
                    break
                total += len(chunk)
                if total > MAX_ARTIFACT_BYTES:
                    raise HTTPError(
                        422, f"artifact exceeds the {MAX_ARTIFACT_BYTES} byte limit"
                    )
                if declared >= 0 and total > declared:
                    raise HTTPError(422, "peer sent more bytes than it declared")
                running.update(chunk)
                f.write(chunk)
            # Durable before the rename: a crash between the two would
            # otherwise publish a file whose tail never reached the disk, and
            # the digest that vouched for it is not re-checked on any later
            # read.
            f.flush()
            os.fsync(f.fileno())

        if declared >= 0 and total != declared:
            raise HTTPError(422, f"short read: got {total} of {declared} bytes")
        if not hmac.compare_digest(running.hexdigest(), digest):
            raise HTTPError(422, "artifact digest mismatch; nothing committed")

        # Same fresh-inode-plus-mv commit as snapshot_restore: the bytes are
        # written to a brand-new inode and renamed into place, never written
        # over a file another process may already have open or cached.
        dest_dir.mkdir(parents=True, exist_ok=True)
        rootfs = dest_dir / "rootfs.ext4"
        sudo(["mv", str(tmp), str(rootfs)])
        sudo(["chmod", "666", str(rootfs)], check=False)
        record = {
            "snapshot_id": snapshot_id,
            "comment": f"pulled from {peer_url}",
            "created_at": now_iso(),
            # No origin vm on this host; provenance is the peer, not a builder.
            "origin_vm_id": "",
            "digest": digest,
            "bytes": total,
            "source_peer": peer_url,
        }
        (dest_dir / "meta.json").write_text(json.dumps(record))
        return record
    except HTTPError:
        raise
    except (OSError, http.client.HTTPException) as e:
        # Transport faults and a full disk are the peer's problem or the
        # host's, never a malformed request, so they read as 422 like a
        # digest mismatch does.
        raise HTTPError(422, f"artifact pull failed: {e}")
    finally:
        conn.close()
        # Unconditional: on every failure path this is the partial file, and
        # on the success path it is already gone.
        tmp.unlink(missing_ok=True)


# -- Upload / Exec / start-agent ---------------------------------------------

def do_upload(vm_id: str, path: str, content_b64: str) -> None:
    meta = load_meta(vm_id)
    if not meta:
        raise HTTPError(404, "vm not found")
    try:
        data = base64.b64decode(content_b64)
    except Exception as e:
        raise HTTPError(400, f"bad base64: {e}")
    remote = (
        f"mkdir -p {shlex.quote(str(Path(path).parent))} && cat > {shlex.quote(path)}"
    )
    rc, out, err = ssh_exec(meta["guest_ip"], remote, stdin=data, timeout=60.0)
    if rc != 0:
        raise HTTPError(500, f"upload failed: {err.decode(errors='replace')}")


def do_exec(vm_id: str, cmd: list[str], timeout_ms: int = 0) -> dict:
    meta = load_meta(vm_id)
    if not meta:
        raise HTTPError(404, "vm not found")
    if not isinstance(cmd, list) or not cmd:
        raise HTTPError(400, "cmd must be non-empty array")
    # The caller's bound wins, capped at our own ceiling so a client cannot ask
    # us to hold an SSH session open indefinitely.
    timeout = EXEC_TIMEOUT_MAX
    if timeout_ms and timeout_ms > 0:
        timeout = min(timeout_ms / 1000.0, EXEC_TIMEOUT_MAX)
    remote = " ".join(shlex.quote(c) for c in cmd)
    rc, out, err = ssh_exec(meta["guest_ip"], remote, timeout=timeout)
    return {
        "exit_code": rc,
        "stdout": base64.b64encode(out).decode(),
        "stderr": base64.b64encode(err).decode(),
    }


def do_start_agent(vm_id: str, manifest_path: str, secrets_path: str,
                   gateway: str | None = None, extra_args: str | None = None,
                   tls_cert_path: str | None = None, tls_key_path: str | None = None,
                   auth_token: str | None = None, download_url: str | None = None,
                   binary_path: str = "/usr/local/bin/fused",
                   listen: str = "0.0.0.0:9550",
                   expose: list[dict] | None = None) -> list[dict]:
    meta = load_meta(vm_id)
    if not meta:
        raise HTTPError(404, "vm not found")
    # Optionally fetch the agent binary into the guest first (skip rootfs
    # re-bake). Idempotent: only download when the target isn't already an
    # executable.
    if download_url:
        fetch = (
            f"test -x {shlex.quote(binary_path)} || "
            f"(curl -fsSL {shlex.quote(download_url)} -o {shlex.quote(binary_path)} "
            f"&& chmod +x {shlex.quote(binary_path)})"
        )
        rc, out, err = ssh_exec(meta["guest_ip"], fetch, timeout=120.0)
        if rc != 0:
            detail = err.decode(errors="replace").strip()
            stdout = out.decode(errors="replace").strip()
            if stdout:
                detail = f"{detail} | stdout: {stdout}" if detail else stdout
            raise HTTPError(500, f"start-agent download failed: {detail or 'unknown error (rc={rc})'}")
    extras = []
    if gateway:
        extras += ["--gateway", gateway]
    extras += ["--vm-id", meta["vm_id"]]
    if tls_cert_path:
        extras += ["--tls-cert", tls_cert_path]
    if tls_key_path:
        extras += ["--tls-key", tls_key_path]
    if auth_token:
        extras += ["--auth-token-file", "/fuse/auth-token"]
    if extra_args:
        extras.append(extra_args)
    extras_str = " ".join(shlex.quote(e) if " " not in e else e for e in extras)
    # Write a SELF-CONTAINED systemd unit (not a drop-in). A drop-in extends a
    # baked-in base fused.service; writing the whole unit here means the agent
    # also works on a plain (unbaked) rootfs as long as `fused` is present —
    # combined with download_url, that's a no-bake deploy.
    unit = (
        "[Unit]\n"
        "Description=Fuse in-guest agent (fused)\n"
        "After=network-online.target\n"
        "Wants=network-online.target\n\n"
        "[Service]\n"
        "Type=simple\n"
        "EnvironmentFile=-/etc/default/fused\n"
        f"ExecStart={binary_path} --listen {listen} "
        f"--manifest {shlex.quote(manifest_path)} "
        f"--secrets {shlex.quote(secrets_path)} $FUSED_EXTRA_ARGS\n"
        "Restart=on-failure\n"
        "RestartSec=2\n\n"
        "[Install]\n"
        "WantedBy=multi-user.target\n"
    )
    # Bring up any declared services before starting the main task. Guarded on
    # the compose file's presence, so environments with no `services` in their
    # Fusefile (including every existing caller of this function, and the
    # FROZEN /start-surfd path which never passes one) see no change at all.
    # `docker-compose` here is the baked podman compose provider (see
    # fc-bake-rootfs.sh) — the guest has no docker CLI.
    compose_up = (
        "if [ -f /fuse/compose.yaml ]; then "
        "/usr/local/bin/docker-compose -f /fuse/compose.yaml up -d; "
        "fi; "
    )
    remote = (
        "export LC_ALL=C; set -e; "
        # Check the absolute binary path, not `command -v fused`: a
        # non-interactive SSH shell's PATH may exclude /usr/local/bin even when
        # the binary is baked in, which would wrongly report it missing.
        f"test -x {shlex.quote(binary_path)} || {{ echo 'agent binary not found at {binary_path}' >&2; exit 127; }}; "
        f"{compose_up}"
        f"printf '%s\\n' 'FUSED_EXTRA_ARGS={extras_str}' > /etc/default/fused; "
        f"cat > /etc/systemd/system/fused.service <<'EOF'\n{unit}EOF\n"
        "systemctl daemon-reload; "
        "systemctl enable fused >/dev/null 2>&1 || true; "
        "systemctl restart fused; "
        "sleep 0.3; systemctl is-active fused"
    )
    rc, out, err = ssh_exec(meta["guest_ip"], remote, timeout=60.0)
    if rc != 0:
        detail = err.decode(errors="replace").strip()
        stdout = out.decode(errors="replace").strip()
        if stdout:
            detail = f"{detail} | stdout: {stdout}" if detail else stdout
        raise HTTPError(500, f"agent start failed: {detail or 'unknown error (rc={rc})'}")

    # Publish any declared ingress ports now that the agent (and any compose
    # services) are up. Guarded on `expose` being non-empty, so callers with
    # no ports to publish (including the FROZEN /start-surfd path, which
    # never passes expose) see no change at all. Persisted onto meta so
    # destroy_vm can remove the same rules on teardown.
    endpoints: list[dict] = []
    if expose:
        for entry in expose:
            guest_port = int(entry["port"])
            host_port = _free_host_port()
            add_expose_forward(host_port, meta["guest_ip"], guest_port)
            endpoints.append({"as": entry.get("as", ""), "url": host_authority(PUBLIC_HOST, host_port), "port": guest_port, "host_port": host_port})
        meta["expose_endpoints"] = endpoints
        save_meta(meta)
    return endpoints


def do_start_surfd(vm_id: str, manifest_path: str, secrets_path: str,
                   gateway: str | None = None, extra_args: str | None = None,
                   tls_cert_path: str | None = None, tls_key_path: str | None = None,
                   auth_token: str | None = None) -> None:
    # Thin wrapper preserving the FROZEN /start-surfd behavior byte-for-byte:
    # fused defaults, no download. The generic launch lives in do_start_agent.
    do_start_agent(
        vm_id, manifest_path, secrets_path,
        gateway=gateway, extra_args=extra_args,
        tls_cert_path=tls_cert_path, tls_key_path=tls_key_path,
        auth_token=auth_token, download_url=None,
        binary_path="/usr/local/bin/fused", listen="0.0.0.0:9550",
    )


# -- HTTP layer ---------------------------------------------------------------

class HTTPError(Exception):
    def __init__(self, code: int, msg: str):
        self.code = code
        self.msg = msg


def vm_public(meta: dict) -> dict:
    return {"vm_id": meta["vm_id"], "url": meta.get("url", "")}


EXEC_TIMEOUT_MAX = 600.0  # ceiling on any single guest command


# -- Attach (interactive pty) -------------------------------------------------
#
# The orchestrator relays a fuse-attach/1 stream between the CLI and this
# endpoint without interpreting it, so the frame codec below is one of only two
# implementations that matter (the other is the Go client). See docs/attach.md.
#
# Frames are [type:1][reserved:3][length:4 big-endian][payload:length].

ATTACH_PROTO = "fuse-attach/1"

FRAME_STDIN = 0
FRAME_STDOUT = 1
FRAME_STDERR = 2
FRAME_RESIZE = 3
FRAME_EXIT = 4

FRAME_HEADER = 8
MAX_FRAME_PAYLOAD = 1 << 20  # a bogus length must not let a peer allocate GBs


def encode_frame(ftype: int, payload: bytes) -> bytes:
    return bytes([ftype, 0, 0, 0]) + len(payload).to_bytes(4, "big") + payload


class FrameDecoder:
    """Incremental decoder: feed it socket reads, get whole frames back.

    Incremental because a TCP read has no relationship to a frame boundary --
    one read can carry half a frame or three of them.
    """

    def __init__(self) -> None:
        self.buf = bytearray()

    def feed(self, data: bytes):
        self.buf += data
        while True:
            if len(self.buf) < FRAME_HEADER:
                return
            length = int.from_bytes(self.buf[4:8], "big")
            if length > MAX_FRAME_PAYLOAD:
                raise ValueError(f"attach frame too large: {length}")
            if len(self.buf) < FRAME_HEADER + length:
                return
            ftype = self.buf[0]
            payload = bytes(self.buf[FRAME_HEADER:FRAME_HEADER + length])
            del self.buf[:FRAME_HEADER + length]
            yield ftype, payload


def set_winsize(fd: int, rows: int, cols: int) -> None:
    """Resize the pty. The kernel sends SIGWINCH to the pty's foreground
    process group, which is how ssh learns to tell the guest its new size."""
    if rows <= 0 or cols <= 0:
        return
    # A winsize field is 16 bits. A relayed resize frame carrying rows/cols
    # above 65535 would otherwise make struct.pack raise struct.error (not an
    # OSError), which the pty relay does not catch, wedging the session. Clamp
    # rather than raise: a client asking for an absurd size wants the maximum.
    rows = min(rows, 0xFFFF)
    cols = min(cols, 0xFFFF)
    packed = struct.pack("HHHH", rows, cols, 0, 0)
    try:
        fcntl.ioctl(fd, termios.TIOCSWINSZ, packed)
    except OSError:
        pass


def parse_attach_spec(query: dict) -> dict:
    """Read the attach spec out of the request query. cmd repeats to preserve
    argv boundaries, so a command containing spaces survives the round trip."""

    def _int(name: str) -> int:
        try:
            return int(query.get(name, ["0"])[0])
        except (ValueError, IndexError):
            return 0

    tty = query.get("tty", [""])[0] in ("1", "true")
    return {
        "cmd": query.get("cmd", []),
        "tty": tty,
        "rows": _int("rows"),
        "cols": _int("cols"),
    }


def drain_buffered(rfile, sock) -> bytes:
    """Return bytes the client sent immediately after the request head.

    They are already inside rfile's buffer, where select() on the socket will
    never see them -- so without this the first keystrokes of a fast client
    would be silently swallowed.
    """
    sock.setblocking(False)
    try:
        peeked = rfile.peek(0)
        if peeked:
            return rfile.read(len(peeked))
        return b""
    except (BlockingIOError, OSError):
        return b""
    finally:
        sock.setblocking(True)


def attach_argv(guest_ip: str, cmd: list[str]) -> list[str]:
    """Build the ssh argv for an attach session.

    -tt forces a pty on the far side even though ssh's own stdin is already
    one; without it a command given to ssh runs without a terminal. An empty
    cmd means the guest's login shell.
    """
    return SSH_BASE + ["-tt", f"root@{guest_ip}"] + list(cmd)


def do_attach(handler, vm_id: str, spec: dict) -> None:
    """Relay a fuse-attach/1 stream between the client socket and a pty running
    ssh into the guest.

    This owns the socket: it writes the 101 itself and the connection is not an
    HTTP conversation afterwards. It must therefore be called outside the
    per-VM lock -- an interactive session lasts as long as a human keeps it
    open, and holding the lock would block every other operation on that VM for
    the duration.
    """
    meta = load_meta(vm_id)
    if not meta:
        raise HTTPError(404, "vm not found")
    if not spec["tty"]:
        raise HTTPError(400, "attach requires tty=1; use /exec for non-interactive commands")

    argv = attach_argv(meta["guest_ip"], spec["cmd"])

    conn = handler.connection
    # An interactive session is idle whenever nobody is typing, so the
    # request-level socket timeout must not apply to it. The selector loop
    # below is what bounds this connection instead.
    conn.settimeout(None)
    pending = drain_buffered(handler.rfile, conn)

    handler.wfile.write(
        b"HTTP/1.1 101 Switching Protocols\r\n"
        b"Upgrade: " + ATTACH_PROTO.encode() + b"\r\n"
        b"Connection: Upgrade\r\n\r\n"
    )
    handler.wfile.flush()

    # pty.fork rather than subprocess: the child needs the pty as its
    # controlling terminal in a new session, which is what makes SIGWINCH
    # delivery (and therefore window resizing) work at all.
    pid, master_fd = pty.fork()
    if pid == 0:
        try:
            # SSH_BASE[0] ("ssh"), not argv[0]: argv is built by concatenating
            # SSH_BASE with the caller-supplied cmd, and indexing into that
            # concatenation reads as an attacker-controlled exec path to static
            # analysis even though position 0 is always the literal from
            # SSH_BASE. Naming the constant directly keeps the executable
            # unambiguous.
            os.execvp(SSH_BASE[0], argv)
        except Exception:
            pass
        os._exit(127)

    set_winsize(master_fd, spec["rows"], spec["cols"])

    decoder = FrameDecoder()
    exit_code = 0
    client_gone = False
    try:
        if pending:
            _pump_client_frames(decoder, pending, master_fd)

        sel = selectors.DefaultSelector()
        sel.register(conn, selectors.EVENT_READ, "sock")
        sel.register(master_fd, selectors.EVENT_READ, "pty")

        while True:
            done = False
            for key, _ in sel.select(timeout=30.0):
                if key.data == "sock":
                    data = conn.recv(65536)
                    if not data:
                        done = True
                        client_gone = True
                        break
                    _pump_client_frames(decoder, data, master_fd)
                else:
                    try:
                        out = os.read(master_fd, 65536)
                    except OSError:
                        out = b""  # EIO on Linux once the child is gone
                    if not out:
                        done = True  # guest process exited
                        break
                    conn.sendall(encode_frame(FRAME_STDOUT, out))
            if done:
                break
    except Exception:
        # Any escape from the relay loop means we are tearing down, and the
        # child must be killed, not waited on: _reap(kill=False) would block in
        # waitpid forever on a still-live ssh -tt process.
        client_gone = True
    finally:
        exit_code = _reap(pid, kill=client_gone)
        try:
            os.close(master_fd)
        except OSError:
            pass

    # Best effort: if the client is the side that went away there is nobody
    # left to tell, and that is not a failure.
    try:
        conn.sendall(encode_frame(FRAME_EXIT, json.dumps({"exit_code": exit_code}).encode()))
    except OSError:
        pass


def _pump_client_frames(decoder: FrameDecoder, data: bytes, master_fd: int) -> None:
    for ftype, payload in decoder.feed(data):
        if ftype == FRAME_STDIN:
            os.write(master_fd, payload)
        elif ftype == FRAME_RESIZE:
            try:
                size = json.loads(payload)
                set_winsize(master_fd, int(size.get("rows", 0)), int(size.get("cols", 0)))
            except (ValueError, TypeError):
                pass
        # stdout/stderr/exit frames are server-to-client; ignore them inbound.


def _reap(pid: int, kill: bool) -> int:
    """Wait for the ssh child and turn its wait status into an exit code.

    Only kill when the client is the side that left. On a normal exit the pty
    reports EOF a hair before the child is reaped, so an unconditional SIGKILL
    would race a process that was already exiting cleanly and report 137 in
    place of its real status.
    """
    if kill:
        try:
            os.kill(pid, signal.SIGKILL)
        except OSError:
            pass
    try:
        _, status = os.waitpid(pid, 0)
    except OSError:
        return 0
    if os.WIFEXITED(status):
        return os.WEXITSTATUS(status)
    if os.WIFSIGNALED(status):
        return 128 + os.WTERMSIG(status)
    return 0


def host_capacity() -> dict:
    """Real hardware capacity of this host: cpu count, total ram, and free
    disk on the filesystem backing FC_DIR (where rootfs images and vm state
    live). Fuse's orchestrator probes this at registration time instead of
    trusting operator-declared --cpus/--ram-mb/--storage-gb flags.
    """
    cpus = os.cpu_count() or 1
    ram_mb = 0
    try:
        with open("/proc/meminfo") as f:
            for line in f:
                if line.startswith("MemTotal:"):
                    ram_mb = int(line.split()[1]) // 1024
                    break
    except OSError:
        pass
    free_bytes = shutil.disk_usage(FC_DIR).free
    # firecracker hosts have no gpu passthrough; report zero for parity with
    # qemu-agent's capacity shape so the wire payload matches.
    return {
        "cpus": cpus,
        "ram_mb": ram_mb,
        "storage_gb": free_bytes // (1024 ** 3),
        "arch": goarch(),
        "gpus": 0,
        "gpu_devices": [],
    }


def goarch() -> str:
    """This host's cpu architecture in goarch vocabulary ("amd64", "arm64"),
    which is what the orchestrator's scheduler matches specs against. An
    unrecognized machine string is passed through lowercased so registration
    can name it in an error instead of silently mis-scheduling.
    """
    machine = platform.machine().lower()
    return {"x86_64": "amd64", "aarch64": "arm64"}.get(machine, machine)


class Handler(BaseHTTPRequestHandler):
    server_version = "fc-agent/0.1"
    # socketserver applies this to the connection, so a client that opens a
    # socket and then stalls mid-request is dropped instead of holding a
    # thread. do_attach clears it for its own socket.
    timeout = REQUEST_TIMEOUT

    def _auth(self) -> bool:
        return hmac.compare_digest(
            self.headers.get("Authorization", ""), f"Bearer {TOKEN}"
        )

    def _json(self, code: int, obj) -> None:
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _text(self, code: int, msg: str) -> None:
        body = msg.encode()
        self.send_response(code)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _read_json(self, limit: int = MAX_BODY_BYTES) -> dict:
        """Read and parse a bounded JSON request body.

        The orchestrator is the only intended caller, but this agent listens on
        a host port, so the body has to be bounded before it is read: a large
        Content-Length would otherwise be believed and allocated.

        Chunked bodies are rejected rather than read. Nothing Fuse sends uses
        them, and the previous code silently treated a chunked body as empty --
        which would also be a way past the limit.
        """
        if "chunked" in self.headers.get("Transfer-Encoding", "").lower():
            raise HTTPError(411, "chunked request bodies are not supported; send Content-Length")

        try:
            n = int(self.headers.get("Content-Length", "0") or 0)
        except ValueError:
            raise HTTPError(400, "invalid Content-Length")
        if n < 0:
            raise HTTPError(400, "invalid Content-Length")
        if n > limit:
            raise HTTPError(413, f"request body of {n} bytes exceeds the {limit} byte limit")

        raw = self.rfile.read(n) if n else b"{}"
        # A short read means the client declared more than it sent. Parsing the
        # truncated remainder would be guessing at what it meant.
        if len(raw) < n:
            raise HTTPError(400, "request body shorter than Content-Length")
        try:
            return json.loads(raw or b"{}")
        except Exception as e:
            raise HTTPError(400, f"bad JSON: {e}")

    def _serve_artifact(self, digest: str) -> None:
        """Stream a local artifact to a peer agent, authorized by a grant.

        This is the one endpoint on this agent that does NOT require
        `Bearer $FC_AGENT_TOKEN`, and that is the point: the pulling host must
        never hold this host's token. See the artifact section above for the
        grant format and why it is shaped that way.
        """
        if not verify_artifact_grant(self.headers.get(ARTIFACT_GRANT_HEADER, ""), digest):
            # One undifferentiated answer for every verification failure.
            return self._text(403, "forbidden")
        path = artifact_rootfs(digest)
        size = path.stat().st_size
        self.send_response(200)
        self.send_header("Content-Type", "application/octet-stream")
        self.send_header("Content-Length", str(size))
        self.send_header("X-Fuse-Artifact-Digest", digest)
        self.end_headers()
        # Bounded chunks with an exact Content-Length: an artifact is hundreds
        # of MB to tens of GB and must never be materialized in memory. The
        # socket timeout bounds each write; the deadline bounds a reader that
        # stays just under it forever and would otherwise pin this thread.
        deadline = time.monotonic() + ARTIFACT_TRANSFER_TIMEOUT
        sent = 0
        try:
            with open(path, "rb", buffering=0) as f:
                while sent < size:
                    if time.monotonic() > deadline:
                        break
                    chunk = f.read(min(ARTIFACT_CHUNK_BYTES, size - sent))
                    if not chunk:
                        break
                    self.wfile.write(chunk)
                    sent += len(chunk)
        except OSError:
            # A peer that hung up or stalled mid-stream. Swallowed rather than
            # raised, because _route's handler would try to send a 500 on top
            # of a response whose headers and part of whose body are already on
            # the wire.
            pass
        if sent < size:
            # Fewer bytes than the Content-Length promised. Drop the connection
            # so the peer cannot mistake the next response for the remainder;
            # its digest check would have rejected the body regardless.
            self.close_connection = True

    def _route(self, method: str):
        path = self.path.split("?", 1)[0]
        try:
            # Grant-authenticated, deliberately ahead of the bearer check: a
            # peer agent pulling a blob has a capability for one digest, not
            # this host's token. Any grant failure is 403 before the store is
            # touched, so this cannot be used to probe which digests exist.
            m = re.fullmatch(r"/v1/artifacts/([^/]+)", path)
            if m and method == "GET":
                return self._serve_artifact(m.group(1))

            if not self._auth():
                return self._text(401, "unauthorized")
            query = {}
            if "?" in self.path:
                for kv in self.path.split("?", 1)[1].split("&"):
                    if "=" in kv:
                        k, v = kv.split("=", 1)
                        query[k] = v

            # /v1/vm collection
            if path == "/v1/vm" and method == "POST":
                req = self._read_json()
                with _create_lock:
                    meta = create_vm(req)
                return self._json(200, vm_public(meta))
            if path == "/v1/vm" and method == "GET":
                prefix = query.get("prefix", "")
                vms = [vm_public(m) for m in list_vms() if m["name"].startswith(prefix)]
                return self._json(200, {"vms": vms})
            # /v1/vm/{id}
            m = re.fullmatch(r"/v1/vm/([^/]+)", path)
            if m:
                vm_id = sanitize_name(m.group(1))
                if method == "GET":
                    meta = load_meta(vm_id)
                    if not meta:
                        raise HTTPError(404, "vm not found")
                    return self._json(200, vm_public(meta))
                if method == "DELETE":
                    # Same lock order as create/fork (vm_lock outer, _create_lock
                    # inner) so a delete can never run mid-create: it either
                    # completes before the create starts or waits for the create
                    # (and its meta.tmp writes) to finish first.
                    with vm_lock(vm_id):
                        with _create_lock:
                            destroy_vm(vm_id)
                    return self._text(204, "")
            # /v1/vm/{id}/<action>
            m = re.fullmatch(r"/v1/vm/([^/]+)/([a-z-]+)", path)
            if m:
                vm_id = sanitize_name(m.group(1))
                action = m.group(2)

                # Attach is handled outside vm_lock and outside the HTTP
                # response machinery: it hijacks the socket for the lifetime of
                # an interactive session, so holding the lock would wedge every
                # other operation on this VM until the human logs out.
                if action == "attach" and method == "GET":
                    self.close_connection = True
                    spec = parse_attach_spec(parse_qs(urlparse(self.path).query, keep_blank_values=True))
                    do_attach(self, vm_id, spec)
                    return None

                with vm_lock(vm_id):
                    if action == "upload" and method == "POST":
                        # Uploads carry a base64 file, so they get the larger
                        # ceiling rather than the control-plane one.
                        body = self._read_json(MAX_UPLOAD_BYTES)
                        do_upload(vm_id, body["path"], body["content_b64"])
                        return self._json(200, {"ok": True})
                    if action == "exec" and method == "POST":
                        body = self._read_json()
                        return self._json(
                            200,
                            do_exec(vm_id, body["cmd"], int(body.get("timeout_ms", 0) or 0)),
                        )
                    if action == "start-surfd" and method == "POST":
                        body = self._read_json()
                        do_start_surfd(
                            vm_id,
                            body["manifest_path"],
                            body["secrets_path"],
                            gateway=body.get("gateway"),
                            extra_args=body.get("extra_args"),
                            tls_cert_path=body.get("tls_cert_path"),
                            tls_key_path=body.get("tls_key_path"),
                            auth_token=body.get("auth_token"),
                        )
                        return self._json(200, {"ok": True})
                    if action == "start-agent" and method == "POST":
                        body = self._read_json()
                        endpoints = do_start_agent(
                            vm_id,
                            body["manifest_path"],
                            body["secrets_path"],
                            gateway=body.get("gateway"),
                            extra_args=body.get("extra_args"),
                            tls_cert_path=body.get("tls_cert_path"),
                            tls_key_path=body.get("tls_key_path"),
                            auth_token=body.get("auth_token"),
                            download_url=body.get("download_url"),
                            binary_path=body.get("binary_path") or "/usr/local/bin/fused",
                            listen=body.get("listen") or "0.0.0.0:9550",
                            expose=body.get("expose"),
                        )
                        return self._json(200, {"ok": True, "endpoints": endpoints})
                    if action == "snapshot" and method == "POST":
                        body = self._read_json()
                        rec = snapshot_create(vm_id, body.get("comment", ""))
                        return self._json(200, {"snapshot_id": rec["snapshot_id"]})
                    if action == "snapshots" and method == "GET":
                        return self._json(200, {"snapshots": snapshot_list(vm_id)})
                    if action == "restore" and method == "POST":
                        body = self._read_json()
                        snapshot_restore(vm_id, body["snapshot_id"])
                        return self._json(200, {"ok": True})
                    if action == "fork" and method == "POST":
                        # Seeds a NEW vm from vm_id's snapshot. Holds vm_id's
                        # lock (the source must not be restored/destroyed
                        # mid-copy) and takes _create_lock inside fork_vm to
                        # allocate the new vm. No path takes _create_lock
                        # before a vm lock, so this order cannot cycle.
                        body = self._read_json()
                        return self._json(200, vm_public(fork_vm(vm_id, body)))
            # Artifact pull. Bearer-authenticated like everything else here,
            # because the caller is the orchestrator; the grant in the body is
            # for the peer, not for us. The body is a few short fields, so it
            # goes through _read_json; the artifact stream it triggers does
            # not, and never could (MAX_BODY_BYTES is 1 MiB).
            m = re.fullmatch(r"/v1/artifacts/([^/]+)/pull", path)
            if m and method == "POST":
                body = self._read_json()
                return self._json(200, pull_artifact(
                    m.group(1),
                    body["peer_url"],
                    body["grant"],
                    body.get("snapshot_id", ""),
                ))
            # Capacity
            if path == "/v1/capacity" and method == "GET":
                return self._json(200, host_capacity())
            # Health
            if path in ("/", "/healthz") and method == "GET":
                return self._json(200, {"ok": True, "app_name": "fc-agent"})
            return self._text(404, "not found")
        except HTTPError as e:
            return self._text(e.code, e.msg)
        except KeyError as e:
            return self._text(400, f"missing field: {e}")
        except Exception as e:
            traceback.print_exc(file=sys.stderr)
            return self._text(500, f"{e}")

    def do_GET(self):    self._route("GET")
    def do_POST(self):   self._route("POST")
    def do_PUT(self):    self._route("PUT")
    def do_DELETE(self): self._route("DELETE")

    def log_message(self, fmt, *args):
        sys.stderr.write("[fc-agent] " + fmt % args + "\n")


def reattach_vms() -> None:
    """On agent startup, re-launch any VMs whose firecracker process is gone.

    Triggered by host reboots (TAPs destroyed, pids gone) and agent crashes.
    VMs with a live pid are left alone.
    """
    if not VMS_DIR.exists():
        return
    for d in sorted(VMS_DIR.iterdir()):
        meta = load_meta(d.name)
        if not meta:
            continue
        pid = meta.get("pid")
        sock = meta.get("sock")
        if pid and pid_alive(pid) and sock and Path(sock).exists():
            print(f"[fc-agent] reattach: {meta['vm_id']} still running (pid {pid})", flush=True)
            continue
        print(f"[fc-agent] reattach: relaunching {meta['vm_id']}", flush=True)
        try:
            # Recreate TAP + DNAT using the stored index/ports.
            teardown_tap(meta["tap"])
            iface = host_iface()
            tap, host_ip, guest_ip = setup_tap(meta["index"], iface)
            meta["tap"], meta["host_ip"], meta["guest_ip"] = tap, host_ip, guest_ip
            if "host_port" in meta:
                del_agent_forward(meta["host_port"], guest_ip)
                add_agent_forward(meta["host_port"], guest_ip, iface)
            start_firecracker(meta)
            save_meta(meta)
        except Exception as e:
            print(f"[fc-agent] reattach FAILED for {meta['vm_id']}: {e}", flush=True)
            traceback.print_exc(file=sys.stderr)


def main():
    reattach_vms()
    srv = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    print(f"[fc-agent] listening :{PORT}", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
