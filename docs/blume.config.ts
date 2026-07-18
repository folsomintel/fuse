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
