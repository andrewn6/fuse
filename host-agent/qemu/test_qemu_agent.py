import importlib.util
import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock


_import_root = tempfile.TemporaryDirectory()
os.environ["QEMU_DIR"] = _import_root.name
os.environ["QEMU_AGENT_TOKEN"] = "test-token"
os.environ["PUBLIC_HOST"] = "127.0.0.1"
_spec = importlib.util.spec_from_file_location(
    "qemu_agent", Path(__file__).with_name("qemu-agent.py")
)
qemu_agent = importlib.util.module_from_spec(_spec)
assert _spec.loader is not None
_spec.loader.exec_module(qemu_agent)


class QEMUAgentTest(unittest.TestCase):
    def setUp(self):
        self.temp_dir = tempfile.TemporaryDirectory()
        self.root = Path(self.temp_dir.name)
        qemu_agent.QEMU_DIR = self.root
        qemu_agent.STATE_DIR = self.root / "agent-state"
        qemu_agent.VMS_DIR = qemu_agent.STATE_DIR / "vms"
        qemu_agent.VMS_DIR.mkdir(parents=True)
        qemu_agent.BASE_ROOTFS = self.root / "rootfs-cuda.qcow2"
        qemu_agent.IMAGES_DIR = self.root / "images"
        qemu_agent.IMAGES_DIR.mkdir()
        qemu_agent.VFIO_INVENTORY = self.root / "vfio-inventory.txt"
        qemu_agent.MIG_INVENTORY = self.root / "mig-inventory.txt"

    def tearDown(self):
        self.temp_dir.cleanup()

    def create_without_hardware(self, request):
        with (
            mock.patch.object(qemu_agent, "pick_gpu_slots", return_value=[]),
            mock.patch.object(qemu_agent, "pick_mig_devices", return_value=[]),
            mock.patch.object(
                qemu_agent,
                "setup_tap",
                return_value=("qv1", "10.200.1.1", "10.200.1.2"),
            ),
            mock.patch.object(qemu_agent, "add_agent_forward"),
            mock.patch.object(qemu_agent, "sudo"),
            mock.patch.object(qemu_agent, "start_qemu"),
            mock.patch.object(qemu_agent, "wait_for_ssh", return_value=False),
        ):
            return qemu_agent.create_vm(request)

    def test_create_uses_default_rootfs(self):
        qemu_agent.BASE_ROOTFS.write_bytes(b"default")

        meta = self.create_without_hardware({"name": "default-vm"})

        self.assertEqual(meta["image"], "")
        self.assertEqual(Path(meta["rootfs"]).read_bytes(), b"default")

    def test_create_uses_named_rootfs(self):
        named = qemu_agent.IMAGES_DIR / "cuda.qcow2"
        named.write_bytes(b"named")

        meta = self.create_without_hardware({"name": "named-vm", "image": "cuda"})

        self.assertEqual(meta["image"], "cuda")
        self.assertEqual(Path(meta["rootfs"]).read_bytes(), b"named")

    def assert_image_rejected(self, image):
        """create_vm must 400 on image before it allocates anything."""
        with (
            mock.patch.object(qemu_agent, "setup_tap") as setup_tap,
            mock.patch.object(qemu_agent, "pick_gpu_slots") as pick_gpu_slots,
            mock.patch.object(qemu_agent, "add_agent_forward") as add_agent_forward,
        ):
            with self.assertRaises(qemu_agent.HTTPError) as raised:
                qemu_agent.create_vm({"name": "escape-vm", "image": image})

        self.assertEqual(raised.exception.code, 400, image)
        setup_tap.assert_not_called()
        pick_gpu_slots.assert_not_called()
        add_agent_forward.assert_not_called()
        self.assertFalse((qemu_agent.VMS_DIR / "escape-vm").exists())

    def test_create_rejects_traversal_image_names(self):
        victim = qemu_agent.VMS_DIR / "victim"
        victim.mkdir(parents=True)
        (victim / "rootfs.qcow2").write_bytes(b"victim-secrets")

        for image in (
            "../agent-state/vms/victim/rootfs",
            "../../etc/shadow",
            "sub/../../escape",
            "./../escape",
        ):
            self.assert_image_rejected(image)

    def test_create_rejects_absolute_image_names(self):
        outside = self.root / "outside.qcow2"
        outside.write_bytes(b"outside")

        self.assert_image_rejected(str(self.root / "outside"))
        self.assert_image_rejected("/etc/passwd")

    def test_create_rejects_symlink_escaping_images_dir(self):
        outside = self.root / "outside.qcow2"
        outside.write_bytes(b"outside")
        (qemu_agent.IMAGES_DIR / "linked.qcow2").symlink_to(outside)

        self.assert_image_rejected("linked")

    def test_create_rejects_symlinked_parent_escaping_images_dir(self):
        elsewhere = self.root / "elsewhere"
        elsewhere.mkdir()
        (elsewhere / "cuda.qcow2").write_bytes(b"outside")
        (qemu_agent.IMAGES_DIR / "nested").symlink_to(elsewhere)

        self.assert_image_rejected("nested/cuda")

    def test_create_rejects_control_characters_in_image_name(self):
        self.assert_image_rejected("cuda\x00/../../etc/shadow")
        self.assert_image_rejected("   ")

    def test_create_allows_nested_image_inside_images_dir(self):
        nested = qemu_agent.IMAGES_DIR / "gpu"
        nested.mkdir()
        (nested / "cuda.qcow2").write_bytes(b"nested")

        meta = self.create_without_hardware({"name": "nested-vm", "image": "gpu/cuda"})

        self.assertEqual(Path(meta["rootfs"]).read_bytes(), b"nested")

    def test_create_rejects_missing_rootfs_before_network_setup(self):
        with mock.patch.object(qemu_agent, "setup_tap") as setup_tap:
            with self.assertRaises(qemu_agent.HTTPError) as raised:
                qemu_agent.create_vm({"name": "missing-vm"})

        self.assertEqual(raised.exception.code, 400)
        setup_tap.assert_not_called()

    def test_inventory_group_includes_companion_functions(self):
        qemu_agent.VFIO_INVENTORY.write_text(
            "1 a100 0000:17:00.0 0000:17:00.1\n"
        )

        slots = qemu_agent.pick_gpu_slots(1, "a100")

        self.assertEqual(slots, ["0000:17:00.0", "0000:17:00.1"])

    def test_inventory_group_is_not_split(self):
        qemu_agent.VFIO_INVENTORY.write_text(
            "2 a100 0000:17:00.0 0000:18:00.0\n"
        )

        with self.assertRaises(qemu_agent.HTTPError) as raised:
            qemu_agent.pick_gpu_slots(1, "a100")

        self.assertEqual(raised.exception.code, 409)

    def test_used_companion_function_reserves_whole_group(self):
        qemu_agent.VFIO_INVENTORY.write_text(
            "1 a100 0000:17:00.0 0000:17:00.1\n"
        )
        with mock.patch.object(
            qemu_agent, "allocated_pci_slots", return_value={"0000:17:00.1"}
        ):
            with self.assertRaises(qemu_agent.HTTPError) as raised:
                qemu_agent.pick_gpu_slots(1, "a100")

        self.assertEqual(raised.exception.code, 409)

    def test_no_gpu_capacity_does_not_leave_vm_state(self):
        qemu_agent.BASE_ROOTFS.write_bytes(b"default")

        with self.assertRaises(qemu_agent.HTTPError):
            qemu_agent.create_vm({"name": "no-capacity", "gpus": 1})

        self.assertFalse(qemu_agent.vm_dir("no-capacity").exists())

    def test_pick_mig_devices_by_profile(self):
        qemu_agent.MIG_INVENTORY.write_text(
            "1g.10gb a100 aaa11111-1111-1111-1111-111111111111\n"
            "1g.10gb a100 bbb22222-2222-2222-2222-222222222222\n"
            "2g.20gb a100 ccc33333-3333-3333-3333-333333333333\n"
        )

        uuids = qemu_agent.pick_mig_devices(2, "1g.10gb", "a100")

        self.assertEqual(
            uuids,
            [
                "aaa11111-1111-1111-1111-111111111111",
                "bbb22222-2222-2222-2222-222222222222",
            ],
        )

    def test_pick_mig_devices_skips_allocated(self):
        qemu_agent.MIG_INVENTORY.write_text(
            "1g.10gb a100 aaa11111-1111-1111-1111-111111111111\n"
            "1g.10gb a100 bbb22222-2222-2222-2222-222222222222\n"
        )
        with mock.patch.object(
            qemu_agent,
            "allocated_mdev_uuids",
            return_value={"aaa11111-1111-1111-1111-111111111111"},
        ):
            uuids = qemu_agent.pick_mig_devices(1, "1g.10gb", None)

        self.assertEqual(uuids, ["bbb22222-2222-2222-2222-222222222222"])

    def test_pick_mig_devices_insufficient_raises_409(self):
        qemu_agent.MIG_INVENTORY.write_text(
            "1g.10gb a100 aaa11111-1111-1111-1111-111111111111\n"
        )

        with self.assertRaises(qemu_agent.HTTPError) as raised:
            qemu_agent.pick_mig_devices(2, "1g.10gb", "a100")

        self.assertEqual(raised.exception.code, 409)

    def test_create_with_gpu_profile_uses_mig_path(self):
        qemu_agent.BASE_ROOTFS.write_bytes(b"default")
        qemu_agent.MIG_INVENTORY.write_text(
            "1g.10gb a100 aaa11111-1111-1111-1111-111111111111\n"
        )

        with (
            mock.patch.object(qemu_agent, "pick_gpu_slots") as pick_slots,
            mock.patch.object(
                qemu_agent,
                "setup_tap",
                return_value=("qv1", "10.200.1.1", "10.200.1.2"),
            ),
            mock.patch.object(qemu_agent, "add_agent_forward"),
            mock.patch.object(qemu_agent, "sudo"),
            mock.patch.object(qemu_agent, "start_qemu"),
            mock.patch.object(qemu_agent, "wait_for_ssh", return_value=False),
        ):
            meta = qemu_agent.create_vm(
                {
                    "name": "mig-vm",
                    "gpus": 1,
                    "gpu_kind": "a100",
                    "gpu_profile": "1G.10GB",
                }
            )

        pick_slots.assert_not_called()
        self.assertEqual(meta["gpu_profile"], "1g.10gb")
        self.assertEqual(meta["gpu_slots"], [])
        self.assertEqual(meta["gpu_mdevs"], ["aaa11111-1111-1111-1111-111111111111"])

    def test_host_capacity_reports_mig_instance_uuids(self):
        qemu_agent.MIG_INVENTORY.write_text(
            "1g.10gb a100 aaa11111-1111-1111-1111-111111111111 GPU-parent-1\n"
            "1g.10gb a100 bbb22222-2222-2222-2222-222222222222 GPU-parent-1\n"
            "2g.20gb a100 ccc33333-3333-3333-3333-333333333333 GPU-parent-2\n"
        )

        cap = qemu_agent.host_capacity()

        instances = cap["mig_instances"]
        self.assertEqual(len(instances), 3)
        first = instances[0]
        self.assertEqual(first["profile"], "1g.10gb")
        self.assertEqual(first["kind"], "a100")
        self.assertEqual(first["uuid"], "aaa11111-1111-1111-1111-111111111111")
        self.assertEqual(first["parent_gpu_uuid"], "GPU-parent-1")
        # the count map is derived as a back-compat summary of the instances.
        self.assertEqual(cap["mig_profiles"], {"1g.10gb": 2, "2g.20gb": 1})

    def test_host_capacity_omits_mig_when_no_inventory(self):
        # no mig-inventory.txt: neither mig_instances nor mig_profiles appear,
        # so a cpu-only or vfio-only host reports no MIG capacity.
        cap = qemu_agent.host_capacity()
        self.assertNotIn("mig_instances", cap)
        self.assertNotIn("mig_profiles", cap)

    def test_read_mig_inventory_accepts_optional_parent_uuid(self):
        qemu_agent.MIG_INVENTORY.write_text(
            "1g.10gb a100 aaa11111-1111-1111-1111-111111111111 GPU-parent-1\n"
            "2g.20gb a100 bbb22222-2222-2222-2222-222222222222\n"
        )

        devs = qemu_agent.read_mig_inventory()

        self.assertEqual(devs[0]["parent_gpu_uuid"], "GPU-parent-1")
        self.assertEqual(devs[1]["parent_gpu_uuid"], "")

    def test_claim_mig_devices_binds_requested_uuids(self):
        qemu_agent.MIG_INVENTORY.write_text(
            "1g.10gb a100 aaa11111-1111-1111-1111-111111111111\n"
            "1g.10gb a100 bbb22222-2222-2222-2222-222222222222\n"
        )

        uuids = qemu_agent.claim_mig_devices(
            ["bbb22222-2222-2222-2222-222222222222"], "1g.10gb", "a100"
        )

        self.assertEqual(uuids, ["bbb22222-2222-2222-2222-222222222222"])

    def test_claim_mig_devices_rejects_wrong_profile(self):
        qemu_agent.MIG_INVENTORY.write_text(
            "2g.20gb a100 ccc33333-3333-3333-3333-333333333333\n"
        )

        with self.assertRaises(qemu_agent.HTTPError) as raised:
            qemu_agent.claim_mig_devices(
                ["ccc33333-3333-3333-3333-333333333333"], "1g.10gb", None
            )

        self.assertEqual(raised.exception.code, 409)

    def test_claim_mig_devices_rejects_already_allocated(self):
        qemu_agent.MIG_INVENTORY.write_text(
            "1g.10gb a100 aaa11111-1111-1111-1111-111111111111\n"
        )
        with mock.patch.object(
            qemu_agent,
            "allocated_mdev_uuids",
            return_value={"aaa11111-1111-1111-1111-111111111111"},
        ):
            with self.assertRaises(qemu_agent.HTTPError) as raised:
                qemu_agent.claim_mig_devices(
                    ["aaa11111-1111-1111-1111-111111111111"], "1g.10gb", None
                )

        self.assertEqual(raised.exception.code, 409)

    def test_create_binds_requested_mig_instance_uuids(self):
        qemu_agent.BASE_ROOTFS.write_bytes(b"default")
        qemu_agent.MIG_INVENTORY.write_text(
            "1g.10gb a100 aaa11111-1111-1111-1111-111111111111 GPU-parent-1\n"
            "1g.10gb a100 bbb22222-2222-2222-2222-222222222222 GPU-parent-1\n"
        )

        with (
            mock.patch.object(qemu_agent, "pick_gpu_slots") as pick_slots,
            mock.patch.object(qemu_agent, "pick_mig_devices") as pick_mig,
            mock.patch.object(
                qemu_agent,
                "setup_tap",
                return_value=("qv1", "10.200.1.1", "10.200.1.2"),
            ),
            mock.patch.object(qemu_agent, "add_agent_forward"),
            mock.patch.object(qemu_agent, "sudo"),
            mock.patch.object(qemu_agent, "start_qemu"),
            mock.patch.object(qemu_agent, "wait_for_ssh", return_value=False),
        ):
            meta = qemu_agent.create_vm(
                {
                    "name": "mig-vm",
                    "gpus": 1,
                    "gpu_kind": "a100",
                    "gpu_profile": "1g.10gb",
                    "mig_instance_uuids": ["bbb22222-2222-2222-2222-222222222222"],
                }
            )

        # the orchestrator-chosen uuid was bound, and neither local picker ran.
        pick_slots.assert_not_called()
        pick_mig.assert_not_called()
        self.assertEqual(
            meta["gpu_mdevs"], ["bbb22222-2222-2222-2222-222222222222"]
        )

    def test_destroy_reapplies_mig_layout_when_lifecycle_managed(self):
        qemu_agent.BASE_ROOTFS.write_bytes(b"default")
        qemu_agent.MIG_INVENTORY.write_text(
            "1g.10gb a100 aaa11111-1111-1111-1111-111111111111\n"
        )

        with (
            mock.patch.object(
                qemu_agent,
                "setup_tap",
                return_value=("qv1", "10.200.1.1", "10.200.1.2"),
            ),
            mock.patch.object(qemu_agent, "add_agent_forward"),
            mock.patch.object(qemu_agent, "sudo"),
            mock.patch.object(qemu_agent, "start_qemu"),
            mock.patch.object(qemu_agent, "wait_for_ssh", return_value=False),
        ):
            meta = qemu_agent.create_vm(
                {"name": "mig-vm", "gpus": 1, "gpu_profile": "1g.10gb"}
            )

        with (
            mock.patch.object(qemu_agent, "stop_qemu"),
            mock.patch.object(qemu_agent, "del_agent_forward"),
            mock.patch.object(qemu_agent, "teardown_tap"),
            mock.patch.object(qemu_agent, "MIG_LIFECYCLE_MANAGED", True),
            mock.patch.object(qemu_agent, "MIG_SETUP_SCRIPT", self.root / "mig-setup.sh"),
            mock.patch.object(qemu_agent.Path, "exists", return_value=True),
            mock.patch("subprocess.run") as run,
            mock.patch.object(qemu_agent, "sudo"),
        ):
            qemu_agent.destroy_vm(meta["vm_id"])

        # the lifecycle hook re-ran the setup script exactly once.
        self.assertEqual(run.call_count, 1)
        self.assertEqual(run.call_args.args[0], [str(self.root / "mig-setup.sh")])

    def test_destroy_skips_mig_reapply_when_not_lifecycle_managed(self):
        qemu_agent.BASE_ROOTFS.write_bytes(b"default")
        qemu_agent.MIG_INVENTORY.write_text(
            "1g.10gb a100 aaa11111-1111-1111-1111-111111111111\n"
        )

        with (
            mock.patch.object(
                qemu_agent,
                "setup_tap",
                return_value=("qv1", "10.200.1.1", "10.200.1.2"),
            ),
            mock.patch.object(qemu_agent, "add_agent_forward"),
            mock.patch.object(qemu_agent, "sudo"),
            mock.patch.object(qemu_agent, "start_qemu"),
            mock.patch.object(qemu_agent, "wait_for_ssh", return_value=False),
        ):
            meta = qemu_agent.create_vm(
                {"name": "mig-vm", "gpus": 1, "gpu_profile": "1g.10gb"}
            )

        with (
            mock.patch.object(qemu_agent, "stop_qemu"),
            mock.patch.object(qemu_agent, "del_agent_forward"),
            mock.patch.object(qemu_agent, "teardown_tap"),
            mock.patch.object(qemu_agent, "MIG_LIFECYCLE_MANAGED", False),
            mock.patch("subprocess.run") as run,
            mock.patch.object(qemu_agent, "sudo"),
        ):
            qemu_agent.destroy_vm(meta["vm_id"])

        run.assert_not_called()

    def test_qemu_mig_setup_script_skips_without_nvidia_smi(self):
        # qemu-mig-setup.sh is an operator script run on a real MIG host, so we
        # do not exercise it against live hardware here. this test just guards
        # the contract that --list degrades (rather than crashing) when
        # nvidia-smi is absent, mirroring the skip-on-absent-hardware pattern.
        import shutil as _shutil

        if _shutil.which("nvidia-smi") is not None:
            self.skipTest("nvidia-smi present on this host; skip the absent-path test")

        import subprocess

        script = Path(qemu_agent.__file__).with_name("qemu-mig-setup.sh")
        proc = subprocess.run(
            [str(script), "--list"],
            capture_output=True, text=True, timeout=10,
            env={"QEMU_DIR": str(self.root), "PATH": "/usr/bin:/bin"},
        )
        # the script dies with a clear message rather than producing partial
        # output, so a missing nvidia-smi is loud, not silent.
        self.assertNotEqual(proc.returncode, 0)

    def test_start_qemu_emits_mdev_sysfsdev(self):
        meta = {
            "vm_id": "mig-vm",
            "memory_mb": 1024,
            "cpus": 2,
            "rootfs": "/tmp/rootfs.qcow2",
            "tap": "qv1",
            "mac": "06:00:ac:10:01:02",
            "guest_ip": "10.200.1.2",
            "host_ip": "10.200.1.1",
            "gpu_slots": [],
            "gpu_mdevs": ["aaa11111-1111-1111-1111-111111111111"],
        }
        captured = {}

        def fake_sudo(cmd, check=True):
            if isinstance(cmd, list) and cmd and cmd[0] == "/usr/bin/qemu-system-x86_64":
                captured["argv"] = list(cmd)
            return mock.Mock(returncode=0)

        vm_path = self.root / "vms" / "mig-vm"
        vm_path.mkdir(parents=True)
        (vm_path / "qemu.pid").write_text("12345")
        (vm_path / "qmp.sock").write_text("")

        with (
            mock.patch.object(qemu_agent, "sudo", side_effect=fake_sudo),
            mock.patch.object(qemu_agent, "vm_dir", return_value=vm_path),
            mock.patch.object(qemu_agent, "QEMU_BIN", "/usr/bin/qemu-system-x86_64"),
            mock.patch.object(qemu_agent, "OVMF_CODE", Path("/usr/share/OVMF/OVMF_CODE.fd")),
            mock.patch.object(qemu_agent, "KERNEL", self.root / "vmlinuz.bin"),
            mock.patch("time.sleep"),
        ):
            qemu_agent.start_qemu(meta)

        self.assertIn("argv", captured)
        self.assertIn(
            "vfio-pci,sysfsdev=/sys/bus/mdev/devices/aaa11111-1111-1111-1111-111111111111",
            captured["argv"],
        )


