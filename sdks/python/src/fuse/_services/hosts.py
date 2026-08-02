from __future__ import annotations

from urllib.parse import quote

from .._transport import Transport
from ..types import Host, HostPage, RegisterHostRequest

# the server's max page size; list() requests this internally so it walks
# every page in as few round trips as possible.
_MAX_PAGE_LIMIT = 200


class HostsService:
    def __init__(self, transport: Transport) -> None:
        self._t = transport

    def register(self, request: RegisterHostRequest) -> Host:
        resp = self._t.request("POST", "/v1/hosts", body=request)
        return Host.model_validate(resp.json())

    def list(self) -> list[Host]:
        # returns every registered host, transparently walking every result
        # page. for explicit single-page control (e.g. a cursor from a
        # previous call), use list_page.
        out: list[Host] = []
        cursor = ""
        while True:
            page = self.list_page(limit=_MAX_PAGE_LIMIT, cursor=cursor)
            out.extend(page.hosts)
            if not page.next_cursor:
                break
            cursor = page.next_cursor
        return out

    def list_page(self, *, limit: int = 0, cursor: str = "") -> HostPage:
        # returns one page of registered hosts.
        params: dict[str, str] = {}
        if limit > 0:
            params["limit"] = str(limit)
        if cursor:
            params["cursor"] = cursor
        resp = self._t.request("GET", "/v1/hosts", params=params)
        return HostPage.model_validate(resp.json())

    def get(self, host_id: str) -> Host:
        if not host_id:
            raise ValueError("host id is required")
        resp = self._t.request("GET", f"/v1/hosts/{quote(host_id, safe='')}")
        return Host.model_validate(resp.json())

    def cordon(self, host_id: str) -> None:
        self._action(host_id, "cordon")

    def uncordon(self, host_id: str) -> None:
        self._action(host_id, "uncordon")

    def deregister(self, host_id: str) -> None:
        if not host_id:
            raise ValueError("host id is required")
        self._t.request("DELETE", f"/v1/hosts/{quote(host_id, safe='')}")

    def _action(self, host_id: str, action: str) -> None:
        if not host_id:
            raise ValueError("host id is required")
        if not action:
            raise ValueError("action is required")
        path = f"/v1/hosts/{quote(host_id, safe='')}"
        self._t.request("POST", path, params={"action": action})
