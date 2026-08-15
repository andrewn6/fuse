from __future__ import annotations

from collections.abc import Iterator
from urllib.parse import quote

from .._transport import Transport
from ..types import (
    ComputerAction,
    ComputerDisplay,
    ComputerResult,
    CreateRequest,
    EnvironmentInfo,
    EnvironmentPage,
    Event,
    ExecRequest,
    ExecResult,
    ForkOptions,
)
from .events import stream_events

# the server's max page size; list() requests this internally so it walks
# every page in as few round trips as possible.
_MAX_PAGE_LIMIT = 200


class EnvironmentsService:
    def __init__(self, transport: Transport) -> None:
        self._t = transport

    def list(
        self, *, task_id: str = "", state: str = "", host_id: str = "", cursor: str = ""
    ) -> list[EnvironmentInfo]:
        # returns every environment matching the filters, transparently
        # walking every result page. for explicit single-page control (e.g.
        # a cursor from a previous call), use list_page.
        out: list[EnvironmentInfo] = []
        while True:
            page = self.list_page(
                task_id=task_id,
                state=state,
                host_id=host_id,
                limit=_MAX_PAGE_LIMIT,
                cursor=cursor,
            )
            out.extend(page.environments)
            if not page.next_cursor:
                break
            cursor = page.next_cursor
        return out

    def list_page(
        self,
        *,
        task_id: str = "",
        state: str = "",
        host_id: str = "",
        limit: int = 0,
        cursor: str = "",
    ) -> EnvironmentPage:
        # returns one page of environments matching the filters.
        params: dict[str, str] = {"task_id": task_id, "state": state, "host_id": host_id}
        if limit > 0:
            params["limit"] = str(limit)
        if cursor:
            params["cursor"] = cursor
        resp = self._t.request("GET", "/v1/environments", params=params)
        return EnvironmentPage.model_validate(resp.json())

    def get(self, vm_id: str) -> EnvironmentInfo:
        if not vm_id:
            raise ValueError("vm id is required")
        resp = self._t.request("GET", f"/v1/environments/{quote(vm_id, safe='')}")
        return EnvironmentInfo.model_validate(resp.json())

    def create(self, request: CreateRequest) -> EnvironmentInfo:
        resp = self._t.request("POST", "/v1/environments", body=request)
        return EnvironmentInfo.model_validate(resp.json())

    def drain(self, vm_id: str) -> EnvironmentInfo:
        return self._action(vm_id, "drain")

    def fork(
        self, vm_id: str, options: ForkOptions | None = None
    ) -> EnvironmentInfo:
        if not vm_id:
            raise ValueError("vm id is required")
        path = f"/v1/environments/{quote(vm_id, safe='')}"
        resp = self._t.request(
            "POST", path, params={"action": "fork"}, body=options or ForkOptions()
        )
        return EnvironmentInfo.model_validate(resp.json())

    def computer(self, vm_id: str, action: ComputerAction) -> ComputerResult:
        # relays one computer-use action to the environment's desktop and
        # returns the result, usually carrying a base64 png screenshot.
        # requires an environment booted from a desktop image; on any other
        # image the server answers 503 with a reason.
        if not vm_id:
            raise ValueError("vm id is required")
        if not action.action:
            raise ValueError("action is required")
        path = f"/v1/environments/{quote(vm_id, safe='')}/computer"
        resp = self._t.request("POST", path, body=action)
        return ComputerResult.model_validate(resp.json())

    def computer_display(self, vm_id: str) -> ComputerDisplay:
        # reports whether the environment has a live display and at what
        # geometry. up is false with a reason on an image with no desktop.
        if not vm_id:
            raise ValueError("vm id is required")
        path = f"/v1/environments/{quote(vm_id, safe='')}/computer"
        resp = self._t.request("GET", path)
        return ComputerDisplay.model_validate(resp.json())

    def exec(self, vm_id: str, request: ExecRequest) -> ExecResult:
        # runs a command inside a running environment's guest. requires the
        # master token.
        #
        # a non-zero exit_code is returned, not raised: the command ran and
        # failed. an ApiError means the command could not be run at all.
        if not vm_id:
            raise ValueError("vm id is required")
        has_cmd = bool(request.cmd)
        has_shell = bool(request.shell)
        if not has_cmd and not has_shell:
            raise ValueError("one of cmd or shell is required")
        if has_cmd and has_shell:
            raise ValueError("cmd and shell are mutually exclusive")
        path = f"/v1/environments/{quote(vm_id, safe='')}"
        resp = self._t.request("POST", path, params={"action": "exec"}, body=request)
        return ExecResult.model_validate(resp.json())

    def rotate_token(self, vm_id: str) -> None:
        if not vm_id:
            raise ValueError("vm id is required")
        path = f"/v1/environments/{quote(vm_id, safe='')}"
        self._t.request("POST", path, params={"action": "rotate-token"})

    def destroy(self, vm_id: str) -> None:
        if not vm_id:
            raise ValueError("vm id is required")
        self._t.request("DELETE", f"/v1/environments/{quote(vm_id, safe='')}")

    def events(self, vm_id: str) -> Iterator[Event]:
        # opens the sse stream and yields Event values. the iterator ends
        # cleanly on eof, after a terminal-state event, or after a final
        # Event whose err is set on a stream-level failure.
        return stream_events(self._t, vm_id)

    def _action(self, vm_id: str, action: str) -> EnvironmentInfo:
        if not vm_id:
            raise ValueError("vm id is required")
        if not action:
            raise ValueError("action is required")
        path = f"/v1/environments/{quote(vm_id, safe='')}"
        resp = self._t.request("POST", path, params={"action": action})
        return EnvironmentInfo.model_validate(resp.json())
