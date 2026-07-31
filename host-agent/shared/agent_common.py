"""Code shared by the Firecracker and QEMU host agents.

Both agents speak the same HTTP contract (host-agent/FUSE.md) and differ only
in how they launch and inspect a VM. Everything that is *not* backend-specific
lives here: the attach frame protocol and its pty relay, the SSH transport to a
guest, public-host resolution, and a handful of process helpers.

Loading: the agents locate this file by path (see `_load_shared` in each), so
it needs no package, no __init__.py, and no sys.path entry. Keep it importable
with the stdlib alone -- host agents run under whatever python3 the distro
ships, with nothing installed.

Nothing here may reference an agent's module-level state (FC_DIR, VMS_DIR,
TOKEN, ...). Anything that needs it takes it as an argument; that constraint is
what keeps the two agents from drifting back apart.
"""
from __future__ import annotations

import fcntl
import ipaddress
import json
import os
import pty
import re
import selectors
import signal
import struct
import subprocess
import termios
import time
from datetime import datetime, timezone
from pathlib import Path


class HTTPError(Exception):
    """Raised by handlers to return a specific status instead of a 500."""

    def __init__(self, code: int, msg: str):
        self.code = code
        self.msg = msg


# -- process helpers ----------------------------------------------------------


def run(cmd: list[str], check: bool = True, input_bytes: bytes | None = None) -> subprocess.CompletedProcess:
    return subprocess.run(cmd, capture_output=True, check=check, input=input_bytes)


def sudo(cmd: list[str], check: bool = True) -> subprocess.CompletedProcess:
    """Run a command under non-interactive sudo."""
    return run(["sudo", "-n"] + cmd, check=check)


def pid_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
        return True
    except Exception:
        return False


def now_iso() -> str:
    """UTC timestamp in the Z-suffixed ISO form both agents write to meta.json."""
    return datetime.now(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


def sanitize_name(name: str) -> str:
    """Reduce an arbitrary name to a dns-safe vm id (lowercase, [a-z0-9-])."""
    s = re.sub(r"[^a-z0-9-]+", "-", name.lower()).strip("-")
    return s or "vm"


# -- public host resolution ---------------------------------------------------
#
# The agent hands the orchestrator a bare "host:port" authority for each VM, so
# a malformed host silently produces environments nobody can reach. Prefer IPv4
# (consumers of the url treat it as an opaque host:port), bracket IPv6 when that
# is all the host has, and fail loudly on a value that is neither an IP nor a
# plausible hostname.

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


# -- SSH transport to a guest -------------------------------------------------


class SSHClient:
    """SSH into guests as root, over a multiplexed connection per guest ip.

    Per-agent state (the key and the control-socket directory live under
    FC_DIR / QEMU_DIR) is held here rather than in module globals, so both
    agents share the transport without sharing a directory layout.
    """

    def __init__(self, key: Path, control_dir: Path):
        self.base = [
            "ssh", "-i", str(key),
            "-o", "StrictHostKeyChecking=no",
            "-o", "UserKnownHostsFile=/dev/null",
            "-o", "LogLevel=ERROR",
            "-o", "ConnectTimeout=5",
            "-o", "BatchMode=yes",
            # Reuse one multiplexed connection per guest instead of paying a
            # fresh TCP+SSH handshake on every call (readiness poll, uploads,
            # exec, agent start easily add up to 8-10 calls per create).
            # Short-lived on purpose: ControlPersist expires the master on its
            # own, so a stale socket left by a destroyed VM is harmless (a new
            # VM at the same guest ip just starts a fresh master once the old
            # one has timed out).
            "-o", "ControlMaster=auto",
            "-o", "ControlPersist=60s",
        ]
        self.control_dir = control_dir

    def control_path(self, guest_ip: str) -> str:
        return str(self.control_dir / f"{guest_ip}.sock")

    def exec(self, guest_ip: str, remote_cmd: str, stdin: bytes | None = None, timeout: float = 60.0) -> tuple[int, bytes, bytes]:
        """Run remote_cmd in the guest over SSH; return (rc, stdout, stderr)."""
        cmd = self.base + ["-o", f"ControlPath={self.control_path(guest_ip)}", f"root@{guest_ip}", remote_cmd]
        try:
            cp = subprocess.run(cmd, input=stdin, capture_output=True, timeout=timeout)
        except subprocess.TimeoutExpired as e:
            return 124, b"", f"timeout: {e}".encode()
        return cp.returncode, cp.stdout, cp.stderr

    def wait(self, guest_ip: str, timeout: float = 30.0) -> bool:
        """Block until the guest accepts SSH or timeout elapses; return readiness."""
        deadline = time.time() + timeout
        while time.time() < deadline:
            rc, _, _ = self.exec(guest_ip, "true", timeout=4.0)
            if rc == 0:
                return True
            time.sleep(0.3)
        return False

    def attach_argv(self, guest_ip: str, cmd: list[str]) -> list[str]:
        """Build the ssh argv for an attach session.

        -tt forces a pty on the far side even though ssh's own stdin is already
        one; without it a command given to ssh runs without a terminal. An empty
        cmd means the guest's login shell.
        """
        return self.base + ["-tt", f"root@{guest_ip}"] + list(cmd)


# -- attach frame protocol ----------------------------------------------------
#
# The orchestrator relays a fuse-attach/1 stream between the CLI and the agent
# without interpreting it, so the frame codec below is one of only two
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


def do_attach(handler, guest_ip: str, spec: dict, ssh: SSHClient, argv_builder=None) -> None:
    """Relay a fuse-attach/1 stream between the client socket and a pty running
    ssh into the guest.

    argv_builder defaults to the client's own attach_argv; callers pass their
    module-level alias so a test can substitute a local shell for ssh.

    This owns the socket: it writes the 101 itself and the connection is not an
    HTTP conversation afterwards. It must therefore be called outside the
    per-VM lock -- an interactive session lasts as long as a human keeps it
    open, and holding the lock would block every other operation on that VM for
    the duration.
    """
    if not spec["tty"]:
        raise HTTPError(400, "attach requires tty=1; use /exec for non-interactive commands")

    argv = (argv_builder or ssh.attach_argv)(guest_ip, spec["cmd"])

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
            # ssh.base[0] ("ssh"), not argv[0]: argv is built by concatenating
            # the ssh base with the caller-supplied cmd, and indexing into that
            # concatenation reads as an attacker-controlled exec path to static
            # analysis even though position 0 is always the literal from the
            # base. Naming it directly keeps the executable unambiguous.
            os.execvp(ssh.base[0], argv)
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
