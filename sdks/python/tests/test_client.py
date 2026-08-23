from __future__ import annotations

import json

import httpx
import pytest
import respx

import fuse

BASE_URL = "https://fuse.test"


def new_client() -> fuse.Client:
    # client pointed at the mocked base url. respx intercepts httpx globally,
    # so both the normal and streaming clients are captured.
    return fuse.Client(BASE_URL, "tok")


@respx.mock
def test_environments_create() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments").mock(
        return_value=httpx.Response(
            200,
            json={
                "id": "vm-1",
                "state": "running",
                "task_id": "task-1",
                "url": "https://x",
            },
        )
    )
    with new_client() as client:
        env = client.environments.create(fuse.CreateRequest(task_id="task-1"))

    assert route.calls.last.request.method == "POST"
    assert route.calls.last.request.url.path == "/v1/environments"
    assert env.id == "vm-1"
    assert env.state == "running"


@respx.mock
def test_environments_create_with_image_and_expose() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments").mock(
        return_value=httpx.Response(
            200,
            json={"id": "vm-1", "state": "running", "task_id": "task-1", "url": "u"},
        )
    )
    with new_client() as client:
        client.environments.create(
            fuse.CreateRequest(
                task_id="task-1",
                spec=fuse.Spec(image="my/image:latest"),
                expose=[fuse.ExposeSpec(port=8080, as_="web")],
            )
        )

    body = json.loads(route.calls.last.request.content)
    assert body["spec"]["image"] == "my/image:latest"
    assert body["expose"] == [{"port": 8080, "as": "web"}]
    # keyword alias must serialize to the wire key "as", not "as_".
    assert "as_" not in body["expose"][0]


@respx.mock
def test_environments_fork() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments/vm-1").mock(
        return_value=httpx.Response(
            200,
            json={"id": "vm-2", "state": "running", "task_id": "task-1", "url": "u"},
        )
    )
    with new_client() as client:
        env = client.environments.fork(
            "vm-1", fuse.ForkOptions(reuse_snapshot_id="snap-1", comment="c")
        )

    request = route.calls.last.request
    assert request.method == "POST"
    assert request.url.path == "/v1/environments/vm-1"
    assert request.url.params.get("action") == "fork"
    body = json.loads(request.content)
    assert body == {"reuse_snapshot_id": "snap-1", "comment": "c"}
    assert env.id == "vm-2"


@respx.mock
def test_environments_exec() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments/vm-1").mock(
        return_value=httpx.Response(
            200, json={"exit_code": 0, "stdout": "hi\n", "stderr": ""}
        )
    )
    with new_client() as client:
        result = client.environments.exec(
            "vm-1", fuse.ExecRequest(cmd=["ls", "-l"], timeout_ms=5000)
        )

    request = route.calls.last.request
    assert request.method == "POST"
    assert request.url.path == "/v1/environments/vm-1"
    assert request.url.params.get("action") == "exec"
    body = json.loads(request.content)
    assert body == {"cmd": ["ls", "-l"], "timeout_ms": 5000}
    assert result.exit_code == 0
    assert result.stdout == "hi\n"
    assert result.stderr == ""


@respx.mock
def test_environments_exec_nonzero_exit_is_returned() -> None:
    # a command that ran and failed is a successful call, not an error.
    respx.post(f"{BASE_URL}/v1/environments/vm-1").mock(
        return_value=httpx.Response(
            200, json={"exit_code": 2, "stdout": "", "stderr": "no such file\n"}
        )
    )
    with new_client() as client:
        result = client.environments.exec("vm-1", fuse.ExecRequest(cmd=["false"]))

    assert result.exit_code == 2
    assert result.stderr == "no such file\n"


@respx.mock
def test_environments_exec_shell() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments/vm-1").mock(
        return_value=httpx.Response(
            200, json={"exit_code": 0, "stdout": "3\n", "stderr": ""}
        )
    )
    with new_client() as client:
        result = client.environments.exec(
            "vm-1", fuse.ExecRequest(shell="ls | wc -l")
        )

    body = json.loads(route.calls.last.request.content)
    assert body == {"shell": "ls | wc -l"}
    assert result.stdout == "3\n"


