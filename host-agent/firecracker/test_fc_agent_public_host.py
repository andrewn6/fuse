"""Regression tests for public host resolution and host:port composition.

Pins the fix for the malformed IPv6 authority (issue #126): a dual-stack host
resolved to its IPv6 address and the agent emitted
"2607:5300:203:535a:::19554" as an environment url.

Stdlib-only, no VMs or virtualization: importing fc-agent.py only needs
FC_DIR/FC_AGENT_TOKEN/PUBLIC_HOST in the env.
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


class HostAuthorityTest(unittest.TestCase):
    def test_ipv4(self):
        self.assertEqual(fc_agent.host_authority("203.0.113.7", 19554), "203.0.113.7:19554")

    def test_ipv6_is_bracketed(self):
        self.assertEqual(
            fc_agent.host_authority("2607:5300:203:535a::", 19554),
            "[2607:5300:203:535a::]:19554",
        )

    def test_hostname(self):
        self.assertEqual(
            fc_agent.host_authority("host.example.com", 8090), "host.example.com:8090"
        )

    def test_composed_ipv6_url_parses(self):
        from urllib.parse import urlparse

        parsed = urlparse("http://" + fc_agent.host_authority("2607:5300:203:535a::", 8090))
        self.assertEqual(parsed.hostname, "2607:5300:203:535a::")
        self.assertEqual(parsed.port, 8090)


class ResolvePublicHostTest(unittest.TestCase):
    def resolve(self, public_host):
        env = {"PUBLIC_HOST": public_host} if public_host is not None else {}
        with mock.patch.dict(os.environ, env, clear=False):
            if public_host is None:
                os.environ.pop("PUBLIC_HOST", None)
            return fc_agent.resolve_public_host()

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
            if "-4" in cmd:
                return "203.0.113.7"
            return "2607:5300:203:535a::"

        with mock.patch.object(fc_agent, "_probe", side_effect=fake_probe):
            self.assertEqual(self.resolve(None), "203.0.113.7")

    def test_probe_falls_back_to_ipv6(self):
        def fake_probe(cmd):
            return "2607:5300:203:535a::" if "-6" in cmd else ""

        with mock.patch.object(fc_agent, "_probe", side_effect=fake_probe):
            self.assertEqual(self.resolve(None), "2607:5300:203:535a::")

    def test_probe_skips_non_ip_output(self):
        with mock.patch.object(fc_agent, "_probe", return_value="<html>error</html>"):
            with self.assertRaises(SystemExit):
                self.resolve(None)

    def test_probe_exhausted_fails_loudly(self):
        with mock.patch.object(fc_agent, "_probe", return_value=""):
            with self.assertRaises(SystemExit):
                self.resolve(None)


class ComposeUpCommandTest(unittest.TestCase):
    """The guest runs podman, not docker.

    Compose defaults to /var/run/docker.sock, which no guest has, so an
    unqualified `docker-compose up` fails to reach any runtime. That failure is
    not contained to the service: compose runs ahead of fused under `set -e`, so
    the whole create returns a 500 and no environment starts.
    """

    def test_points_at_podmans_socket(self):
        cmd = fc_agent.compose_up_command()
        self.assertIn("DOCKER_HOST=unix:///run/podman/podman.sock", cmd)

    def test_never_relies_on_the_docker_socket_default(self):
        # The regression this pins is an *absent* env var, so asserting the
        # docker socket is unmentioned is what actually catches a revert.
        self.assertNotIn("/var/run/docker.sock", fc_agent.compose_up_command())

    def test_sets_the_variable_for_the_compose_command(self):
        # A prefix assignment, not a bare export earlier in the script: the
        # variable has to survive into the compose invocation itself.
        cmd = fc_agent.compose_up_command()
        self.assertIn(
            "DOCKER_HOST=unix:///run/podman/podman.sock "
            "/usr/local/bin/docker-compose -f /fuse/compose.yaml up -d",
            cmd,
        )

    def test_is_guarded_on_the_compose_file(self):
        # An environment with no services declares no compose.yaml, and must not
        # have its boot fail on a missing file.
        self.assertTrue(
            fc_agent.compose_up_command().startswith("if [ -f /fuse/compose.yaml ]; then")
        )


if __name__ == "__main__":
    unittest.main()
