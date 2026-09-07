const fs = require('fs');

async function main() {
  for (const path of process.argv.slice(2)) {
    const bytes = fs.readFileSync(path);
    const { instance } = await WebAssembly.instantiate(bytes);
    const result = instance.exports.run_tests();
    if (result !== 0) {
      throw new Error(`${path}: failed at source line ${result}`);
    }
    process.stdout.write(`PASS ${path}\n`);
  }
}

main().catch((error) => {
  process.stderr.write(`${error.message}\n`);
  process.exitCode = 1;
});
