"""Tests that fc-agent.py is wired to the shared module.

The behaviour itself lives in host-agent/shared/test_agent_common.py. What
matters here is the seam: the agent must find agent_common.py, re-export its
helpers under the names the rest of the file (and the attach tests) use, and
build its SSH client from FC_DIR-derived paths.
"""
import importlib.util
import os
import tempfile
import unittest
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

_shared_path = Path(__file__).resolve().parent.parent / "shared" / "agent_common.py"


class SharedWiringTest(unittest.TestCase):
    def test_loader_found_the_shared_module(self):
        self.assertEqual(Path(fc_agent.common.__file__), _shared_path)

    def test_helpers_are_the_shared_ones(self):
        for name in (
            "host_authority", "resolve_public_host", "probe_public_host", "now_iso",
            "sanitize_name", "pid_alive", "run", "sudo", "encode_frame",
            "parse_attach_spec", "drain_buffered", "set_winsize", "FrameDecoder",
            "HTTPError",
        ):
            self.assertIs(getattr(fc_agent, name), getattr(fc_agent.common, name), name)

    def test_frame_constants_match(self):
        for name in ("ATTACH_PROTO", "FRAME_STDIN", "FRAME_STDOUT", "FRAME_EXIT", "FRAME_HEADER"):
            self.assertEqual(getattr(fc_agent, name), getattr(fc_agent.common, name), name)

    def test_public_host_resolved_at_import(self):
        self.assertEqual(fc_agent.PUBLIC_HOST, "127.0.0.1")

    def test_url_composition_uses_the_shared_helper(self):
        self.assertEqual(
            fc_agent.host_authority("2607:5300:203:535a::", 19554),
            "[2607:5300:203:535a::]:19554",
        )


class SSHWiringTest(unittest.TestCase):
    def test_ssh_client_uses_agent_paths(self):
        self.assertIn(str(fc_agent.SSH_KEY), fc_agent.SSH.base)
        self.assertEqual(
            fc_agent._ssh_control_path("10.200.1.2"),
            str(fc_agent.SSH_CONTROL_DIR / "10.200.1.2.sock"),
        )

    def test_exec_and_wait_are_bound_to_the_client(self):
        self.assertIs(fc_agent.ssh_exec.__self__, fc_agent.SSH)
        self.assertIs(fc_agent.wait_for_ssh.__self__, fc_agent.SSH)
        self.assertIs(fc_agent.attach_argv.__self__, fc_agent.SSH)


class DoAttachWrapperTest(unittest.TestCase):
    def test_unknown_vm_is_a_404(self):
        with mock.patch.object(fc_agent, "load_meta", return_value=None):
            with self.assertRaises(fc_agent.HTTPError) as cm:
                fc_agent.do_attach(object(), "vm-missing", {"tty": True, "cmd": []})
        self.assertEqual(cm.exception.code, 404)

    def test_guest_ip_and_patched_argv_reach_the_shared_relay(self):
        """The wrapper passes the module-level attach_argv through, which is
        what lets the attach tests swap ssh for a local shell."""
        seen = {}

        def fake_relay(handler, guest_ip, spec, ssh, argv_builder=None):
            seen.update(guest_ip=guest_ip, ssh=ssh, argv_builder=argv_builder)

        with (
            mock.patch.object(fc_agent, "load_meta", return_value={"guest_ip": "10.200.1.2"}),
            mock.patch.object(fc_agent.common, "do_attach", side_effect=fake_relay),
            mock.patch.object(fc_agent, "attach_argv", "sentinel-argv-builder"),
        ):
            fc_agent.do_attach(object(), "vm-1", {"tty": True, "cmd": []})

        self.assertEqual(seen["guest_ip"], "10.200.1.2")
        self.assertIs(seen["ssh"], fc_agent.SSH)
        self.assertEqual(seen["argv_builder"], "sentinel-argv-builder")


if __name__ == "__main__":
    unittest.main()
