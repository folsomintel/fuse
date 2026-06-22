import { FuseError } from "./errors.js";

/**
 * requireArg throws a FuseError when a required path argument is missing,
 * mirroring the Go SDK's "<thing> is required" guards.
 */
export function requireArg(value: string | undefined | null, name: string): string {
  if (!value) {
    throw new FuseError(`${name} is required`);
  }
  return value;
}
