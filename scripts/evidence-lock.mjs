#!/usr/bin/env node
// Writes evidence.lock.json for the tagged commit. Requires: GITHUB_TOKEN, GITHUB_REPOSITORY,
// TAG, and the `yaml` package. Waits (bounded) for each named CI workflow to finish at the commit.
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";
import { parse } from "yaml";

const repo = process.env.GITHUB_REPOSITORY;
const tag = process.env.TAG;
const token = process.env.GITHUB_TOKEN;
if (!/^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(tag ?? "")) throw new Error(`not a SemVer tag: ${tag}`);
const sha = execFileSync("git", ["rev-list", "-n", "1", tag], { encoding: "utf8" }).trim();
const manifest = parse(readFileSync("evidence/evidence.yaml", "utf8"), { strict: true, uniqueKeys: true });
if (manifest.repo !== repo) throw new Error(`manifest repo ${manifest.repo} != ${repo}`);

// Promotion rule (documented in evidence/evidence.yaml): MEASURED once the signed summary at this
// commit covers at least 14 days of both origin and probe data. The site's audit re-checks it.
const SUMMARY = "reports/summary.json";
const MIN_DAYS = 14;
try {
  const s = JSON.parse(execFileSync("git", ["show", `${sha}:${SUMMARY}`], { encoding: "utf8" }));
  execFileSync("git", ["cat-file", "-e", `${sha}:${SUMMARY}.sigstore.json`]);
  const origin = s.origin?.days_with_data ?? 0;
  const probe = s.probe?.days_with_data ?? 0;
  const item = manifest.items.find((i) => i.id === "opsledger-measurement");
  if (item && origin >= MIN_DAYS && probe >= MIN_DAYS) {
    item.verification = "measured";
    item.ops_reports = [SUMMARY];
    console.log(`promoted opsledger-measurement to MEASURED (${origin} origin days, ${probe} probe days)`);
  } else {
    console.log(`opsledger-measurement stays TESTED (${origin} origin days, ${probe} probe days; ${MIN_DAYS} needed)`);
  }
} catch {
  console.log("no signed summary at this commit; claims stay TESTED");
}

const tree = new Set(execFileSync("git", ["ls-tree", "-r", "-t", "--name-only", sha], { encoding: "utf8" }).split("\n"));
const paths = new Set();
for (const i of manifest.items) {
  for (const k of ["implementation", "benchmarks", "decisions", "documents", "ops_reports"]) for (const p of i[k] ?? []) paths.add(p);
  for (const p of i.ops_reports ?? []) paths.add(`${p}.sigstore.json`);
  for (const t of i.tests ?? []) paths.add(t.path);
}
const files = [...paths].sort().map((p) => {
  const dir = p.endsWith("/");
  if (!tree.has(dir ? p.slice(0, -1) : p)) throw new Error(`${p} does not exist at ${sha}`);
  if (dir) return { path: p, kind: "dir" };
  const bytes = execFileSync("git", ["show", `${sha}:${p}`]);
  return { path: p, kind: "file", sha256: createHash("sha256").update(bytes).digest("hex"), bytes: bytes.length };
});

const api = async (path) => {
  const r = await fetch(`https://api.github.com/repos/${repo}${path}`, {
    headers: { Authorization: `Bearer ${token}`, Accept: "application/vnd.github+json" },
  });
  if (!r.ok) throw new Error(`GitHub API ${r.status} for ${path}`);
  return r.json();
};
const ciRuns = [];
for (const wf of [...new Set(manifest.items.map((i) => i.ci_workflow).filter(Boolean))].sort()) {
  let run;
  for (let attempt = 0; attempt < 60 && !run; attempt++) {
    const d = await api(`/actions/workflows/${encodeURIComponent(wf)}/runs?head_sha=${sha}&per_page=20`);
    run = d.workflow_runs.find((r) => r.status === "completed");
    if (!run) await new Promise((r) => setTimeout(r, 30_000));
  }
  if (!run) throw new Error(`no completed ${wf} run at ${sha}`);
  ciRuns.push({
    workflow: wf,
    run_id: run.id,
    url: run.html_url,
    conclusion: run.conclusion ?? "neutral",
    head_sha: sha,
    completed_at: run.updated_at,
  });
}

const commit = await api(`/commits/${sha}`);
const lock = {
  schema: "https://aliakbari.dev/schemas/evidence-lock-v1.json",
  repo,
  tag,
  verified_commit: sha,
  committed_at: commit.commit.committer.date,
  generator: "release-workflow",
  ci_runs: ciRuns,
  files,
  items: manifest.items,
};
writeFileSync("evidence.lock.json", `${JSON.stringify(lock, null, 2)}\n`);
console.log(
  `evidence.lock.json: ${manifest.items.length} items, ${files.length} paths, CI: ${ciRuns.map((r) => `${r.workflow}=${r.conclusion}`).join(" ")}`,
);