def test_environments_exec_requires_exactly_one_of_cmd_or_shell() -> None:
    with new_client() as client:
        with pytest.raises(ValueError):
            client.environments.exec("vm-1", fuse.ExecRequest())
        with pytest.raises(ValueError):
            client.environments.exec(
                "vm-1", fuse.ExecRequest(cmd=["ls"], shell="ls")
            )


@respx.mock
def test_environments_exec_conflict() -> None:
    respx.post(f"{BASE_URL}/v1/environments/vm-1").mock(
        return_value=httpx.Response(
            409, json={"error": {"code": "conflict", "message": "vm is not running"}}
        )
    )
    with new_client() as client:
        with pytest.raises(fuse.ApiError) as excinfo:
            client.environments.exec("vm-1", fuse.ExecRequest(cmd=["ls"]))

    err = excinfo.value
    assert err.status == 409
    assert fuse.is_conflict(err)


@respx.mock
def test_environments_get_with_endpoints() -> None:
    respx.get(f"{BASE_URL}/v1/environments/vm-1").mock(
        return_value=httpx.Response(
            200,
            json={
                "id": "vm-1",
                "state": "running",
                "task_id": "task-1",
                "url": "u",
                "endpoints": [{"as": "web", "url": "https://web", "port": 8080}],
            },
        )
    )
    with new_client() as client:
        env = client.environments.get("vm-1")

    assert len(env.endpoints) == 1
    assert env.endpoints[0].as_ == "web"
    assert env.endpoints[0].url == "https://web"
    assert env.endpoints[0].port == 8080


@respx.mock
def test_environments_list() -> None:
    route = respx.get(f"{BASE_URL}/v1/environments").mock(
        return_value=httpx.Response(
            200,
            json={
                "environments": [
                    {"id": "vm-1", "state": "running", "task_id": "task-1", "url": "u"},
                    {"id": "vm-2", "state": "draining", "task_id": "task-1", "url": "u"},
                ]
            },
        )
    )
    with new_client() as client:
        envs = client.environments.list(task_id="task-1")

    request = route.calls.last.request
    assert request.method == "GET"
    assert request.url.path == "/v1/environments"
    assert request.url.params.get("task_id") == "task-1"
    assert [e.id for e in envs] == ["vm-1", "vm-2"]


@respx.mock
def test_environments_list_page_round_trips_cursor() -> None:
    def responder(request: httpx.Request) -> httpx.Response:
        if "cursor" not in request.url.params:
            return httpx.Response(
                200,
                json={
                    "environments": [{"id": "vm-1", "state": "running", "url": "u"}],
                    "next_cursor": "YWJj",
                },
            )
        return httpx.Response(
            200, json={"environments": [{"id": "vm-2", "state": "running", "url": "u"}]}
        )

    respx.get(f"{BASE_URL}/v1/environments").mock(side_effect=responder)
    with new_client() as client:
        page1 = client.environments.list_page(limit=1)
        assert page1.next_cursor == "YWJj"
        assert [e.id for e in page1.environments] == ["vm-1"]

        page2 = client.environments.list_page(cursor=page1.next_cursor or "")
        assert page2.next_cursor is None
        assert [e.id for e in page2.environments] == ["vm-2"]

        envs = client.environments.list()
        assert [e.id for e in envs] == ["vm-1", "vm-2"]


@respx.mock
def test_environments_drain() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments/vm-1").mock(
        return_value=httpx.Response(
            200,
            json={"id": "vm-1", "state": "draining", "task_id": "task-1", "url": "u"},
        )
    )
    with new_client() as client:
        env = client.environments.drain("vm-1")

    request = route.calls.last.request
    assert request.method == "POST"
    assert request.url.path == "/v1/environments/vm-1"
    assert request.url.params.get("action") == "drain"
    assert env.state == "draining"


@respx.mock
def test_environments_rotate_token() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments/vm-1").mock(
        return_value=httpx.Response(204)
    )
    with new_client() as client:
        client.environments.rotate_token("vm-1")

    request = route.calls.last.request
    assert request.method == "POST"
    assert request.url.path == "/v1/environments/vm-1"
    assert request.url.params.get("action") == "rotate-token"


