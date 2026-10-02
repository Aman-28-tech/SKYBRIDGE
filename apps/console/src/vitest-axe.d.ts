import type { AxeResults } from "axe-core";

// Runtime registration happens via `expect.extend(matchers)` in
// a11y.test.tsx; this declaration only teaches tsc the matcher name so
// `tsc --noEmit` (and the acceptance gate) stays green.
declare module "vitest" {
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  interface Assertion<T = AxeResults> {
    toHaveNoViolations(): void;
  }
  interface AsymmetricMatchersContaining {
    toHaveNoViolations(): void;
  }
}
