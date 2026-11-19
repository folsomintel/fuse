import {
  idempotencyKey,
  type CallOptions,
  type MutatingCallOptions,
  type Transport,
} from "./transport.js";
import type { ConvertImageRequest, Image } from "./types.js";
import { requireArg } from "./validate.js";

/** ImagesService converts oci images (a Fusefile's `from:`) into bootable
 * rootfs images. */
export class ImagesService {
  constructor(private readonly t: Transport) {}

  /**
   * Start converting req.ref and return the image at once, usually still
   * converting. Poll get until it is ready. Converting a ref that is already
   * converting or ready returns that image; one that failed is retried.
   * The same idempotency key makes a retry attach to the first convert.
   */
  async convert(
    req: ConvertImageRequest,
    opts: MutatingCallOptions = {},
  ): Promise<Image> {
    requireArg(req.ref, "image ref");
    return this.t.json<Image>("POST", "/v1/images", {
      body: req,
      signal: opts.signal,
      idempotencyKey: idempotencyKey(opts),
    });
  }

  /** Report an image's conversion state by digest. */
  async get(digest: string, opts: CallOptions = {}): Promise<Image> {
    requireArg(digest, "image digest");
    return this.t.json<Image>("GET", `/v1/images/${encodeURIComponent(digest)}`, {
      signal: opts.signal,
    });
  }

  /**
   * Report one host's record of an image. get answers with the best record
   * of any host, so to wait on a convert pinned to a host (hostId, region or
   * labels), poll getOnHost with the host_id the convert returned: another
   * host's ready record says nothing about the pinned one.
   */
  async getOnHost(
    hostId: string,
    digest: string,
    opts: CallOptions = {},
  ): Promise<Image> {
    requireArg(hostId, "host id");
    requireArg(digest, "image digest");
    return this.t.json<Image>(
      "GET",
      `/v1/hosts/${encodeURIComponent(hostId)}/images/${encodeURIComponent(digest)}`,
      { signal: opts.signal },
    );
  }
}
