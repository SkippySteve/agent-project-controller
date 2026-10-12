'use strict';
const message = document.querySelector('#message');
const drafts = new Map(), openPanels = new Set();
let refreshing = false;
async function api(path, method = 'GET', body) {
  const response = await fetch('/api/v1/' + path, {method, credentials: 'same-origin', headers: {'Content-Type': 'application/json'}, body: body === undefined ? undefined : JSON.stringify(body)});
  if (!response.ok) throw new Error((await response.text()).trim());
  return response.json();
}
function element(tag, text, className) { const e = document.createElement(tag); if (text) e.textContent = text; if (className) e.className = className; return e; }
function label(text, input) { const result = element('label', text); result.append(input); return result; }
function ruleRow(root, rule = {host: '', methods: ['GET', 'HEAD'], paths: ['/']}, disabled = false) {
  const row = element('div', null, 'rule-row');
  const host = element('input'); host.name = 'host'; host.value = rule.host; host.required = true; host.placeholder = 'api.example.com';
  const methods = element('input'); methods.name = 'methods'; methods.value = rule.methods.join(', '); methods.required = true;
  const paths = element('textarea'); paths.name = 'paths'; paths.value = rule.paths.join('\n'); paths.rows = 2; paths.required = true;
  for (const input of [host, methods, paths]) input.disabled = disabled;
  row.append(label('Hostname', host), label('Methods', methods), label('Path prefixes, one per line', paths));
  if (!disabled) { const remove = element('button', 'Remove destination', 'secondary'); remove.type = 'button'; remove.onclick = () => { row.remove(); root.dispatchEvent(new Event('input', {bubbles: true})); }; row.append(remove); }
  root.append(row);
}
function readRules(root) { return [...root.querySelectorAll('.rule-row')].map(row => ({host: row.querySelector('[name=host]').value.trim(), methods: row.querySelector('[name=methods]').value.split(',').map(s => s.trim()).filter(Boolean), paths: row.querySelector('[name=paths]').value.split('\n').map(s => s.trim()).filter(Boolean)})); }
function statusPanel(p) {
  const condition = p.status?.conditions?.find(c => c.type === 'Ready');
  const panel = element('div', null, 'project-status');
  if (condition && condition.observedGeneration !== undefined && condition.observedGeneration !== p.metadata.generation) {
    panel.append(element('p', 'Update requested. Waiting for the controller to apply it.')); return panel;
  }
  if (!condition) { panel.append(element('p', 'Request saved. Waiting for preparation to begin.')); return panel; }
  const blocked = condition.status !== 'True' && ['PreparationPending', 'PolicyRejected', 'CleanupPending'].includes(condition.reason) && !condition.message.startsWith('Waiting for');
  if (blocked) {
    panel.classList.add('needs-attention');
    panel.append(element('strong', p.status?.phase === 'Removing' ? 'Removal needs attention' : 'Preparation needs attention'));
  }
  panel.append(element('p', condition.message));
  if (blocked) panel.append(element('p', 'The controller will retry automatically. Correct the setting described above; keep this project request.', 'hint'));
  return panel;
}
function accessPanel(p) {
  const details = element('details', null, 'access-panel');
  details.append(element('summary', 'Access and egress'));
  details.open = openPanels.has(p.metadata.name);
  details.ontoggle = () => { if (details.open) openPanels.add(p.metadata.name); else openPanels.delete(p.metadata.name); };
  details.append(element('h4', 'Repository access'), element('p', p.spec.repository + ': project-scoped Git and pull request access. Protected branches and merges remain controlled by the repository owner.'));
  details.append(element('h4', 'Outbound HTTPS'), element('p', 'Only listed hosts, methods and path prefixes are allowed. Repository access is managed separately. A path of / permits the whole host. Registry rules permit downloads, not selected package versions.', 'hint'));
  const canEdit = p.spec.desiredState === 'Paused' && p.status?.phase === 'Paused' && p.status?.observedGeneration === p.metadata.generation;
  if (!canEdit && p.spec.desiredState !== 'Removed') details.append(element('p', 'Pause the project and wait for Paused to change access. Saving keeps it paused until you choose Resume.', 'notice'));
  const form = element('form', null, 'access-form'), rules = element('div', null, 'rules');
  const draft = drafts.get(p.metadata.name);
  for (const rule of draft?.rules || p.spec.https || []) ruleRow(rules, rule, !canEdit);
  form.append(rules);
  if (!(draft?.rules || p.spec.https || []).length) form.append(element('p', 'No additional HTTPS destinations are allowed.', 'hint'));
  if (canEdit) {
    const add = element('button', 'Add HTTPS destination', 'secondary'); add.type = 'button'; add.onclick = () => { ruleRow(rules); rules.dispatchEvent(new Event('input', {bubbles: true})); };
    const save = element('button', 'Save access'); save.type = 'submit';
    const discard = element('button', 'Discard edits', 'secondary'); discard.type = 'button'; discard.onclick = () => { drafts.delete(p.metadata.name); refresh(); };
    form.append(add, save, discard);
    form.oninput = () => { const initial = drafts.get(p.metadata.name); drafts.set(p.metadata.name, {rules: readRules(rules), resourceVersion: initial?.resourceVersion || p.metadata.resourceVersion}); };
    form.onsubmit = async event => {
      event.preventDefault(); save.disabled = true;
      try {
        await api('workspaces/' + encodeURIComponent(p.metadata.name) + '/access', 'PATCH', {resourceVersion: drafts.get(p.metadata.name)?.resourceVersion || p.metadata.resourceVersion, https: readRules(rules)});
        drafts.delete(p.metadata.name); message.textContent = p.metadata.name + ': access update requested. The project remains paused.';
        document.activeElement?.blur(); await refresh();
      } catch (error) { message.textContent = error.message; save.disabled = false; }
    };
  }
  details.append(form); return details;
}
async function refresh() {
  if (refreshing) return;
  refreshing = true;
  try {
    const projects = await api('workspaces');
    // Polling must not destroy keyboard focus or an in-progress access edit.
    if (document.activeElement?.closest('.access-form')) return;
    const root = document.querySelector('#projects'); root.replaceChildren();
    for (const p of projects || []) {
      const article = element('article'); article.dataset.project = p.metadata.name;
      const head = element('div', null, 'project-head');
      head.append(element('h3', p.metadata.name), element('span', p.status?.phase || 'Requested', 'phase')); article.append(head, element('p', p.spec.repository), statusPanel(p));
      if (p.status?.phase === 'Ready') article.append(element('code', 'agent-project-controller connect ' + p.metadata.name));
      article.append(accessPanel(p));
      const buttons = element('div', null, 'actions');
      if (p.spec.desiredState !== 'Removed') for (const [text, state] of [[p.spec.desiredState === 'Paused' ? 'Resume' : 'Pause', p.spec.desiredState === 'Paused' ? 'Running' : 'Paused'], ['Remove project', 'Removed']]) {
        const button = element('button', text, state === 'Removed' ? 'danger' : 'secondary');
        if (state === 'Running' && (p.status?.observedGeneration !== p.metadata.generation || p.status?.phase !== 'Paused') && p.metadata.uid) button.disabled = true;
        button.onclick = async () => {
          if (state === 'Removed' && !confirm('Remove ' + p.metadata.name + '? This revokes access and deletes project storage.')) return;
          button.disabled = true;
          try { await api('workspaces/' + encodeURIComponent(p.metadata.name), 'PATCH', {desiredState: state, resourceVersion: p.metadata.resourceVersion}); message.textContent = p.metadata.name + ': ' + state.toLowerCase() + ' requested'; drafts.delete(p.metadata.name); await refresh(); }
          catch (error) { message.textContent = error.message; button.disabled = false; }
        }; buttons.append(button);
      }
      const audit = element('button', 'View access history', 'secondary');
      audit.onclick = async () => {
        try { const records = await api('workspaces/' + encodeURIComponent(p.metadata.name) + '/audit'); const history = element('pre', records.sort((a, b) => a.time.localeCompare(b.time)).map(r => r.time + '  ' + r.action + (r.message ? '\n  ' + r.message : '')).join('\n') || 'No audit records yet.'); article.append(history); audit.remove(); }
        catch (error) { message.textContent = error.message; }
      }; buttons.append(audit); article.append(buttons); root.append(article);
    }
    if (!projects?.length) root.append(element('p', 'No project environments have been requested yet.', 'hint'));
  } catch (error) { message.textContent = error.message; }
  finally { refreshing = false; }
}
const customRules = document.querySelector('#create-rules');
document.querySelector('#add-create-rule').onclick = () => ruleRow(customRules);
document.querySelector('#create').onsubmit = async event => {
  event.preventDefault(); const form = event.target, button = document.querySelector('#open-project'); button.disabled = true;
  try {
    const data = new FormData(form), registries = [...form.querySelectorAll('input[name=registry]:checked')].map(input => input.value);
    await api('workspaces', 'POST', {name: data.get('name'), repository: data.get('repository'), registries, https: readRules(customRules)});
    form.reset(); customRules.replaceChildren(); message.textContent = 'Project requested. Preparation status will appear below.'; await refresh();
  } catch (error) { message.textContent = error.message; }
  finally { button.disabled = false; }
};
api('repositories').then(repos => { const select = document.querySelector('#repositories'); for (const r of repos || []) { const option = element('option', r.full_name); option.value = r.full_name; select.append(option); } }).catch(error => { message.textContent = error.message; });
api('session').then(session => {
  document.querySelector('#existing-sandboxes').hidden = !session.sandboxControl;
  if (session.oidc) {
    const logout = document.querySelector('#logout'); logout.hidden = false;
    logout.onclick = async () => { const response = await fetch('/auth/logout', {method: 'POST', headers: {'Content-Type': 'application/json'}, credentials: 'same-origin'}); if (response.ok) location.assign('/auth/signed-out'); else message.textContent = 'Sign-out failed; try again.'; };
  }
}).catch(error => { message.textContent = error.message; });
refresh(); setInterval(refresh, 10000);
