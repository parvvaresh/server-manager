#!/usr/bin/env node
// Assemble the npm packages for a release.
//
//   node scripts/npm-stage.mjs VERSION DIST_DIR OUT_DIR
//
// DIST_DIR holds the cross compiled binaries named sshm-<goos>-<goarch>.
// OUT_DIR receives one directory per package, ready for `npm publish`:
// a platform package for each binary, and the main package that depends on
// all of them as optionalDependencies.
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const [version, distDir, outDir] = process.argv.slice(2);

if (!version || !distDir || !outDir) {
  console.error("usage: npm-stage.mjs VERSION DIST_DIR OUT_DIR");
  process.exit(2);
}
if (!/^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(version)) {
  console.error(`npm-stage: "${version}" is not a semver version`);
  process.exit(2);
}

// Go's names on the left, Node's on the right.
const TARGETS = [
  { goos: "darwin", goarch: "arm64", os: "darwin", cpu: "arm64" },
  { goos: "darwin", goarch: "amd64", os: "darwin", cpu: "x64" },
  { goos: "linux", goarch: "arm64", os: "linux", cpu: "arm64" },
  { goos: "linux", goarch: "amd64", os: "linux", cpu: "x64" },
];

const main = JSON.parse(fs.readFileSync(path.join(root, "npm/sshm/package.json"), "utf8"));
const shared = {
  homepage: main.homepage,
  bugs: main.bugs,
  repository: main.repository,
};

fs.rmSync(outDir, { recursive: true, force: true });

const optionalDependencies = {};
for (const t of TARGETS) {
  const name = `${main.name}-${t.os}-${t.cpu}`;
  const src = path.join(distDir, `sshm-${t.goos}-${t.goarch}`);
  if (!fs.existsSync(src)) {
    console.error(`npm-stage: missing binary ${src}`);
    process.exit(1);
  }

  const dir = path.join(outDir, name.split("/").pop());
  fs.mkdirSync(path.join(dir, "bin"), { recursive: true });
  fs.copyFileSync(src, path.join(dir, "bin/sshm"));
  fs.chmodSync(path.join(dir, "bin/sshm"), 0o755);

  writeJSON(path.join(dir, "package.json"), {
    name,
    version,
    description: `The ${t.os} ${t.cpu} binary for ${main.name}`,
    ...shared,
    files: ["bin"],
    os: [t.os],
    cpu: [t.cpu],
    preferUnplugged: true,
  });
  fs.writeFileSync(
    path.join(dir, "README.md"),
    `# ${name}\n\nThe ${t.os} ${t.cpu} binary for [${main.name}](https://www.npmjs.com/package/${main.name}). ` +
      `Install that package instead of this one.\n`
  );
  optionalDependencies[name] = version;
  console.log(`staged ${name}@${version}`);
}

const mainDir = path.join(outDir, main.name.split("/").pop());
fs.cpSync(path.join(root, "npm/sshm"), mainDir, { recursive: true });
fs.chmodSync(path.join(mainDir, "bin/sshm.js"), 0o755);
// npmjs.com cannot resolve the README's relative image paths, so point them
// at the files in the release tag, which never moves.
const raw = `https://raw.githubusercontent.com/parvvaresh/server-manager/v${version}/`;
const readme = fs
  .readFileSync(path.join(root, "README.md"), "utf8")
  .replace(/(src=")(docs\/)/g, `$1${raw}$2`);
fs.writeFileSync(path.join(mainDir, "README.md"), readme);
writeJSON(path.join(mainDir, "package.json"), { ...main, version, optionalDependencies });
console.log(`staged ${main.name}@${version}`);

function writeJSON(file, obj) {
  fs.writeFileSync(file, JSON.stringify(obj, null, 2) + "\n");
}
