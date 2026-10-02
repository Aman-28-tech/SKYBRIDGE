// The v1 console is read-only: no Terraform Apply/Destroy, no cloud
// mutations, no ownership transfer or rollback controls, no arbitrary
// cloud actions. This test scans the console source (excluding this test
// and node_modules) for executable mutation controls — buttons that
// trigger mutations or code that issues non-GET requests. Status display
// copy such as "Terraform Apply status: DISABLED" is allowed (it states
// the safety boundary); only executable controls are forbidden.
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

const ROOT = join(__dirname, "..", "..");
const SKIP_DIRS = new Set(["node_modules", ".next", "coverage"]);

function walk(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    const p = join(dir, entry);
    const st = statSync(p);
    if (st.isDirectory()) {
      if (!SKIP_DIRS.has(entry)) walk(p, out);
    } else if (/\.(tsx?|js)$/.test(entry) && !entry.endsWith("no-mutation.test.ts")) {
      out.push(p);
    }
  }
  return out;
}

describe("NO_MUTATION_CONTROLS", () => {
  it("console source contains no executable mutation controls", () => {
    const files = walk(join(ROOT, "src"));
    const hits: string[] = [];
    for (const f of files) {
      const content = readFileSync(f, "utf8");
      const buttons = content.match(/<button[\s\S]*?<\/button>/gi) || [];
      for (const b of buttons) {
        if (/(apply|destroy|transfer|rollback|quiesce|approve|switch traffic)/i.test(b)) {
          hits.push(`${f}: mutation button: ${b.slice(0, 80)}`);
        }
      }
      // Non-GET HTTP verbs issued by the console client.
      if (/method:\s*"(POST|PUT|PATCH|DELETE)"/.test(content)) {
        hits.push(`${f}: non-GET method`);
      }
      // Cloud SDK imports (would imply real-cloud calls).
      if (/aws-sdk-go|azure-sdk|@aws-sdk|@azure\//.test(content)) {
        hits.push(`${f}: cloud SDK import`);
      }
      // Direct terraform apply/destroy execution.
      if (/terraform"\s*,\s*"(apply|destroy)/.test(content) || /exec[^;]*terraform\s+(apply|destroy)/i.test(content)) {
        hits.push(`${f}: terraform execution`);
      }
    }
    expect(hits).toEqual([]);
  });
});
