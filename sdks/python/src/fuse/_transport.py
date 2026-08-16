from __future__ import annotations

from collections.abc import Iterator
from typing import Any, Callable, Optional

import httpx

from ._core import (
    DEFAULT_TIMEOUT,
    DEFAULT_USER_AGENT,
    clean_params,
    iter_sse_data,
    serialize_body,
)
from ._stream import ComputerStream, open_upgrade
from .errors import REQUEST_ID_HEADER, check_response


class Transport:
    # shared http layer for all services. a separate no-timeout client is used
    # for long-lived sse streams so they are not killed by the request timeout.
    def __init__(
        self,
        base_url: str,
        token: str = "",
        *,
        http_client: Optional[httpx.Client] = None,
        stream_client: Optional[httpx.Client] = None,
        user_agent: str = DEFAULT_USER_AGENT,
        request_id: Optional[Callable[[], str]] = None,
        timeout: float = DEFAULT_TIMEOUT,
    ) -> None:
        self._bearer = token
        self._user_agent = user_agent
        self._request_id = request_id
        # if a custom client is supplied, set its base_url yourself.
        self._owns_http = http_client is None
        self._owns_stream = stream_client is None
        self._http = http_client or httpx.Client(base_url=base_url, timeout=timeout)
        self._stream = stream_client or httpx.Client(base_url=base_url, timeout=None)

    def _headers(self, *, has_body: bool) -> dict[str, str]:
        headers: dict[str, str] = {}
        if has_body:
            headers["Content-Type"] = "application/json"
        if self._bearer:
            headers["Authorization"] = f"Bearer {self._bearer}"
        if self._user_agent:
            headers["User-Agent"] = self._user_agent
        if self._request_id is not None:
            rid = self._request_id()
            if rid:
                headers[REQUEST_ID_HEADER] = rid
        return headers

    def request(
        self,
        method: str,
        path: str,
        *,
        params: Optional[dict[str, Any]] = None,
        body: Any = None,
    ) -> httpx.Response:
        json_body = serialize_body(body)
        resp = self._http.request(
            method,
            path,
            params=clean_params(params),
            json=json_body,
            headers=self._headers(has_body=json_body is not None),
        )
        check_response(resp)
        return resp

    def stream_sse(
        self,
        path: str,
        *,
        params: Optional[dict[str, Any]] = None,
    ) -> Iterator[str]:
        headers = self._headers(has_body=False)
        headers["Accept"] = "text/event-stream"
        with self._stream.stream(
            "GET", path, params=clean_params(params), headers=headers
        ) as resp:
            # on error, read the body so check_response can parse the envelope.
            if resp.status_code >= 300:
                resp.read()
                check_response(resp)
            yield from iter_sse_data(resp.iter_lines())

    def upgrade(self, path: str, proto: str) -> ComputerStream:
        # a raw http/1.1 upgrade over its own socket; httpx cannot carry one
        # (a client never gets the socket back after a response), so this
        # bypasses both clients. see _stream.py.
        return open_upgrade(
            str(self._http.base_url), path, proto, self._headers(has_body=False)
        )

    def close(self) -> None:
        if self._owns_http:
            self._http.close()
        if self._owns_stream:
            self._stream.close()
