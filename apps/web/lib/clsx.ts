/** Tiny classnames joiner — avoids pulling in a dependency. */
export function clsx(...parts: Array<string | false | null | undefined>): string {
  return parts.filter(Boolean).join(" ");
}
