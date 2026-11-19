from __future__ import annotations

import uuid
from typing import Optional
from urllib.parse import quote

from .._transport import Transport
from ..types import ConvertImageRequest, Image
from .operations import IDEMPOTENCY_KEY_HEADER


class ImagesService:
    # converts oci images (a fusefile's `from:`) into bootable rootfs images.
    def __init__(self, transport: Transport) -> None:
        self._t = transport

    def convert(
        self, request: ConvertImageRequest, *, idempotency_key: Optional[str] = None
    ) -> Image:
        # starts converting request.ref and returns the image at once, usually
        # still converting. poll get until it is ready. converting a ref that
        # is already converting or ready returns that image; a failed one is
        # retried. without a key the call sends a fresh random one; pass your
        # own so a retry attaches to the first convert.
        if not request.ref:
            raise ValueError("image ref is required")
        key = idempotency_key or str(uuid.uuid4())
        resp = self._t.request(
            "POST",
            "/v1/images",
            body=request,
            headers={IDEMPOTENCY_KEY_HEADER: key},
        )
        return Image.model_validate(resp.json())

    def get(self, digest: str) -> Image:
        # reports an image's conversion state by digest.
        if not digest:
            raise ValueError("image digest is required")
        resp = self._t.request("GET", f"/v1/images/{quote(digest, safe='')}")
        return Image.model_validate(resp.json())

    def get_on_host(self, host_id: str, digest: str) -> Image:
        # reports one host's record of an image. get answers with the best
        # record of any host, so to wait on a convert pinned to a host
        # (host_id, region or labels), poll get_on_host with the host_id the
        # convert returned: another host's ready record says nothing about
        # the pinned one.
        if not host_id:
            raise ValueError("host id is required")
        if not digest:
            raise ValueError("image digest is required")
        resp = self._t.request(
            "GET",
            f"/v1/hosts/{quote(host_id, safe='')}/images/{quote(digest, safe='')}",
        )
        return Image.model_validate(resp.json())
