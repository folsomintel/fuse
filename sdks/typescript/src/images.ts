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
  async convert(req: ConvertImageRequest, opts: MutatingCallOptions = {}): Promise<Image> {
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
}
