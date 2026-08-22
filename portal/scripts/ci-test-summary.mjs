// Appends a coverage table + per-file test breakdown to the GitHub Actions job
// summary, complementing Vitest's built-in github-actions reporter (which gives
// the pass/fail counts + inline failure annotations). Reads the JSON that
// `npm run test:ci` writes. Best-effort: a report glitch must never fail CI, so
// missing/unparseable inputs degrade to a note and exit 0.
//
// Usage: node scripts/ci-test-summary.mjs
//   env GITHUB_STEP_SUMMARY: file to append to (falls back to stdout locally)
import { readFileSync, appendFileSync } from "node:fs";

const COVERAGE = "coverage/coverage-summary.json";
const RESULTS = "vitest-results.json";

const out = [];
const emit = (s) => out.push(s);

// relPath trims an absolute path down to its "src/..." tail so the report reads
// the same on any machine/runner.
const relPath = (p) => {
  const i = p.indexOf("/src/");
  return i >= 0 ? p.slice(i + 1) : p;
};

const readJSON = (path) => {
  try {
    return JSON.parse(readFileSync(path, "utf8"));
  } catch (e) {
    emit(`> ⚠️ could not read \`${path}\`: ${e.message}\n`);
    return null;
  }
};

const pct = (n) => `${n.toFixed(1)}%`;
const ms = (n) => `${Math.round(n)}ms`;

// Each section is independently guarded: a malformed input or an unexpected
// schema (e.g. a future Vitest bump dropping a metric key) degrades to a note
// and still lets the other section + the final write proceed. The script never
// throws, so the CI step can never fail the job over a report glitch.
const section = (name, fn) => {
  try {
    fn();
  } catch (e) {
    emit(`> ⚠️ ${name} report error: ${e.message}\n`);
  }
};

// ── Coverage ──────────────────────────────────────────────────────────────
section("coverage", () => {
  const cov = readJSON(COVERAGE);
  if (!cov?.total) return;
  const t = cov.total;
  emit("## 📊 Portal coverage\n");
  emit("| Metric | Coverage | Covered / Total |");
  emit("|---|---:|---:|");
  for (const k of ["lines", "statements", "functions", "branches"]) {
    const m = t[k];
    emit(`| ${k[0].toUpperCase() + k.slice(1)} | ${pct(m.pct)} | ${m.covered} / ${m.total} |`);
  }
  emit("");

  const files = Object.entries(cov)
    .filter(([k]) => k !== "total")
    .sort(([a], [b]) => a.localeCompare(b));
  if (files.length) {
    emit("<details><summary>Per-file coverage</summary>\n");
    emit("| File | Lines | Statements | Funcs | Branches |");
    emit("|---|---:|---:|---:|---:|");
    for (const [path, m] of files) {
      emit(`| ${relPath(path)} | ${pct(m.lines.pct)} | ${pct(m.statements.pct)} | ${pct(m.functions.pct)} | ${pct(m.branches.pct)} |`);
    }
    emit("\n</details>\n");
  }
});

// ── Per-file test breakdown ────────────────────────────────────────────────
section("test breakdown", () => {
  const res = readJSON(RESULTS);
  if (!res?.testResults) return;
  const rows = res.testResults
    .map((f) => {
      const a = f.assertionResults ?? [];
      return {
        file: relPath(f.name),
        total: a.length,
        passed: a.filter((r) => r.status === "passed").length,
        failed: a.filter((r) => r.status === "failed").length,
        duration: a.reduce((s, r) => s + (r.duration ?? 0), 0),
      };
    })
    .sort((x, y) => y.duration - x.duration); // slowest first

  const totals = rows.reduce(
    (acc, r) => ({
      total: acc.total + r.total,
      passed: acc.passed + r.passed,
      failed: acc.failed + r.failed,
    }),
    { total: 0, passed: 0, failed: 0 },
  );

  emit("## 🧪 Portal test breakdown\n");
  const failNote = totals.failed ? ` · **${totals.failed} failed**` : "";
  emit(`**${totals.passed} passed** of ${totals.total} across ${rows.length} files${failNote}.\n`);
  emit("| File | Tests | Passed | Failed | Duration |");
  emit("|---|---:|---:|---:|---:|");
  for (const r of rows) {
    emit(`| ${r.file} | ${r.total} | ${r.passed} | ${r.failed || ""} | ${ms(r.duration)} |`);
  }
  emit("");
});

if (out.length === 0) emit("> ⚠️ no portal test report data found.\n");

const body = out.join("\n") + "\n";
const target = process.env.GITHUB_STEP_SUMMARY;
if (target) {
  appendFileSync(target, body);
} else {
  process.stdout.write(body);
}
