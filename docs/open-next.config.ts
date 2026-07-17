import { defineCloudflareConfig } from "@opennextjs/cloudflare";

// buildCommand defaults to the package manager's `build` script (e.g.
// `bun run build`). Since our `build` script is `opennextjs-cloudflare build`
// (so the Cloudflare build step produces the .open-next output), leaving the
// default would make opennext re-invoke `bun run build` and recurse forever.
// Pin it to the raw next build instead.
export default {
  ...defineCloudflareConfig(),
  buildCommand: "next build",
};
