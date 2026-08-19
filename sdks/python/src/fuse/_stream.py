# the live desktop stream: a hand-rolled http/1.1 upgrade over a raw socket,
# mirroring dialUpgrade in sdks/go/exec.go. httpx cannot carry an upgrade (a
# client never gets the socket back after a response), so this speaks the
# handshake itself and hands the caller the socket, positioned at the first
# byte of the stream proper.

from __future__ import annotations

import socket
import ssl
from typing import Optional
from urllib.parse import urlsplit

import httpx

from .errors import parse_api_error

# the Upgrade token that opens a live desktop stream.
VNC_PROTO = "fuse-vnc/1"

# bounds the upgrade exchange itself, not the stream that follows, which
# lives as long as the viewer does.
_HANDSHAKE_TIMEOUT = 15.0

# caps on the response head and on an error body, so a broken server cannot
# stream unbounded bytes into memory during the handshake.
_MAX_HEAD_BYTES = 64 * 1024
_MAX_ERROR_BODY = 1 << 20


class ComputerStream:
    """A live duplex connection to an environment's desktop.

    The bytes are raw RFB (VNC), verbatim from the vnc server inside the
    guest: hand the stream to any RFB client. There is no frame protocol on
    top. Close it when done; it is also a context manager.
    """

    def __init__(self, sock: socket.socket, buffered: bytes) -> None:
        self._sock = sock
        # bytes that arrived with the response head belong to the stream
        self._buffered = buffered

    def recv(self, max_bytes: int = 65536) -> bytes:
        """Return up to max_bytes from the stream; b"" means it ended."""
        if self._buffered:
            out, self._buffered = self._buffered[:max_bytes], self._buffered[max_bytes:]
            return out
        return self._sock.recv(max_bytes)

    def send(self, data: bytes) -> None:
        """Send all of data to the desktop."""
        self._sock.sendall(data)

    def fileno(self) -> int:
        # note for selector users: recv() may return buffered bytes that a
        # readiness poll on this fd knows nothing about; drain recv() until
        # it would block before waiting on the fd.
        return self._sock.fileno()

    def close(self) -> None:
        try:
            self._sock.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass
        self._sock.close()

    def __enter__(self) -> ComputerStream:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()


def open_upgrade(
    base_url: str,
    path: str,
    proto: str,
    headers: dict[str, str],
    *,
    ssl_context: Optional[ssl.SSLContext] = None,
) -> ComputerStream:
    """Perform the http/1.1 upgrade by hand and return the stream.

    Raises ApiError when the server answers plain http (the same shape every
    other call in this sdk raises), and OSError family for transport faults.
    """
    parts = urlsplit(base_url)
    if parts.scheme not in ("http", "https"):
        raise ValueError(f"cannot upgrade a {parts.scheme or 'schemeless'} url")
    host = parts.hostname or ""
    port = parts.port or (443 if parts.scheme == "https" else 80)

    sock = socket.create_connection((host, port), timeout=_HANDSHAKE_TIMEOUT)
    try:
        if parts.scheme == "https":
            ctx = ssl_context or ssl.create_default_context()
            sock = ctx.wrap_socket(sock, server_hostname=host)

        # the base url may carry a path prefix; splice like the other sdks do
        full_path = parts.path.rstrip("/") + path

        lines = [
            f"GET {full_path} HTTP/1.1",
            f"Host: {parts.netloc}",
            "Connection: Upgrade",
            f"Upgrade: {proto}",
        ]
        for name, value in headers.items():
            lines.append(f"{name}: {value}")
        sock.sendall(("\r\n".join(lines) + "\r\n\r\n").encode("ascii"))

        head = _read_head(sock)
        header_end = head.index(b"\r\n\r\n") + 4
        status, resp_headers = _parse_head(head[:header_end])
        buffered = head[header_end:]

        if status != 101:
            body = _read_error_body(sock, resp_headers, buffered)
            raise parse_api_error(status, httpx.Headers(resp_headers), body)

        # hand back a deadline-free socket: an idle desktop produces zero
        # rfb traffic and must not be cut off.
        sock.settimeout(None)
        return ComputerStream(sock, buffered)
    except BaseException:
        sock.close()
        raise


def _read_head(sock: socket.socket) -> bytes:
    # read until the blank line ending the response head; whatever follows it
    # in the same segments is stream (or error body) payload.
    head = b""
    while b"\r\n\r\n" not in head:
        if len(head) > _MAX_HEAD_BYTES:
            raise ValueError("upgrade response head too large")
        chunk = sock.recv(4096)
        if not chunk:
            raise ConnectionError("connection closed during upgrade handshake")
        head += chunk
    return head


def _parse_head(head: bytes) -> tuple[int, dict[str, str]]:
    lines = head.decode("latin-1").split("\r\n")
    status_parts = lines[0].split(" ", 2)
    if len(status_parts) < 2 or not status_parts[1].isdigit():
        raise ValueError(f"malformed upgrade response: {lines[0]!r}")
    status = int(status_parts[1])
    headers: dict[str, str] = {}
    for line in lines[1:]:
        if not line or ":" not in line:
            continue
        name, _, value = line.partition(":")
        headers[name.strip()] = value.strip()
    return status, headers


def _read_error_body(sock: socket.socket, headers: dict[str, str], buffered: bytes) -> bytes:
    # best effort: honour content-length when present, otherwise take what
    # arrives until the server closes or the handshake timeout fires.
    want = -1
    length = {k.lower(): v for k, v in headers.items()}.get("content-length", "")
    if length.isdigit():
        want = min(int(length), _MAX_ERROR_BODY)
    body = buffered[:_MAX_ERROR_BODY]
    try:
        while (want < 0 or len(body) < want) and len(body) < _MAX_ERROR_BODY:
            chunk = sock.recv(4096)
            if not chunk:
                break
            body += chunk
    except OSError:
        pass
    return body[:want] if want >= 0 else body
