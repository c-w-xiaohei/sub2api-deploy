const fs = require("node:fs");

const [dotenvPath, key] = process.argv.slice(2);
if (!dotenvPath || !key || !/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) process.exit(2);
const dotenvLine = fs.readFileSync(dotenvPath, "utf8")
  .split(/\r?\n/)
  .find((candidate) => candidate.startsWith(`${key}=`));
if (!dotenvLine) process.exit(1);
const value = dotenvLine.slice(key.length + 1);
if (value.startsWith('"') && value.endsWith('"')) {
  process.stdout.write(JSON.parse(value));
} else {
  process.stdout.write(value);
}