class PublicHostTest(unittest.TestCase):
    """Pins host:port composition (issue #126): a dual-stack host resolved to
    its IPv6 address and the agent emitted "2607:5300:203:535a:::19554"."""

    def test_ipv4(self):
        self.assertEqual(qemu_agent.host_authority("203.0.113.7", 19654), "203.0.113.7:19654")

    def test_ipv6_is_bracketed(self):
        self.assertEqual(
            qemu_agent.host_authority("2607:5300:203:535a::", 19654),
            "[2607:5300:203:535a::]:19654",
        )

    def test_hostname(self):
        self.assertEqual(
            qemu_agent.host_authority("host.example.com", 8091), "host.example.com:8091"
        )

    def test_override_accepts_ip_and_hostname(self):
        for value in ("198.51.100.4", "2607:5300:203:535a::", "gpu1.example.com"):
            with mock.patch.dict(os.environ, {"PUBLIC_HOST": value}):
                self.assertEqual(qemu_agent.resolve_public_host(), value)

    def test_override_rejects_garbage(self):
        with mock.patch.dict(os.environ, {"PUBLIC_HOST": "http://2607:5300:203:535a:::8091"}):
            with self.assertRaises(SystemExit):
                qemu_agent.resolve_public_host()

    def test_probe_prefers_ipv4(self):
        def fake_probe(cmd):
            return "203.0.113.7" if "-4" in cmd else "2607:5300:203:535a::"

        with mock.patch.dict(os.environ, {"PUBLIC_HOST": ""}):
            with mock.patch.object(qemu_agent, "_probe", side_effect=fake_probe):
                self.assertEqual(qemu_agent.resolve_public_host(), "203.0.113.7")

    def test_probe_exhausted_fails_loudly(self):
        with mock.patch.dict(os.environ, {"PUBLIC_HOST": ""}):
            with mock.patch.object(qemu_agent, "_probe", return_value=""):
                with self.assertRaises(SystemExit):
                    qemu_agent.resolve_public_host()


