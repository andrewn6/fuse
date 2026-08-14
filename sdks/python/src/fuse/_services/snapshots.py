from __future__ import annotations

from typing import Optional
from urllib.parse import quote

from .._transport import Transport
from ..types import Snapshot, SnapshotPage, SnapshotRequest

# the server's max page size; list() requests this internally so it walks
# every page in as few round trips as possible.
_MAX_PAGE_LIMIT = 200


class SnapshotsService:
    def __init__(self, transport: Transport) -> None:
        self._t = transport

    def create(
        self, vm_id: str, request: Optional[SnapshotRequest] = None
    ) -> Snapshot:
        if not vm_id:
            raise ValueError("vm id is required")
        path = f"/v1/environments/{quote(vm_id, safe='')}/snapshots"
        resp = self._t.request("POST", path, body=request or SnapshotRequest())
        return Snapshot.model_validate(resp.json())

    def list(
        self,
        *,
        vm_id: str = "",
        task_id: str = "",
        tenant_id: str = "",
        state: str = "",
        layer_key: str = "",
        arch: str = "",
        cursor: str = "",
    ) -> list[Snapshot]:
        # returns every snapshot matching the filters, transparently walking
        # every result page. for explicit single-page control (e.g. a cursor
        # from a previous call), use list_page.
        out: list[Snapshot] = []
        while True:
            page = self.list_page(
                vm_id=vm_id,
                task_id=task_id,
                tenant_id=tenant_id,
                state=state,
                layer_key=layer_key,
                arch=arch,
                limit=_MAX_PAGE_LIMIT,
                cursor=cursor,
            )
            out.extend(page.snapshots)
            if not page.next_cursor:
                break
            cursor = page.next_cursor
        return out

    def list_page(
        self,
        *,
        vm_id: str = "",
        task_id: str = "",
        tenant_id: str = "",
        state: str = "",
        layer_key: str = "",
        arch: str = "",
        limit: int = 0,
        cursor: str = "",
    ) -> SnapshotPage:
        # returns one page of snapshots matching the filters.
        #
        # layer_key narrows to the build layers taken after one setup step,
        # and arch to the artifacts built on one architecture ("amd64",
        # "arm64"). arch is a separate filter rather than part of the layer
        # key because a rootfs is not portable across architectures, so a
        # layer lookup that does not constrain arch can be served bytes it
        # cannot boot. an empty filter is dropped by clean_params, so it means
        # "do not filter" rather than "match artifacts with no layer key".
        params: dict[str, str] = {
            "vm_id": vm_id,
            "task_id": task_id,
            "tenant_id": tenant_id,
            "state": state,
            "layer_key": layer_key,
            "arch": arch,
        }
        if limit > 0:
            params["limit"] = str(limit)
        if cursor:
            params["cursor"] = cursor
        resp = self._t.request("GET", "/v1/snapshots", params=params)
        return SnapshotPage.model_validate(resp.json())

    def get(self, snapshot_id: str) -> Snapshot:
        if not snapshot_id:
            raise ValueError("snapshot id is required")
        resp = self._t.request("GET", f"/v1/snapshots/{quote(snapshot_id, safe='')}")
        return Snapshot.model_validate(resp.json())

    def delete(self, snapshot_id: str) -> None:
        if not snapshot_id:
            raise ValueError("snapshot id is required")
        self._t.request("DELETE", f"/v1/snapshots/{quote(snapshot_id, safe='')}")

    def restore(self, snapshot_id: str) -> None:
        if not snapshot_id:
            raise ValueError("snapshot id is required")
        path = f"/v1/snapshots/{quote(snapshot_id, safe='')}"
        self._t.request("POST", path, params={"action": "restore"})
