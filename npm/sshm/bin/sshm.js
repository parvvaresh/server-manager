#!/usr/bin/env node
// Launcher for the sshm binary. The binary itself ships in one platform
// package per OS and CPU, installed through optionalDependencies, so npm only
// downloads the one that matches this machine.
"use strict";

const { spawnSync } = require("child_process");
const fs = require("fs");

const PLATFORMS = {
  "darwin arm64": "@parvvaresh/sshm-darwin-arm64",
  "darwin x64": "@parvvaresh/sshm-darwin-x64",
  "linux arm64": "@parvvaresh/sshm-linux-arm64",
  "linux x64": "@parvvaresh/sshm-linux-x64",
};

function fail(msg) {
  process.stderr.write(`sshm: ${msg}\n`);
  process.exit(1);
}

function binaryPath() {
  if (process.env.SSHM_BINARY) return process.env.SSHM_BINARY;

  const key = `${process.platform} ${process.arch}`;
  const pkg = PLATFORMS[key];
  if (!pkg) {
    fail(`no prebuilt binary for ${key}; supported: ${Object.keys(PLATFORMS).join(", ")}`);
  }
  try {
    return require.resolve(`${pkg}/bin/sshm`);
  } catch {
    fail(
      `the package ${pkg} is missing. It is installed as an optional ` +
        `dependency, so reinstall without --omit=optional or --no-optional:\n` +
        `  npm install -g @parvvaresh/sshm`
    );
  }
}

function run(bin) {
  return spawnSync(bin, process.argv.slice(2), { stdio: "inherit" });
}

// The child shares our terminal, so it receives Ctrl-C and friends directly.
// Stay alive until it decides what to do with them.
const SIGNALS = ["SIGINT", "SIGTERM", "SIGHUP", "SIGQUIT"];
for (const sig of SIGNALS) process.on(sig, () => {});

const bin = binaryPath();
let result = run(bin);

// Some package managers drop the executable bit when unpacking.
if (result.error && result.error.code === "EACCES") {
  try {
    fs.chmodSync(bin, 0o755);
    result = run(bin);
  } catch {}
}

if (result.error) fail(`cannot run ${bin}: ${result.error.message}`);

if (result.signal) {
  for (const sig of SIGNALS) process.removeAllListeners(sig);
  process.kill(process.pid, result.signal);
}
process.exit(result.status === null ? 1 : result.status);