@respx.mock
def test_snapshots_create() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments/vm-1/snapshots").mock(
        return_value=httpx.Response(
            200,
            json={"id": "snap-1", "vm_id": "vm-1", "created_at": "2024-01-01T00:00:00Z"},
        )
    )
    with new_client() as client:
        snap = client.snapshots.create("vm-1", fuse.SnapshotRequest(comment="c"))

    request = route.calls.last.request
    assert request.method == "POST"
    assert request.url.path == "/v1/environments/vm-1/snapshots"
    assert snap.id == "snap-1"
    assert snap.vm_id == "vm-1"


@respx.mock
def test_snapshots_create_sends_live_and_decodes_kind() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments/vm-1/snapshots").mock(
        return_value=httpx.Response(
            200,
            json={
                "id": "snap-1",
                "vm_id": "vm-1",
                "kind": "live",
                "created_at": "2024-01-01T00:00:00Z",
            },
        )
    )
    with new_client() as client:
        snap = client.snapshots.create("vm-1", fuse.SnapshotRequest(live=True))

    body = json.loads(route.calls.last.request.content)
    assert body["live"] is True
    assert snap.kind == "live"


@respx.mock
def test_snapshots_create_live_served_as_disk_reports_disk() -> None:
    # an agent too old to honour live answers with a disk snapshot; the sdk must
    # surface that rather than let a caller read its own request back.
    respx.post(f"{BASE_URL}/v1/environments/vm-1/snapshots").mock(
        return_value=httpx.Response(
            200,
            json={
                "id": "snap-1",
                "vm_id": "vm-1",
                "kind": "disk",
                "created_at": "2024-01-01T00:00:00Z",
            },
        )
    )
    with new_client() as client:
        snap = client.snapshots.create("vm-1", fuse.SnapshotRequest(live=True))

    assert snap.kind == "disk"


@respx.mock
def test_snapshots_create_omits_live_when_unset() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments/vm-1/snapshots").mock(
        return_value=httpx.Response(
            200,
            json={"id": "snap-1", "vm_id": "vm-1", "created_at": "2024-01-01T00:00:00Z"},
        )
    )
    with new_client() as client:
        snap = client.snapshots.create("vm-1", fuse.SnapshotRequest(comment="c"))

    assert "live" not in json.loads(route.calls.last.request.content)
    # a server that predates the field sends no kind at all; that reads as ""
    # and callers treat it as disk.
    assert snap.kind == ""


@respx.mock
def test_snapshots_list() -> None:
    route = respx.get(f"{BASE_URL}/v1/snapshots").mock(
        return_value=httpx.Response(
            200,
            json={
                "snapshots": [
                    {"id": "snap-1", "vm_id": "vm-1", "created_at": "2024-01-01T00:00:00Z"}
                ]
            },
        )
    )
    with new_client() as client:
        snaps = client.snapshots.list(vm_id="vm-1")

    request = route.calls.last.request
    assert request.method == "GET"
    assert request.url.params.get("vm_id") == "vm-1"
    assert [s.id for s in snaps] == ["snap-1"]


@respx.mock
def test_snapshots_list_page_sends_layer_filters() -> None:
    route = respx.get(f"{BASE_URL}/v1/snapshots").mock(
        return_value=httpx.Response(200, json={"snapshots": []})
    )
    with new_client() as client:
        client.snapshots.list_page(layer_key="abc123", arch="arm64")

    # arch is its own filter rather than being folded into the layer key, so a
    # lookup that omits it can match an artifact it cannot boot.
    request = route.calls.last.request
    assert request.url.params.get("layer_key") == "abc123"
    assert request.url.params.get("arch") == "arm64"


@respx.mock
def test_snapshots_list_page_omits_unset_layer_filters() -> None:
    route = respx.get(f"{BASE_URL}/v1/snapshots").mock(
        return_value=httpx.Response(200, json={"snapshots": []})
    )
    with new_client() as client:
        client.snapshots.list_page(vm_id="vm-1")

    # an empty layer_key would ask the server to match every snapshot that is
    # not a layer, so an unset filter must not be sent at all.
    request = route.calls.last.request
    assert "layer_key" not in request.url.params
    assert "arch" not in request.url.params


