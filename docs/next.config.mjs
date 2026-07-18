import { createMDX } from "fumadocs-mdx/next";

const withMDX = createMDX();

/** @type {import('next').NextConfig} */
const config = {
  reactStrictMode: true,
  // diataxis restructure: use-cases -> guides, concepts moved out of learn
  async redirects() {
    return [
      {
        source: "/docs/learn/use-cases/:slug",
        destination: "/docs/guides/:slug",
        permanent: true,
      },
      {
        source: "/docs/learn/concepts/:slug",
        destination: "/docs/concepts/:slug",
        permanent: true,
      },
    ];
  },
};

export default withMDX(config);

// enable cloudflare bindings in `next dev`
import { initOpenNextCloudflareForDev } from "@opennextjs/cloudflare";
initOpenNextCloudflareForDev();
