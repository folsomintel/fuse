import { defineConfig } from "blume";

// pages moved into diataxis quadrants; keep the old urls alive.
const movedGuides = [
  "agent-sandboxes",
  "ci-runners",
  "gpu-workloads",
  "interactive-debugging",
];

const movedConcepts = [
  "fusefile",
  "providers",
  "hosts",
  "environments",
  "snapshots",
  "artifacts",
  "scheduling",
  "reconcile",
  "state-and-recovery",
];

export default defineConfig({
  title: "Fuse",
  description:
    "A control plane for provisioning sandboxed compute environments on your own hosts.",
  // content lives in content/docs, so the "docs" folder becomes the /docs url
  // segment and every existing link keeps working unchanged.
  content: {
    root: "content",
  },
  github: {
    owner: "folsomintel",
    repo: "fuse",
    branch: "main",
    dir: "docs",
  },
  redirects: [
    // no page is authored at / or /docs, so both 404 without these.
    // temporary (302) so a future real landing page isn't shadowed by
    // browser-cached permanent redirects.
    { from: "/", to: "/docs/learn", status: 302 as const },
    { from: "/docs", to: "/docs/learn", status: 302 as const },
    ...movedGuides.map((slug) => ({
      from: `/docs/learn/use-cases/${slug}`,
      to: `/docs/guides/${slug}`,
      status: 308 as const,
    })),
    ...movedConcepts.map((slug) => ({
      from: `/docs/learn/concepts/${slug}`,
      to: `/docs/concepts/${slug}`,
      status: 308 as const,
    })),
  ],
});
