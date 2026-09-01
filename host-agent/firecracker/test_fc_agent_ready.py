"""Tests for the two-phase guest readiness probe.

wait_for_ssh used to fork an ssh client every 300ms, paying a handshake per
attempt and reporting readiness up to 300ms late. It now polls a cheap tcp
connect to find the moment sshd starts listening, then confirms with one real
ssh. These pin the behaviour that split has to preserve: a listening socket is
never on its own treated as ready, and the deadline is still honoured.

Stdlib-only, no VMs: importing fc-agent.py only needs FC_DIR/FC_AGENT_TOKEN in
the env, and every ssh call is mocked.
"""
import importlib.util
import os
import socket
import tempfile
import time
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


class TcpOpenTest(unittest.TestCase):
    def test_reports_a_listening_socket(self):
        srv = socket.socket()
        srv.bind(("127.0.0.1", 0))
        srv.listen(1)
        self.addCleanup(srv.close)
        self.assertTrue(fc_agent.tcp_open("127.0.0.1", srv.getsockname()[1]))

    def test_reports_a_closed_port(self):
        # Bind then close, so the port is one nothing is listening on.
        srv = socket.socket()
        srv.bind(("127.0.0.1", 0))
        port = srv.getsockname()[1]
        srv.close()
        self.assertFalse(fc_agent.tcp_open("127.0.0.1", port, timeout=0.2))

    def test_unroutable_address_is_not_an_exception(self):
        # A connect that cannot complete must return False, not raise: the
        # probe runs in a loop and an exception would abort the whole wait.
        self.assertFalse(fc_agent.tcp_open("192.0.2.1", 22, timeout=0.05))


class WaitForSshTest(unittest.TestCase):
    def test_does_not_ssh_until_the_port_is_open(self):
        # The whole point of phase 1: no ssh client is forked while the guest
        # is still booting.
        with mock.patch.object(fc_agent, "tcp_open", return_value=False), \
             mock.patch.object(fc_agent, "ssh_exec") as ssh:
            self.assertFalse(fc_agent.wait_for_ssh("10.0.0.2", timeout=0.15))
        ssh.assert_not_called()

    def test_open_port_alone_is_not_ready(self):
        # sshd binds before it can serve auth, so a listening socket plus a
        # failing command must not be reported as ready.
        with mock.patch.object(fc_agent, "tcp_open", return_value=True), \
             mock.patch.object(fc_agent, "ssh_exec", return_value=(255, b"", b"refused")):
            self.assertFalse(fc_agent.wait_for_ssh("10.0.0.2", timeout=0.5))

    def test_ready_once_the_command_succeeds(self):
        with mock.patch.object(fc_agent, "tcp_open", return_value=True), \
             mock.patch.object(fc_agent, "ssh_exec", return_value=(0, b"", b"")):
            self.assertTrue(fc_agent.wait_for_ssh("10.0.0.2", timeout=5.0))

    def test_recovers_when_the_first_ssh_attempt_fails(self):
        # A stale listener from a recycled guest ip, or an sshd still coming
        # up, must send us back to probing instead of failing the wait.
        attempts = []

        def flaky(*a, **kw):
            attempts.append(1)
            return (0, b"", b"") if len(attempts) > 2 else (255, b"", b"refused")

        with mock.patch.object(fc_agent, "tcp_open", return_value=True), \
             mock.patch.object(fc_agent, "ssh_exec", side_effect=flaky):
            self.assertTrue(fc_agent.wait_for_ssh("10.0.0.2", timeout=5.0))
        self.assertEqual(len(attempts), 3)

    def test_honours_the_deadline(self):
        started = time.monotonic()
        with mock.patch.object(fc_agent, "tcp_open", return_value=False), \
             mock.patch.object(fc_agent, "ssh_exec", return_value=(255, b"", b"")):
            self.assertFalse(fc_agent.wait_for_ssh("10.0.0.2", timeout=0.3))
        elapsed = time.monotonic() - started
        # Generous upper bound; the point is that it returns near the deadline
        # rather than running to some internal fixed attempt count.
        self.assertLess(elapsed, 2.0)
        self.assertGreaterEqual(elapsed, 0.3)


if __name__ == "__main__":
    unittest.main()
