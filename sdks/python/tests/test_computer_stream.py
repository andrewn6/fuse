# the upgrade path bypasses httpx entirely, so respx cannot mock it; these
# tests run a real socket server the way the go and ts sdk tests do.

from __future__ import annotations

import socket
import threading

import pytest

import fuse

GREETING = b"RFB 003.008\n"


def _serve_upgrade(server: socket.socket, seen: dict) -> None:
    conn, _ = server.accept()
    with conn:
        head = b""
        while b"\r\n\r\n" not in head:
            chunk = conn.recv(4096)
            if not chunk:
                return
            head += chunk
        seen["head"] = head.decode("latin-1")
        conn.sendall(
            b"HTTP/1.1 101 Switching Protocols\r\n"
            b"Upgrade: fuse-vnc/1\r\n"
            b"Connection: Upgrade\r\n\r\n" + GREETING
        )
        data = conn.recv(64)
        if data:
            conn.sendall(b"echo:" + data)


def _serve_error(server: socket.socket, status_line: bytes, body: bytes) -> None:
    conn, _ = server.accept()
    with conn:
        head = b""
        while b"\r\n\r\n" not in head:
            chunk = conn.recv(4096)
            if not chunk:
                return
            head += chunk
        conn.sendall(
            status_line
            + b"Content-Type: application/json\r\n"
            + b"Content-Length: "
            + str(len(body)).encode()
            + b"\r\n\r\n"
            + body
        )


def _listen() -> tuple[socket.socket, str]:
    server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    server.bind(("127.0.0.1", 0))
    server.listen(1)
    host, port = server.getsockname()
    return server, f"http://{host}:{port}"


def test_computer_stream_relays_both_directions() -> None:
    server, base_url = _listen()
    seen: dict = {}
    t = threading.Thread(target=_serve_upgrade, args=(server, seen), daemon=True)
    t.start()
    try:
        with fuse.Client(base_url, "tok") as client:
            with client.environments.computer_stream("vm-1") as stream:
                got = b""
                while len(got) < len(GREETING):
                    got += stream.recv()
                assert got == GREETING

                stream.send(b"hello")
                back = b""
                while len(back) < len(b"echo:hello"):
                    back += stream.recv()
                assert back == b"echo:hello"
        t.join(timeout=5)
        head = seen["head"]
        assert head.startswith("GET /v1/environments/vm-1/computer/stream HTTP/1.1")
        assert "Upgrade: fuse-vnc/1" in head
        assert "Authorization: Bearer tok" in head
    finally:
        server.close()


def test_computer_stream_error_surfaces_as_api_error() -> None:
    server, base_url = _listen()
    body = b'{"error":{"code":"unavailable","message":"guest surface unavailable"}}'
    t = threading.Thread(
        target=_serve_error,
        args=(server, b"HTTP/1.1 503 Service Unavailable\r\n", body),
        daemon=True,
    )
    t.start()
    try:
        with fuse.Client(base_url, "tok") as client:
            with pytest.raises(fuse.ApiError) as exc:
                client.environments.computer_stream("vm-1")
        assert exc.value.status == 503
        assert exc.value.code == "unavailable"
        assert fuse.is_unavailable(exc.value)
    finally:
        server.close()


def test_computer_stream_validates_before_request() -> None:
    with fuse.Client("http://fuse.test", "tok") as client:
        with pytest.raises(ValueError, match="vm id is required"):
            client.environments.computer_stream("")
