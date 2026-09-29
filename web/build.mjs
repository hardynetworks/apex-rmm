// Bundles the dashboard into dist/ (index.html + hashed assets).
import * as esbuild from 'esbuild';
import fs from 'node:fs';
import path from 'node:path';

const watch = process.argv.includes('--watch');
const out = 'dist';
fs.rmSync(out, { recursive: true, force: true });
fs.mkdirSync(path.join(out, 'assets'), { recursive: true });

const opts = {
  entryPoints: ['src/main.tsx'],
  bundle: true,
  minify: !watch,
  sourcemap: watch ? 'inline' : false,
  format: 'esm',
  target: 'es2021',
  jsx: 'automatic',
  jsxImportSource: 'preact',
  outdir: path.join(out, 'assets'),
  entryNames: watch ? '[name]' : '[name]-[hash]',
  assetNames: '[name]-[hash]',
  loader: { '.svg': 'file', '.woff2': 'file' },
  metafile: true,
  logLevel: 'info',
  plugins: [{
    name: 'html',
    setup(build) {
      build.onEnd((res) => {
        if (!res.metafile) return;
        const outs = Object.keys(res.metafile.outputs);
        const js = outs.find((f) => f.endsWith('.js'));
        const css = outs.find((f) => f.endsWith('.css'));
        let html = fs.readFileSync('index.html', 'utf8');
        html = html.replace('<!--CSS-->', css ? `<link rel="stylesheet" href="/${path.relative(out, css)}">` : '')
                   .replace('<!--JS-->', `<script type="module" src="/${path.relative(out, js)}"></script>`);
        fs.writeFileSync(path.join(out, 'index.html'), html);
        fs.copyFileSync('favicon.svg', path.join(out, 'favicon.svg'));
      });
    },
  }],
};

if (watch) {
  const ctx = await esbuild.context(opts);
  await ctx.watch();
} else {
  await esbuild.build(opts);
}
