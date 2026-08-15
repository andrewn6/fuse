from __future__ import annotations

import json

import httpx
import pytest
import respx

import fuse

BASE_URL = "https://fuse.test"


def new_client() -> fuse.Client:
    return fuse.Client(BASE_URL, "tok")


@respx.mock
def test_computer_posts_action() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments/vm-1/computer").mock(
        return_value=httpx.Response(200, json={"screenshot": "cGl4ZWxz"})
    )
    with new_client() as client:
        res = client.environments.computer(
            "vm-1", fuse.ComputerAction(action="left_click", coordinate=[100, 200])
        )
    assert route.called
    sent = json.loads(route.calls.last.request.content)
    assert sent == {"action": "left_click", "coordinate": [100, 200]}
    assert res.screenshot == "cGl4ZWxz"


@respx.mock
def test_computer_display() -> None:
    respx.get(f"{BASE_URL}/v1/environments/vm-1/computer").mock(
        return_value=httpx.Response(
            200, json={"display": ":1", "up": True, "width": 1280, "height": 800}
        )
    )
    with new_client() as client:
        res = client.environments.computer_display("vm-1")
    assert res.up is True
    assert (res.width, res.height) == (1280, 800)


def test_computer_validates_before_request() -> None:
    with new_client() as client:
        with pytest.raises(ValueError, match="vm id is required"):
            client.environments.computer("", fuse.ComputerAction(action="screenshot"))
        with pytest.raises(ValueError, match="action is required"):
            client.environments.computer("vm-1", fuse.ComputerAction(action=""))
        with pytest.raises(ValueError, match="vm id is required"):
            client.environments.computer_display("")


@respx.mock
def test_create_carries_desktop() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments").mock(
        return_value=httpx.Response(
            200,
            json={"id": "vm-1", "state": "running", "task_id": "t", "url": "https://x"},
        )
    )
    with new_client() as client:
        client.environments.create(
            fuse.CreateRequest(
                task_id="t", desktop=fuse.DesktopSpec(width=1280, height=800)
            )
        )
    sent = json.loads(route.calls.last.request.content)
    assert sent["desktop"] == {"width": 1280, "height": 800}
