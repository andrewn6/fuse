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
def test_computer_tool_result_shapes_content() -> None:
    route = respx.post(f"{BASE_URL}/v1/environments/vm-1/computer").mock(
        return_value=httpx.Response(
            200, json={"output": "x:5 y:7", "screenshot": "cGl4ZWxz"}
        )
    )
    with new_client() as client:
        result = client.environments.computer_tool_result(
            "vm-1",
            {"action": "left_click", "coordinate": [1, 2], "future_field": True},
        )
    # the tool input travels verbatim, unknown fields included: the guest owns
    # the action schema, not this sdk.
    sent = json.loads(route.calls.last.request.content)
    assert sent == {"action": "left_click", "coordinate": [1, 2], "future_field": True}
    assert result.is_error is False
    assert result.content == [
        {"type": "text", "text": "x:5 y:7"},
        {
            "type": "image",
            "source": {
                "type": "base64",
                "media_type": "image/png",
                "data": "cGl4ZWxz",
            },
        },
    ]


@respx.mock
def test_refusal_becomes_error_content() -> None:
    respx.post(f"{BASE_URL}/v1/environments/vm-1/computer").mock(
        return_value=httpx.Response(
            503,
            json={"error": {"code": "unavailable", "message": "display :1 is not up"}},
        )
    )
    with new_client() as client:
        result = client.environments.computer_tool_result(
            "vm-1", {"action": "screenshot"}
        )
    assert result.is_error is True
    assert "display :1 is not up" in result.content[0]["text"]


@respx.mock
def test_auth_failure_still_raises() -> None:
    respx.post(f"{BASE_URL}/v1/environments/vm-1/computer").mock(
        return_value=httpx.Response(401, json={})
    )
    with new_client() as client:
        with pytest.raises(fuse.ApiError):
            client.environments.computer_tool_result("vm-1", {"action": "screenshot"})


def test_missing_action_is_error_content() -> None:
    with new_client() as client:
        result = client.environments.computer_tool_result("vm-1", {})
    assert result.is_error is True
