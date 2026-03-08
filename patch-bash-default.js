const fs = require("fs");
const path = require("path");

const root = "/app/dist";
const markers = ["chat:bash", "bash started (session", "/bash"];
let scanned = 0;
let patched = 0;

function walk(dir) {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      walk(full);
      continue;
    }
    if (!entry.isFile() || !entry.name.endsWith(".js")) {
      continue;
    }
    scanned += 1;
    const raw = fs.readFileSync(full, "utf8");
    const isBashCandidate =
      entry.name === "bash-command.js" || markers.some((marker) => raw.includes(marker));
    if (!isBashCandidate) {
      continue;
    }
    const next = raw.replace(/\bdefaultLevel\s*:\s*["']on["']/g, 'defaultLevel: "full"');
    if (next === raw) {
      continue;
    }
    fs.writeFileSync(full, next);
    patched += 1;
    console.log("patched", full);
  }
}

walk(root);
console.log(`scan complete: scanned=${scanned} patched=${patched}`);

if (patched === 0) {
  throw new Error(`no bash defaultLevel patch target found under ${root}`);
}
