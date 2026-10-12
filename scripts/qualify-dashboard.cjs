'use strict';
// Invoked by TestBrowserDashboard against its fake Kubernetes client, never a live deployment.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE_PATH || 'playwright');
(async () => {
  const browser = await chromium.launch({executablePath: process.env.CHROMIUM_PATH || '/usr/bin/chromium', headless: true});
  try {
    const context = await browser.newContext({ignoreHTTPSErrors: true, httpCredentials: {username: 'operator', password: process.env.APC_TEST_WEB_PASSWORD}, viewport: {width: 1100, height: 850}});
    const page = await context.newPage();
    const errors = []; page.on('pageerror', error => errors.push(error.message));
    await page.goto(process.env.APC_TEST_WEB_ORIGIN);
	const blocker = page.locator('article[data-project="blocked-project"]');
	await blocker.getByText('Preparation needs attention', {exact: true}).waitFor();
	await blocker.getByText(/require at least 1 approval \(currently 0\)/).waitFor();
	const paused = page.locator('article[data-project="paused-project"]');
	await paused.getByText('Access and egress', {exact: true}).click();
	await paused.getByRole('button', {name:'Add HTTPS destination',exact:true}).click();
	await paused.getByLabel('Hostname').fill('api.example.com');
	await paused.getByLabel('Methods', {exact:true}).fill('GET');
	await paused.getByLabel('Path prefixes, one per line').fill('/v1');
	await paused.getByRole('button',{name:'Save access',exact:true}).click();
	await page.getByText('paused-project: access update requested. The project remains paused.',{exact:true}).waitFor();
    await page.getByLabel('Project name').fill('browser-project');
    await page.getByLabel('Forgejo repository').selectOption('steve/repo');
    await page.getByRole('button', {name: 'Open project', exact: true}).click();
    await page.getByRole('heading', {name: 'browser-project'}).waitFor();
	const project = page.locator('article[data-project="browser-project"]');
    await project.getByRole('button', {name: 'Pause', exact: true}).click();
    await project.getByRole('button', {name: 'Resume', exact: true}).waitFor();
    await project.getByRole('button', {name: 'Resume', exact: true}).click();
    await project.getByRole('button', {name: 'Pause', exact: true}).waitFor();
    page.on('dialog', dialog => dialog.accept());
    await project.getByRole('button', {name: 'Remove project', exact: true}).click();
    await page.getByText('browser-project: removed requested', {exact: true}).waitFor();
    await project.getByRole('button', {name: 'View access history'}).click();
    await project.locator('pre').waitFor();
    if (process.env.APC_TEST_SCREENSHOT) await page.screenshot({path: process.env.APC_TEST_SCREENSHOT, fullPage: true});
	await page.getByRole('link',{name:'Existing sandboxes',exact:true}).click();
	await page.getByRole('heading',{name:'Existing sandbox access',exact:true}).waitFor();
	for(const name of ['shared-pilot','codex-a','grok-a','cursor-a']) await page.getByRole('link',{name,exact:true}).waitFor();
	await page.getByRole('link',{name:'shared-pilot',exact:true}).click();
	await page.getByRole('button',{name:'Save existing access',exact:true}).click();
	await page.waitForURL(/\/sandboxes\/\?workspace=shared-pilot&tab=access/);
	await page.getByRole('link',{name:'Projects',exact:true}).click();
	await page.getByRole('heading',{name:'Projects and sandboxes',exact:true}).waitFor();
    if (errors.length) throw new Error(errors.join('\n'));
    console.log('Browser qualification passed: visible preparation blocker, paused egress editing, create/pause/resume/remove, audit and existing sandbox controls in one authenticated app.');
  } finally { await browser.close(); }
})().catch(error => {console.error(error.message);process.exit(1);});
