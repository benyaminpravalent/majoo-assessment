// Validate every Mermaid diagram in the documentation with Mermaid's own parser.
//
//   npm install --no-save mermaid@11 jsdom
//   node scripts/check-mermaid.mjs docs/*.md
//
// This runs the real grammar rather than a regex approximation, so a diagram
// that passes here renders on GitHub and in any Mermaid-aware viewer. It parses
// only — it does not lay out or rasterise — which is why it needs jsdom rather
// than a headless browser, and why it takes a second rather than a minute.
//
// Exit code 0 means every diagram parsed.

import fs from 'node:fs';
import path from 'node:path';
import { JSDOM } from 'jsdom';

// Mermaid expects a browser. jsdom supplies enough of one for the parser.
const dom = new JSDOM('<!doctype html><html><body><div id="container"></div></body></html>', {
  pretendToBeVisual: true,
  url: 'http://localhost/',
});

globalThis.window = dom.window;
globalThis.document = dom.window.document;
// navigator is a getter-only property on globalThis in Node 21+.
Object.defineProperty(globalThis, 'navigator', {
  value: dom.window.navigator,
  configurable: true,
});
globalThis.HTMLElement = dom.window.HTMLElement;
globalThis.SVGElement = dom.window.SVGElement;
globalThis.Element = dom.window.Element;
globalThis.Node = dom.window.Node;
globalThis.getComputedStyle = dom.window.getComputedStyle;
globalThis.requestAnimationFrame = (cb) => setTimeout(cb, 0);

const mermaid = (await import('mermaid')).default;
mermaid.initialize({ startOnLoad: false, securityLevel: 'loose' });

const files = process.argv.slice(2);
if (files.length === 0) {
  console.error('usage: node scripts/check-mermaid.mjs <markdown files...>');
  process.exit(2);
}

let blocks = 0;
let failures = 0;

for (const file of files) {
  const text = fs.readFileSync(file, 'utf8');
  const fence = /```mermaid\r?\n([\s\S]*?)```/g;

  let match;
  let index = 0;
  while ((match = fence.exec(text)) !== null) {
    index += 1;
    blocks += 1;

    const diagram = match[1];
    const line = text.slice(0, match.index).split('\n').length;
    const kind = diagram.trim().split('\n')[0].slice(0, 40);

    try {
      await mermaid.parse(diagram);
      console.log(`  ok    ${path.basename(file)} block ${index} (line ${line}) — ${kind}`);
    } catch (err) {
      failures += 1;
      const message = String(err?.message ?? err).split('\n').slice(0, 6).join('\n        ');
      console.error(`  FAIL  ${path.basename(file)} block ${index} (line ${line})`);
      console.error(`        ${message}`);
    }
  }
}

console.log(`\n${blocks} diagram(s) parsed, ${failures} failure(s)`);
process.exit(failures === 0 ? 0 : 1);
