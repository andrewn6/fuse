"""Attach-path tests for the firecracker host agent.

The relay is the trickiest code in the agent -- a pty, a raw hijacked socket,
and a frame protocol -- so these drive it for real rather than mocking it out.
attach_argv is replaced with a local shell, which exercises every layer except
ssh itself: the 101 upgrade, framing in both directions, window resizing, and
exit-code reporting.
"""

import importlib.util
import json
import os
import socket
import tempfile
import threading
import unittest
from http.server import ThreadingHTTPServer
from pathlib import Path
from unittest import mock

_import_root = tempfile.TemporaryDirectory()
os.environ["FC_DIR"] = _import_root.name
os.environ["FC_AGENT_TOKEN"] = "test-token"
os.environ["PUBLIC_HOST"] = "127.0.0.1"
_spec = importlib.util.spec_from_file_location(
    "fc_agent", Path(__file__).with_name("fc-agent.py")
)
fc_agent = importlib.util.module_from_spec(_spec)
assert _spec.loader is not None
_spec.loader.exec_module(fc_agent)


def read_frame(sock):
    """Read one whole frame, or return (None, None) at EOF."""
    head = b""
    while len(head) < fc_agent.FRAME_HEADER:
        chunk = sock.recv(fc_agent.FRAME_HEADER - len(head))
        if not chunk:
            return None, None
        head += chunk
    ftype = head[0]
    length = int.from_bytes(head[4:8], "big")
    payload = b""
    while len(payload) < length:
        chunk = sock.recv(length - len(payload))
        if not chunk:
            return None, None
        payload += chunk
    return ftype, payload


def read_until_exit(sock, deadline_frames=200):
    """Collect stdout until the exit frame, returning (output, exit_code)."""
    out = b""
    for _ in range(deadline_frames):
        ftype, payload = read_frame(sock)
        if ftype is None:
            return out, None
        if ftype == fc_agent.FRAME_STDOUT:
            out += payload
        elif ftype == fc_agent.FRAME_EXIT:
            return out, json.loads(payload)["exit_code"]
    raise AssertionError("no exit frame arrived")


class FrameCodecTest(unittest.TestCase):
    def test_roundtrip(self):
        raw = fc_agent.encode_frame(fc_agent.FRAME_STDOUT, b"hello")
        dec = fc_agent.FrameDecoder()
        self.assertEqual(
            list(dec.feed(raw)), [(fc_agent.FRAME_STDOUT, b"hello")]
        )

    def test_split_across_reads(self):
        """A TCP read has no relationship to a frame boundary."""
        raw = fc_agent.encode_frame(fc_agent.FRAME_STDIN, b"abcdef")
        dec = fc_agent.FrameDecoder()
        self.assertEqual(list(dec.feed(raw[:3])), [])
        self.assertEqual(list(dec.feed(raw[3:9])), [])
        self.assertEqual(
            list(dec.feed(raw[9:])), [(fc_agent.FRAME_STDIN, b"abcdef")]
        )

    def test_multiple_frames_in_one_read(self):
        raw = fc_agent.encode_frame(fc_agent.FRAME_STDIN, b"a") + fc_agent.encode_frame(
            fc_agent.FRAME_STDIN, b"bb"
        )
        dec = fc_agent.FrameDecoder()
        self.assertEqual(
            list(dec.feed(raw)),
            [(fc_agent.FRAME_STDIN, b"a"), (fc_agent.FRAME_STDIN, b"bb")],
        )

    def test_empty_payload(self):
        raw = fc_agent.encode_frame(fc_agent.FRAME_STDOUT, b"")
        dec = fc_agent.FrameDecoder()
        self.assertEqual(list(dec.feed(raw)), [(fc_agent.FRAME_STDOUT, b"")])

    def test_oversized_length_is_rejected(self):
        """A bogus length must not let a peer make us allocate gigabytes."""
        raw = bytes([fc_agent.FRAME_STDIN, 0, 0, 0]) + (1 << 30).to_bytes(4, "big")
        dec = fc_agent.FrameDecoder()
        with self.assertRaises(ValueError):
            list(dec.feed(raw))


