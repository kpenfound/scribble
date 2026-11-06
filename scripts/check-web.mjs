import { readFileSync, existsSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
for (const name of ['app.js', 'outbox.js', 'sw.js']) execFileSync(process.execPath, ['--check', `internal/web/${name}`], { stdio: 'inherit' });
const manifest = JSON.parse(readFileSync('internal/web/manifest.webmanifest', 'utf8'));
for (const icon of manifest.icons) if (!existsSync(`internal/web${icon.src}`)) throw new Error(`Missing ${icon.src}`);
console.log('PWA assets validated; go build embeds them directly.');
