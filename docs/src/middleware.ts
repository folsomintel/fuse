import { NextResponse } from "next/server";

// pass-through middleware.
//
// next 16 emits a middleware trace file (middleware.js.nft.json) even when no
// middleware exists, but does not emit the matching middleware.js in the
// standalone output. @opennextjs/cloudflare's copyTracedFiles then enters its
// middleware-copy branch on the trace's presence and throws because
// server/middleware.js is missing. defining a real (no-op) middleware forces
// next to emit middleware.js, satisfying the copy. remove once opennext gates
// on the middleware manifest instead of the trace file.
export function middleware() {
  return NextResponse.next();
}

// match nothing: this middleware never actually runs at request time.
export const config = {
  matcher: [],
};