@respx.mock
def test_snapshots_get_decodes_artifact_fields() -> None:
    respx.get(f"{BASE_URL}/v1/snapshots/snap-1").mock(
        return_value=httpx.Response(
            200,
            json={
                "id": "snap-1",
                "vm_id": "vm-1",
                "mode": "build",
                "layer_key": "abc123",
                "arch": "amd64",
                "digest": "deadbeef",
                "kind": "live",
                "created_at": "2024-01-01T00:00:00Z",
            },
        )
    )
    with new_client() as client:
        snap = client.snapshots.get("snap-1")

    assert snap.layer_key == "abc123"
    assert snap.arch == "amd64"
    assert snap.digest == "deadbeef"
    # kind has to decode on get too, not only on create: the docs tell callers
    # to check it, which they can only do wherever they read the snapshot.
    assert snap.kind == "live"


@respx.mock
def test_snapshots_restore() -> None:
    route = respx.post(f"{BASE_URL}/v1/snapshots/snap-1").mock(
        return_value=httpx.Response(204)
    )
    with new_client() as client:
        client.snapshots.restore("snap-1")

    request = route.calls.last.request
    assert request.method == "POST"
    assert request.url.path == "/v1/snapshots/snap-1"
    assert request.url.params.get("action") == "restore"


@respx.mock
def test_hosts_register() -> None:
    route = respx.post(f"{BASE_URL}/v1/hosts").mock(
        return_value=httpx.Response(
            200,
            json={
                "id": "host-1",
                "url": "https://h",
                "state": "active",
                "capacity": {
                    "cpus": 4,
                    "ram_mb": 8192,
                    "storage_gb": 100,
                    "vm_count": 10,
                },
                "allocated": {"cpus": 0, "ram_mb": 0, "storage_gb": 0, "vm_count": 0},
                "last_seen": "2024-01-01T00:00:00Z",
                "created_at": "2024-01-01T00:00:00Z",
                "updated_at": "2024-01-01T00:00:00Z",
            },
        )
    )
    with new_client() as client:
        host = client.hosts.register(
            fuse.RegisterHostRequest(id="host-1", url="https://h")
        )

    request = route.calls.last.request
    assert request.method == "POST"
    assert request.url.path == "/v1/hosts"
    assert host.id == "host-1"
    assert host.capacity.cpus == 4


@respx.mock
def test_hosts_register_decodes_gpu_devices() -> None:
    # gpu_devices is populated on capacity only, never on allocated.
    route = respx.post(f"{BASE_URL}/v1/hosts").mock(
        return_value=httpx.Response(
            200,
            json={
                "id": "gpu-1",
                "url": "https://h",
                "state": "active",
                "backend": "qemu",
                "capacity": {
                    "cpus": 64,
                    "ram_mb": 262144,
                    "storage_gb": 2000,
                    "vm_count": 8,
                    "gpus": 2,
                    "gpu_kind": "a100",
                    "mig_profiles": {"1g.10gb": 14},
                    "gpu_devices": [
                        {
                            "uuid": "GPU-abc",
                            "model": "A100-SXM4-40GB",
                            "pci_bus_id": "0000:07:00.0",
                            "memory_mb": 40960,
                            "driver_version": "550.54.15",
                            "cuda_version": "12.4",
                            "compute_cap": "8.0",
                            "mig_capable": True,
                            "mig_mode": "enabled",
                            "iommu_group": "42",
                        }
                    ],
                },
                "allocated": {"cpus": 0, "ram_mb": 0, "storage_gb": 0, "vm_count": 0},
            },
        )
    )
    with new_client() as client:
        host = client.hosts.register(
            fuse.RegisterHostRequest(
                id="gpu-1",
                url="https://h",
                backend="qemu",
                capacity=fuse.HostCapacity(
                    cpus=64,
                    ram_mb=262144,
                    storage_gb=2000,
                    vm_count=8,
                    gpus=2,
                    gpu_kind="a100",
                    mig_profiles={"1g.10gb": 14},
                ),
            )
        )

    assert route.called
    assert host.capacity.mig_profiles == {"1g.10gb": 14}
    assert len(host.capacity.gpu_devices) == 1
    dev = host.capacity.gpu_devices[0]
    assert dev.uuid == "GPU-abc"
    assert dev.model == "A100-SXM4-40GB"
    assert dev.pci_bus_id == "0000:07:00.0"
    assert dev.memory_mb == 40960
    assert dev.driver_version == "550.54.15"
    assert dev.cuda_version == "12.4"
    assert dev.compute_cap == "8.0"
    assert dev.mig_capable is True
    assert dev.mig_mode == "enabled"
    assert dev.iommu_group == "42"
    assert host.allocated.gpu_devices == []