class ComposeUpCommandTest(unittest.TestCase):
    """The guest runs podman, not docker.

    Compose defaults to /var/run/docker.sock, which no guest has, so an
    unqualified `docker-compose up` fails to reach any runtime. That failure is
    not contained to the service: compose runs ahead of fused under `set -e`, so
    the whole create returns a 500 and no environment starts.
    """

    def test_points_at_podmans_socket(self):
        cmd = qemu_agent.compose_up_command()
        self.assertIn("DOCKER_HOST=unix:///run/podman/podman.sock", cmd)

    def test_never_relies_on_the_docker_socket_default(self):
        # The regression this pins is an *absent* env var, so asserting the
        # docker socket is unmentioned is what actually catches a revert.
        self.assertNotIn("/var/run/docker.sock", qemu_agent.compose_up_command())

    def test_sets_the_variable_for_the_compose_command(self):
        # A prefix assignment, not a bare export earlier in the script: the
        # variable has to survive into the compose invocation itself.
        cmd = qemu_agent.compose_up_command()
        self.assertIn(
            "DOCKER_HOST=unix:///run/podman/podman.sock "
            "/usr/local/bin/docker-compose -f /fuse/compose.yaml up -d",
            cmd,
        )

    def test_is_guarded_on_the_compose_file(self):
        # An environment with no services declares no compose.yaml, and must not
        # have its boot fail on a missing file.
        self.assertTrue(
            qemu_agent.compose_up_command().startswith("if [ -f /fuse/compose.yaml ]; then")
        )

