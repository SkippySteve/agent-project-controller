'use strict';
fetch('/api/v1/session', {credentials: 'same-origin'}).then(async response => {
  if (!response.ok) throw new Error('Your console session has ended. Sign in again.');
  const session = await response.json();
  const button = document.querySelector('#console-logout');
  if (session.oidc && button) {
    button.hidden = false;
    button.onclick = async () => {
      const response = await fetch('/auth/logout', {method: 'POST', headers: {'Content-Type': 'application/json'}, credentials: 'same-origin'});
      if (response.ok) location.assign('/auth/signed-out');
      else document.querySelector('#console-message').textContent = 'Sign-out failed; try again.';
    };
  }
}).catch(error => { const message = document.querySelector('#console-message'); if (message) message.textContent = error.message; });