@respx.mock
def test_hosts_list() -> None:
    route = respx.get(f"{BASE_URL}/v1/hosts").mock(
        return_value=httpx.Response(
            200,
            json={"hosts": [{"id": "host-1", "url": "https://h", "state": "active"}]},
        )
    )
    with new_client() as client:
        hosts = client.hosts.list()

    request = route.calls.last.request
    assert request.method == "GET"
    assert request.url.path == "/v1/hosts"
    assert [h.id for h in hosts] == ["host-1"]


@respx.mock
def test_hosts_cordon() -> None:
    route = respx.post(f"{BASE_URL}/v1/hosts/host-1").mock(
        return_value=httpx.Response(204)
    )
    with new_client() as client:
        client.hosts.cordon("host-1")

    request = route.calls.last.request
    assert request.method == "POST"
    assert request.url.path == "/v1/hosts/host-1"
    assert request.url.params.get("action") == "cordon"


@respx.mock
def test_apikeys_create() -> None:
    route = respx.post(f"{BASE_URL}/v1/api-keys").mock(
        return_value=httpx.Response(
            200,
            json={
                "id": "key-1",
                "label": "ci",
                "created_at": "2024-01-01T00:00:00Z",
                "key": "secret-raw",
            },
        )
    )
    with new_client() as client:
        key = client.api_keys.create("ci")

    request = route.calls.last.request
    assert request.method == "POST"
    assert request.url.path == "/v1/api-keys"
    assert key.key == "secret-raw"
    assert key.id == "key-1"


@respx.mock
def test_apikeys_list() -> None:
    route = respx.get(f"{BASE_URL}/v1/api-keys").mock(
        return_value=httpx.Response(
            200,
            json={
                "api_keys": [
                    {"id": "key-1", "label": "ci", "created_at": "2024-01-01T00:00:00Z"}
                ]
            },
        )
    )
    with new_client() as client:
        keys = client.api_keys.list()

    request = route.calls.last.request
    assert request.method == "GET"
    assert request.url.path == "/v1/api-keys"
    assert [k.id for k in keys] == ["key-1"]


@respx.mock
def test_check_response_not_found() -> None:
    respx.get(f"{BASE_URL}/v1/environments/missing").mock(
        return_value=httpx.Response(
            404, json={"error": {"code": "not_found", "message": "x"}}
        )
    )
    with new_client() as client:
        with pytest.raises(fuse.ApiError) as excinfo:
            client.environments.get("missing")

    err = excinfo.value
    assert fuse.as_api_error(err) is err
    assert err.status == 404
    assert err.code == "not_found"
    assert err.message == "x"
    assert fuse.is_not_found(err)


@respx.mock
def test_events_stream() -> None:
    body = (
        "id: 1\n"
        'data: {"event":"state","vm_id":"v","state":"running",'
        '"updated_at":"2024-01-01T00:00:00Z"}\n'
        "\n"
        'data: {"event":"state","vm_id":"v","state":"destroyed",'
        '"updated_at":"2024-01-01T00:00:01Z"}\n'
        "\n"
    )
    respx.get(f"{BASE_URL}/v1/environments/v/events").mock(
        return_value=httpx.Response(
            200, headers={"Content-Type": "text/event-stream"}, text=body
        )
    )
    with new_client() as client:
        events = list(client.environments.events("v"))

    assert [e.err for e in events] == [None, None]
    assert events[0].state == fuse.STATE_RUNNING
    assert events[1].state == fuse.STATE_DESTROYED
    # terminal state must end the iterator.
    assert len(events) == 2
