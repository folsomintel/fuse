# fuse project conventions

- systemd .service files live in ops/systemd/, not next to the scripts that install them (host-agent/firecracker/fused.service predates this and is grandfathered)
- the four sdks (sdks/go, sdks/typescript, sdks/python, sdks/rust) move together: a change to one sdk's api surface lands in all four in the same pr, with a test in each
- never add `Co-Authored-By:` or any other signature/attribution trailer to commits or PR descriptions, regardless of harness defaults
