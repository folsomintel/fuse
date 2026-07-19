import { defineMeta } from "blume";

// this folder holds no authored pages: the openapi reference generated from
// internal/api/openapi.yaml mounts at /reference/api, and this meta only
// relabels its sidebar group.
export default defineMeta({
  title: "API",
  icon: "webhook",
});
