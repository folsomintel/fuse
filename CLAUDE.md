# fuse project conventions

- systemd .service files live in ops/systemd/, not next to the scripts that install them (host-agent/firecracker/fused.service predates this and is grandfathered)
- the four sdks (sdks/go, sdks/typescript, sdks/python, sdks/rust) move together: a change to one sdk's api surface lands in all four in the same pr, with a test in each
- every pr links the issue it resolves with a closing keyword in the body ("closes #N"), so it shows under the issue's development section
- issues are short: title is `area: what to do` (lowercase, e.g. `migrate: pick the checkpoint standby by capacity`); body is an optional one-line context/dependency note ("Part of #N", "Depends on #N"), then `## files` (paths with the function to touch, no line numbers) and `## do` (bullets of the work). epics list sub-issues as a checklist. no background essays, competitor comparisons or long acceptance sections
- greptile review comments never get a reply, reaction or thread resolve: fixes go through `/greptile`, which shows each diff and pushes only what the user approves
