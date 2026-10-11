'use strict';
// Invoked by TestBrowserDashboard against its fake Kubernetes client, never a live deployment.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE_PATH || 'playwright');
(async () => {
  const browser = await chromium.launch({executablePath: process.env.CHROMIUM_PATH || '/usr/bin/chromium', headless: true});
  try {
    const context = await browser.newContext({httpCredentials: {username: 'operator', password: process.env.APC_TEST_WEB_PASSWORD}, viewport: {width: 1100, height: 850}});
    const page = await context.newPage();
    const errors = []; page.on('pageerror', error => errors.push(error.message));
    await page.goto(process.env.APC_TEST_WEB_ORIGIN);
    await page.getByLabel('Project name').fill('browser-project');
    await page.getByLabel('Forgejo repository').selectOption('steve/repo');
    await page.getByRole('button', {name: 'Open project', exact: true}).click();
    await page.getByRole('heading', {name: 'browser-project'}).waitFor();
    await page.getByRole('button', {name: 'Pause', exact: true}).click();
    await page.getByRole('button', {name: 'Resume', exact: true}).waitFor();
    await page.getByRole('button', {name: 'Resume', exact: true}).click();
    await page.getByRole('button', {name: 'Pause', exact: true}).waitFor();
    page.on('dialog', dialog => dialog.accept());
    await page.getByRole('button', {name: 'Remove project', exact: true}).click();
    await page.getByText('browser-project: removed requested', {exact: true}).waitFor();
    await page.getByRole('button', {name: 'View access history'}).click();
    await page.locator('article pre').waitFor();
    if (process.env.APC_TEST_SCREENSHOT) await page.screenshot({path: process.env.APC_TEST_SCREENSHOT, fullPage: true});
    if (errors.length) throw new Error(errors.join('\n'));
    console.log('Browser qualification passed: create, pause, resume, remove and audit with no JavaScript errors.');
  } finally { await browser.close(); }
})().catch(error => {console.error(error.message);process.exit(1);});