class UploadRemoteCommandTest(unittest.TestCase):
    """/fuse holds credentials in cleartext and must not be world readable.

    The upload wire carries no mode, so everything the orchestrator puts in
    /fuse used to land at the session default of 0644: the resolved secrets in
    `env`, the guest agent's `auth-token`, the TLS key. Any unprivileged process
    in the guest could read them.
    """

    RESERVED = [
        "/fuse/env",
        "/fuse/auth-token",
        "/fuse/secrets.json",
        "/fuse/manifest.json",
        "/fuse/compose.yaml",
        "/fuse/tls/key.pem",
    ]

    def test_reserved_files_are_root_only(self):
        for path in self.RESERVED:
            with self.subTest(path=path):
                cmd = qemu_agent.upload_remote_command(path)
                self.assertIn(f"chmod 0600 {path}", cmd)

    def test_reserved_files_are_created_restricted(self):
        # chmod alone would leave the content world readable between `cat` and
        # the chmod, so the mode is also set at creation.
        for path in self.RESERVED:
            with self.subTest(path=path):
                self.assertTrue(
                    qemu_agent.upload_remote_command(path).startswith("umask 0077 &&")
                )

    def test_reserved_directory_is_locked_down(self):
        # A pre-existing /fuse keeps its own mode through umask, so it is
        # chmod'd explicitly. A nested path locks both it and its parent.
        self.assertIn("chmod 0700 /fuse", qemu_agent.upload_remote_command("/fuse/env"))
        nested = qemu_agent.upload_remote_command("/fuse/tls/key.pem")
        self.assertIn("/fuse", nested)
        self.assertIn("/fuse/tls", nested)

    def test_ordinary_paths_are_unchanged(self):
        # Caller files keep the previous behaviour and the default mode: a
        # workload's own files are not the agent's to restrict.
        cmd = qemu_agent.upload_remote_command("/workspace/app/main.py")
        self.assertEqual(cmd, "mkdir -p /workspace/app && cat > /workspace/app/main.py")

    def test_a_path_merely_prefixed_fuse_is_not_reserved(self):
        # /fused and /fuse-backup share the prefix but are not the directory.
        for path in ["/fused/x", "/fuse-backup/x"]:
            with self.subTest(path=path):
                self.assertNotIn("chmod", qemu_agent.upload_remote_command(path))

    def test_traversal_cannot_aim_the_chmod_outside_fuse(self):
        # The dangerous shape: a reserved-looking path that normalizes out of
        # /fuse would otherwise chmod 0700 a directory like / or /etc.
        cmd = qemu_agent.upload_remote_command("/fuse/../etc/passwd")
        self.assertNotIn("chmod", cmd)
        self.assertIn("cat > /etc/passwd", cmd)


if __name__ == "__main__":
    unittest.main()
