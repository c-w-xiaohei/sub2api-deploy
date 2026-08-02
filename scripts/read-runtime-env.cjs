const fs = require("node:fs");

const key = process.argv[2];
if (!key || !/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) process.exit(2);
const line = fs.readFileSync("runtime/runtime.env", "utf8")
  .split(/\r?\n/)
  .find((candidate) => candidate.startsWith(`${key}=`));
if (!line) process.exit(1);
const value = line.slice(key.length + 1);
if (value.startsWith('"') && value.endsWith('"')) {
  process.stdout.write(JSON.parse(value));
} else {
  process.stdout.write(value);
}
