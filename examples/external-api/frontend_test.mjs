// Real browser test: normal CORS and CSP enforcement, no external traffic.
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const html = await readFile(new URL('./browser.html', import.meta.url));
const listen = server => new Promise((resolve, reject) => {
  server.once('error', reject);
  server.listen(0, '127.0.0.1', () => resolve(`http://127.0.0.1:${server.address().port}`));
});
const close = server => new Promise(resolve => server.close(resolve));
let apiRequests = 0;
const api = createServer((request, response) => {
  apiRequests++;
  if (request.url === '/cors') response.setHeader('Access-Control-Allow-Origin', '*');
  response.setHeader('Content-Type', 'application/json');
  response.end(JSON.stringify({ message: 'fixture only' }));
});
const site = createServer((request, response) => {
  if (request.url === '/restricted') {
    response.setHeader('Content-Security-Policy', "connect-src 'self'");
  }
  response.setHeader('Content-Type', 'text/html; charset=utf-8');
  response.end(html);
});
let browser;
try {
  const apiOrigin = await listen(api);
  const siteOrigin = await listen(site);
  browser = await chromium.launch({
    headless: true,
    ...(process.env.CHROMIUM_EXECUTABLE ? { executablePath: process.env.CHROMIUM_EXECUTABLE } : {}),
  });
  const context = await browser.newContext();
  const page = await context.newPage();
  const request = async (path, endpoint) => {
    await page.goto(siteOrigin + path);
    await page.locator('#endpoint').fill(apiOrigin + endpoint);
    await page.locator('button').click();
    await page.waitForFunction(() => {
      const text = document.querySelector('#result').textContent;
      return text !== '' && text !== 'Loading…';
    });
    return page.locator('#result').textContent();
  };
  assert.equal(await request('/', '/cors'), '{\n  "message": "fixture only"\n}');
  const failure = 'Request failed. Check the endpoint, CORS, CSP and HTTPS requirements.';
  assert.equal(await request('/', '/no-cors'), failure);
  const beforeCsp = apiRequests;
  assert.equal(await request('/restricted', '/cors'), failure);
  assert.equal(apiRequests, beforeCsp, 'CSP must prevent the outgoing request');
  await context.close();
  console.log('Browser example passed: readable CORS response, missing-CORS rejection, CSP blocking.');
} finally {
  if (browser) await browser.close();
  await Promise.all([close(site), close(api)]);
}