class AttachSpecTest(unittest.TestCase):
    def test_repeated_cmd_preserves_argv_boundaries(self):
        from urllib.parse import parse_qs

        spec = fc_agent.parse_attach_spec(
            parse_qs("tty=1&rows=40&cols=120&cmd=sh&cmd=-c&cmd=echo+hi+there")
        )
        self.assertTrue(spec["tty"])
        self.assertEqual(spec["rows"], 40)
        self.assertEqual(spec["cols"], 120)
        self.assertEqual(spec["cmd"], ["sh", "-c", "echo hi there"])

    def test_defaults(self):
        spec = fc_agent.parse_attach_spec({})
        self.assertFalse(spec["tty"])
        self.assertEqual(spec["cmd"], [])
        self.assertEqual(spec["rows"], 0)


class AttachRelayTest(unittest.TestCase):
    """Drives the real pty relay over a real socket, with a local shell
    standing in for ssh."""

    def setUp(self):
        self.temp_dir = tempfile.TemporaryDirectory()
        root = Path(self.temp_dir.name)
        fc_agent.FC_DIR = root
        fc_agent.STATE_DIR = root / "agent-state"
        fc_agent.VMS_DIR = fc_agent.STATE_DIR / "vms"
        fc_agent.VMS_DIR.mkdir(parents=True)

        self.meta_patch = mock.patch.object(
            fc_agent, "load_meta", return_value={"guest_ip": "10.200.1.2"}
        )
        self.meta_patch.start()

        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), fc_agent.Handler)
        self.port = self.srv.server_address[1]
        self.thread = threading.Thread(target=self.srv.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self):
        self.meta_patch.stop()
        self.srv.shutdown()
        self.srv.server_close()
        self.temp_dir.cleanup()

    def dial(self, query):
        s = socket.create_connection(("127.0.0.1", self.port), timeout=10)
        s.settimeout(10)
        s.sendall(
            f"GET /v1/vm/vm-1/attach?{query} HTTP/1.1\r\n"
            f"Host: localhost\r\n"
            f"Authorization: Bearer test-token\r\n"
            f"Connection: Upgrade\r\n"
            f"Upgrade: {fc_agent.ATTACH_PROTO}\r\n\r\n".encode()
        )
        head = b""
        while b"\r\n\r\n" not in head:
            chunk = s.recv(1)
            if not chunk:
                self.fail("connection closed before response")
            head += chunk
        return s, head.decode()

    def test_upgrade_and_command_output(self):
        with mock.patch.object(
            fc_agent, "attach_argv", return_value=["/bin/sh", "-c", "echo hello-guest"]
        ):
            sock, head = self.dial("tty=1&rows=24&cols=80")
            self.assertIn("101 Switching Protocols", head)
            self.assertIn(fc_agent.ATTACH_PROTO, head)

            out, code = read_until_exit(sock)
            self.assertIn(b"hello-guest", out)
            self.assertEqual(code, 0)
            sock.close()

    def test_exit_code_is_reported(self):
        with mock.patch.object(
            fc_agent, "attach_argv", return_value=["/bin/sh", "-c", "exit 7"]
        ):
            sock, head = self.dial("tty=1")
            self.assertIn("101", head)
            _, code = read_until_exit(sock)
            self.assertEqual(code, 7)
            sock.close()

    def test_stdin_reaches_the_process(self):
        """cat echoes what we type back through the pty."""
        with mock.patch.object(
            fc_agent, "attach_argv", return_value=["/bin/sh", "-c", "read line; echo got:$line"]
        ):
            sock, head = self.dial("tty=1&rows=24&cols=80")
            self.assertIn("101", head)
            sock.sendall(fc_agent.encode_frame(fc_agent.FRAME_STDIN, b"ping\n"))
            out, code = read_until_exit(sock)
            self.assertIn(b"got:ping", out)
            self.assertEqual(code, 0)
            sock.close()

    def test_oversized_resize_does_not_wedge_the_session(self):
        """A resize past the 16-bit winsize range must be clamped, not raise
        struct.error. Before the fix that exception escaped every handler and
        left _reap blocked in waitpid forever, wedging the thread and leaking
        the ssh child. Here the session must still complete and report its exit.
        """
        with mock.patch.object(
            fc_agent, "attach_argv", return_value=["/bin/sh", "-c", "read _; exit 0"]
        ):
            sock, head = self.dial("tty=1&rows=24&cols=80")
            self.assertIn("101", head)

            sock.sendall(
                fc_agent.encode_frame(
                    fc_agent.FRAME_RESIZE, json.dumps({"rows": 70000, "cols": 80}).encode()
                )
            )
            # If the resize wedged the relay, this stdin never gets read and
            # read_until_exit hangs until the socket timeout fails the test.
            sock.sendall(fc_agent.encode_frame(fc_agent.FRAME_STDIN, b"\n"))
            _, code = read_until_exit(sock)
            self.assertEqual(code, 0)
            sock.close()

    def test_empty_argv_element_is_preserved(self):
        """`-- grep '' file` must reach the guest as three args, not two. The
        query parse used to drop blank values, silently turning it into
        `grep file`."""
        seen = {}

        def capture(guest_ip, cmd):
            seen["cmd"] = cmd
            return ["/bin/sh", "-c", "exit 0"]

        with mock.patch.object(fc_agent, "attach_argv", side_effect=capture):
            sock, head = self.dial("tty=1&cmd=grep&cmd=&cmd=file")
            self.assertIn("101", head)
            read_until_exit(sock)
            sock.close()

        self.assertEqual(seen["cmd"], ["grep", "", "file"])

    def test_resize_reaches_the_pty(self):
        """The guest must see the window size we asked for, and a later resize
        frame must take effect: that is the whole reason resize is in-band.

        The shell reports its size, blocks on stdin, then reports again. We
        only send the resize once the first size has come back, otherwise the
        resize can win the race and both readings show the new size -- which
        would pass while proving nothing about the initial one.
        """
        with mock.patch.object(
            fc_agent,
            "attach_argv",
            return_value=["/bin/sh", "-c", "stty size; read _; stty size"],
        ):
            sock, head = self.dial("tty=1&rows=24&cols=80")
            self.assertIn("101", head)

            before = b""
            while b"24 80" not in before:
                ftype, payload = read_frame(sock)
                if ftype is None:
                    self.fail(f"stream ended before the initial size; got {before!r}")
                if ftype == fc_agent.FRAME_STDOUT:
                    before += payload

            sock.sendall(
                fc_agent.encode_frame(
                    fc_agent.FRAME_RESIZE, json.dumps({"rows": 50, "cols": 100}).encode()
                )
            )
            sock.sendall(fc_agent.encode_frame(fc_agent.FRAME_STDIN, b"\n"))

            after, code = read_until_exit(sock)
            self.assertIn(b"50 100", after)
            self.assertEqual(code, 0)
            sock.close()

    def test_non_tty_is_rejected(self):
        sock, head = self.dial("tty=0")
        self.assertIn("400", head)
        sock.close()

    def test_unauthorized(self):
        s = socket.create_connection(("127.0.0.1", self.port), timeout=10)
        s.settimeout(10)
        s.sendall(
            b"GET /v1/vm/vm-1/attach?tty=1 HTTP/1.1\r\n"
            b"Host: localhost\r\n"
            b"Connection: Upgrade\r\n"
            b"Upgrade: fuse-attach/1\r\n\r\n"
        )
        head = s.recv(4096).decode()
        self.assertIn("401", head)
        s.close()


if __name__ == "__main__":
    unittest.main()
