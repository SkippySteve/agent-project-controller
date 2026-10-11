'use strict';
const message = document.querySelector('#message');
async function api(path, method = 'GET', body) {
  const response = await fetch('/api/v1/' + path, {method, credentials: 'same-origin', headers: {'Content-Type': 'application/json'}, body: body ? JSON.stringify(body) : undefined});
  if (!response.ok) throw new Error(await response.text());
  return response.json();
}
function element(tag, text, className) { const e = document.createElement(tag); if (text) e.textContent = text; if (className) e.className = className; return e; }
async function refresh() {
  try {
    const projects = await api('workspaces');
    const root = document.querySelector('#projects'); root.replaceChildren();
    for (const p of projects || []) {
      const article = element('article'); const head = element('div', null, 'project-head');
      head.append(element('h3', p.metadata.name), element('span', p.status?.phase || 'Requested', 'phase')); article.append(head);
      article.append(element('p', p.spec.repository));
      const condition = p.status?.conditions?.find(c => c.type === 'Ready');
      if (condition) article.append(element('p', condition.message, 'hint'));
      if (p.status?.phase === 'Ready') article.append(element('code', `kubectl -n ${p.status.namespace} exec -it project -c workspace -- /bin/sh`));
      const buttons = element('div', null, 'actions');
      if (p.spec.desiredState !== 'Removed') for (const [text, state] of [[p.spec.desiredState === 'Paused' ? 'Resume' : 'Pause', p.spec.desiredState === 'Paused' ? 'Running' : 'Paused'], ['Remove project', 'Removed']]) {
        const button = element('button', text, state === 'Removed' ? 'danger' : 'secondary');
        button.onclick = async () => { if (state === 'Removed' && !confirm(`Remove ${p.metadata.name}? This revokes access and deletes project storage.`)) return; button.disabled = true; try { await api('workspaces/' + encodeURIComponent(p.metadata.name), 'PATCH', {desiredState: state, resourceVersion: p.metadata.resourceVersion}); message.textContent = `${p.metadata.name}: ${state.toLowerCase()} requested`; await refresh(); } catch (e) { message.textContent = e.message; button.disabled = false; } };
        buttons.append(button);
      }
      const audit = element('button', 'View access history', 'secondary');
      audit.onclick = async () => {try {const records = await api('workspaces/' + encodeURIComponent(p.metadata.name) + '/audit'); const details = element('pre', records.sort((a,b)=>a.time.localeCompare(b.time)).map(r=>`${r.time}  ${r.action}`).join('\n')); article.append(details);audit.remove();}catch(e){message.textContent=e.message;}}; buttons.append(audit);article.append(buttons);root.append(article);
    }
    if (!projects?.length) root.append(element('p', 'No project environments have been requested yet.', 'hint'));
  } catch (e) { message.textContent = e.message; }
}
document.querySelector('#create').onsubmit = async event => {event.preventDefault();const form = event.target; const button = form.querySelector('button');button.disabled = true;try {const data = new FormData(form);await api('workspaces','POST',{name:data.get('name'),repository:data.get('repository')});form.reset();message.textContent='Project requested. Preparation status will appear below.';await refresh();}catch(e){message.textContent=e.message;}finally{button.disabled=false;}};
api('repositories').then(repos => {const select=document.querySelector('#repositories');for(const r of repos || []){const option=element('option',r.full_name);option.value=r.full_name;select.append(option);}}).catch(e=>message.textContent=e.message);
api('session').then(session => {if(session.oidc){const logout=document.querySelector('#logout');logout.hidden=false;logout.onclick=async()=>{const response=await fetch('/auth/logout',{method:'POST',headers:{'Content-Type':'application/json'},credentials:'same-origin'});if(response.ok)location.reload();else message.textContent='Sign-out failed; try again.';};}}).catch(e=>message.textContent=e.message);
refresh();setInterval(refresh,10000);
