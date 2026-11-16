from __future__ import annotations

from urllib.parse import quote

from .._transport import Transport
from ..types import ConvertImageRequest, Image


class ImagesService:
    # converts oci images (a fusefile's `from:`) into bootable rootfs images.
    def __init__(self, transport: Transport) -> None:
        self._t = transport

    def convert(self, request: ConvertImageRequest) -> Image:
        # starts converting request.ref and returns the image at once, usually
        # still converting. poll get until it is ready. converting a ref that
        # is already converting or ready returns that image; a failed one is
        # retried.
        if not request.ref:
            raise ValueError("image ref is required")
        resp = self._t.request("POST", "/v1/images", body=request)
        return Image.model_validate(resp.json())

    def get(self, digest: str) -> Image:
        # reports an image's conversion state by digest.
        if not digest:
            raise ValueError("image digest is required")
        resp = self._t.request("GET", f"/v1/images/{quote(digest, safe='')}")
        return Image.model_validate(resp.json())
