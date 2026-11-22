"""tile view for the fork demo: one tile per copy, host memory alongside.

two renderings of the same rows: an ansi grid redrawn in the terminal, and a
static tiles.html the driver rewrites next to its results (it reloads itself
every 2s until the run ends). stdlib only.
"""

from __future__ import annotations

import html
import os
import shutil
from dataclasses import dataclass
from typing import Optional

# status -> (ansi background, css color). copies go forking -> up -> checking
# -> wrecking -> wrecked; failed is any step that raised or a job that did not
# take. main is copy 0, which only ever reads.
COLORS = {
    "forking": ("100", "#555"),
    "up": ("44", "#2a5db0"),
    "checking": ("43", "#b08a2a"),
    "wrecking": ("43", "#b08a2a"),
    "wrecked": ("41", "#b03a2a"),
    "failed": ("45", "#8a2ab0"),
    "main": ("42", "#2a8a4a"),
}
TILE_WIDTH = 24


@dataclass
class Tile:
    index: int
    status: str
    job: str
    job_short: str
    fork_s: Optional[float]
    first_query_s: Optional[float]
    query_ms: Optional[float]
    error: str = ""


def _secs(v: Optional[float]) -> str:
    return "-" if v is None else f"{v:.1f}s"


def _ms(v: Optional[float]) -> str:
    if v is None:
        return "-"
    return f"{v:.0f}ms" if v < 10000 else f"{v / 1000:.0f}s"


def header(tiles: list[Tile], mem: dict[str, Optional[int]], elapsed_s: float) -> str:
    n = len(tiles) - 1  # copy 0 is main
    up = sum(1 for t in tiles[1:] if t.first_query_s is not None)
    wrecked = sum(1 for t in tiles if t.status == "wrecked")
    failed = sum(1 for t in tiles if t.status == "failed")
    total, avail = mem.get("mem_total_kb"), mem.get("mem_available_kb")
    if total and avail is not None:
        host = f"host mem {(total - avail) / 1048576:.1f}/{total / 1048576:.1f} GiB"
    else:
        host = "host mem n/a"
    fc = mem.get("fc_count")
    return (
        f"fork-postgres  copies {up}/{n} up  wrecked {wrecked}  failed {failed}  "
        f"{host}  vms {fc if fc is not None else 'n/a'}  t+{elapsed_s:.0f}s"
    )


def render_terminal(tiles: list[Tile], head: str) -> str:
    cols = max(1, shutil.get_terminal_size((120, 40)).columns // (TILE_WIDTH + 1))
    lines = [head, "tile: copy, fork time, first heavy query, job", ""]
    row: list[str] = []
    for t in tiles:
        bg = COLORS.get(t.status, COLORS["forking"])[0]
        text = f"{t.index:03d} {_secs(t.fork_s):>5} {_ms(t.query_ms):>5} {t.job_short:<8}"
        row.append(f"\x1b[{bg};97m{text[:TILE_WIDTH]:<{TILE_WIDTH}}\x1b[0m")
        if len(row) == cols:
            lines.append(" ".join(row))
            row = []
    if row:
        lines.append(" ".join(row))
    # home the cursor and clear below, so the grid redraws in place
    return "\x1b[H\x1b[J" + "\n".join(lines) + "\n"


def write_html(path: str, tiles: list[Tile], head: str, final: bool) -> None:
    cells = []
    for t in tiles:
        color = COLORS.get(t.status, COLORS["forking"])[1]
        name = "main (untouched)" if t.index == 0 else t.job
        title = html.escape(t.error, quote=True)
        cells.append(
            f'<div class="t" style="background:{color}" title="{title}">'
            f"<b>{t.index:03d}</b> {html.escape(name)}<br>"
            f"fork {_secs(t.fork_s)} &middot; first query {_secs(t.first_query_s)}<br>"
            f"query {_ms(t.query_ms)} &middot; {html.escape(t.status)}</div>"
        )
    refresh = "" if final else '<meta http-equiv="refresh" content="2">'
    page = f"""<!doctype html>
<html><head><meta charset="utf-8">{refresh}
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>fork-postgres tiles</title>
<style>
body {{ background:#111; color:#eee; font:13px/1.4 ui-monospace, monospace; margin:16px; }}
.g {{ display:grid; grid-template-columns:repeat(auto-fill, minmax(190px, 1fr)); gap:6px; }}
.t {{ padding:6px 8px; border-radius:4px; color:#fff; }}
</style></head>
<body><p>{html.escape(head)}</p><div class="g">{"".join(cells)}</div></body></html>
"""
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        f.write(page)
    os.replace(tmp, path)
