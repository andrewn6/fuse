"""Tests for the code shared by both host agents.

Stdlib-only and hardware-free: agent_common.py imports nothing outside the
stdlib and holds no agent state, so this runs anywhere python3 does.

The public-host cases pin the fix for the malformed IPv6 authority (issue #126):
a dual-stack host resolved to its IPv6 address and the agents emitted
"2607:5300:203:535a:::19554" as an environment url.
"""
import importlib.util
import os
import unittest
from pathlib import Path
from unittest import mock
from urllib.parse import urlparse

_spec = importlib.util.spec_from_file_location(
    "agent_common", Path(__file__).with_name("agent_common.py")
)
common = importlib.util.module_from_spec(_spec)
assert _spec.loader is not None
_spec.loader.exec_module(common)


class HostAuthorityTest(unittest.TestCase):
    def test_ipv4(self):
        self.assertEqual(common.host_authority("203.0.113.7", 19554), "203.0.113.7:19554")

    def test_ipv6_is_bracketed(self):
        self.assertEqual(
            common.host_authority("2607:5300:203:535a::", 19554),
            "[2607:5300:203:535a::]:19554",
        )

    def test_hostname(self):
        self.assertEqual(
            common.host_authority("host.example.com", 8090), "host.example.com:8090"
        )

    def test_composed_ipv6_url_parses(self):
        parsed = urlparse("http://" + common.host_authority("2607:5300:203:535a::", 8090))
        self.assertEqual(parsed.hostname, "2607:5300:203:535a::")
        self.assertEqual(parsed.port, 8090)


class ResolvePublicHostTest(unittest.TestCase):
    def resolve(self, public_host):
        with mock.patch.dict(os.environ, {}, clear=False):
            if public_host is None:
                os.environ.pop("PUBLIC_HOST", None)
            else:
                os.environ["PUBLIC_HOST"] = public_host
            return common.resolve_public_host()

    def test_override_wins(self):
        self.assertEqual(self.resolve("198.51.100.4"), "198.51.100.4")

    def test_override_accepts_ipv6(self):
        self.assertEqual(self.resolve("2607:5300:203:535a::"), "2607:5300:203:535a::")

    def test_override_accepts_hostname(self):
        self.assertEqual(self.resolve("fc1.example.com"), "fc1.example.com")

    def test_override_rejects_garbage(self):
        with self.assertRaises(SystemExit):
            self.resolve("http://2607:5300:203:535a:::8090")

    def test_probe_prefers_ipv4(self):
        def fake_probe(cmd):
            return "203.0.113.7" if "-4" in cmd else "2607:5300:203:535a::"

        with mock.patch.object(common, "_probe", side_effect=fake_probe):
            self.assertEqual(self.resolve(None), "203.0.113.7")

    def test_probe_falls_back_to_ipv6(self):
        def fake_probe(cmd):
            return "2607:5300:203:535a::" if "-6" in cmd else ""

        with mock.patch.object(common, "_probe", side_effect=fake_probe):
            self.assertEqual(self.resolve(None), "2607:5300:203:535a::")

    def test_probe_skips_non_ip_output(self):
        with mock.patch.object(common, "_probe", return_value="<html>error</html>"):
            with self.assertRaises(SystemExit):
                self.resolve(None)

    def test_probe_exhausted_fails_loudly(self):
        with mock.patch.object(common, "_probe", return_value=""):
            with self.assertRaises(SystemExit):
                self.resolve(None)


class FrameCodecTest(unittest.TestCase):
    def test_roundtrip(self):
        raw = common.encode_frame(common.FRAME_STDOUT, b"hello")
        frames = list(common.FrameDecoder().feed(raw))
        self.assertEqual(frames, [(common.FRAME_STDOUT, b"hello")])

    def test_split_reads_are_reassembled(self):
        raw = common.encode_frame(common.FRAME_STDIN, b"abcdef")
        dec = common.FrameDecoder()
        self.assertEqual(list(dec.feed(raw[:5])), [])
        self.assertEqual(list(dec.feed(raw[5:])), [(common.FRAME_STDIN, b"abcdef")])

    def test_oversized_length_is_rejected(self):
        header = bytes([common.FRAME_STDIN, 0, 0, 0]) + (common.MAX_FRAME_PAYLOAD + 1).to_bytes(4, "big")
        with self.assertRaises(ValueError):
            list(common.FrameDecoder().feed(header))

    def test_parse_attach_spec_defaults(self):
        spec = common.parse_attach_spec({})
        self.assertEqual(spec, {"cmd": [], "tty": False, "rows": 0, "cols": 0})

    def test_parse_attach_spec_reads_query(self):
        spec = common.parse_attach_spec(
            {"tty": ["1"], "rows": ["24"], "cols": ["80"], "cmd": ["/bin/sh", "-c", "echo hi"]}
        )
        self.assertTrue(spec["tty"])
        self.assertEqual(spec["rows"], 24)
        self.assertEqual(spec["cols"], 80)
        self.assertEqual(spec["cmd"], ["/bin/sh", "-c", "echo hi"])


class SSHClientTest(unittest.TestCase):
    def setUp(self):
        self.ssh = common.SSHClient(Path("/keys/ubuntu.id_rsa"), Path("/state/ssh-control"))

    def test_base_carries_key_and_multiplexing(self):
        self.assertEqual(self.ssh.base[0], "ssh")
        self.assertIn("/keys/ubuntu.id_rsa", self.ssh.base)
        self.assertIn("ControlPersist=60s", self.ssh.base)

    def test_control_path_is_per_guest(self):
        self.assertEqual(
            self.ssh.control_path("10.200.1.2"), "/state/ssh-control/10.200.1.2.sock"
        )

    def test_attach_argv_forces_a_pty(self):
        argv = self.ssh.attach_argv("10.200.1.2", ["/bin/sh"])
        self.assertEqual(argv[0], "ssh")
        self.assertEqual(argv[-3:], ["-tt", "root@10.200.1.2", "/bin/sh"])


if __name__ == "__main__":
    unittest.main()
