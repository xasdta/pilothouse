import { h, icon, agoLong, fill, add } from '../dom.js';
import { store, api } from '../api.js';
import { toast, refreshChrome, link, setTheme, theme, isWide } from '../ui.js';
import { pushStatus, enablePush, disablePush } from '../push.js';
import { openHelp } from '../palette.js';
import { section } from './rows.js';

export function settingsView() {
  const root = h('div', { class: 'view settings' });
  const notif = h('div', { class: 'group' });
  const presence = h('div', { class: 'group group-pad' });
  const appearance = h('div', { class: 'group group-pad' });
  const device = h('div', { class: 'group' });
  const conn = h('div', { class: 'group' });
  const tile = (c, i) => h('span', { class: 'icon-tile ' + c, 'aria-hidden': 'true' }, icon(i));

  add(root,
    h('div', { class: 'page-head' }, h('div', null, h('h1', { class: 'page-title' }, 'Settings'))),
    h('section', { class: 'section' }, h('div', { class: 'group' },
      link('/rules', { class: 'setting' }, tile('green', 'shield'),
        h('div', { class: 'label' }, h('b', null, 'Rules'), h('span', null, 'Guarded autopilot, and what runs without asking')),
        icon('chev', 'sm', 'chev')),
      link('/insights', { class: 'setting' }, tile('violet', 'chart'),
        h('div', { class: 'label' }, h('b', null, 'Insights'), h('span', null, 'Usage, cost and what the rules did')),
        icon('chev', 'sm', 'chev')))),
    section('Notifications', 0, notif),
    section('Other ways to reach you', 0, channels()),
    section('Where should requests go?', 0, presence),
    section('Appearance', 0, appearance),
    section('This device', 0, device),
    section('Connection', 0, conn),
    section('About', 0, about()),
    h('section', { class: 'section' },
      h('div', { class: 'group' }, h('button', { class: 'setting danger', type: 'button', onclick: signOut }, 'Sign out this device')),
      h('p', { class: 'group-note' }, 'Signing out revokes this device. Pair again with ', h('code', null, 'pilothouse pair'), '.')),
  );

  // ---- Notifications ----
  async function renderNotif() {
    const st = await pushStatus();
    let content;
    switch (st) {
      case 'ios-install':
        content = h('div', { class: 'group-pad' },
          h('div', { class: 'notice' }, tile('amber', 'bell'),
            h('div', null,
              h('h3', null, 'Add Pilothouse to your Home Screen first'),
              h('p', null, 'iPhone only delivers web notifications to apps on the Home Screen.'))),
          h('ol', { class: 'steps' },
            h('li', null, 'Tap ', h('span', { class: 'ic-inline' }, icon('share')), ' Share in Safari’s toolbar'),
            h('li', null, 'Choose ', h('b', null, 'Add to Home Screen'), ' ', h('span', { class: 'ic-inline' }, icon('plusSquare'))),
            h('li', null, 'Open Pilothouse from your Home Screen and come back here')));
        break;
      case 'insecure':
        content = h('div', { class: 'group-pad notice' }, tile('gray', 'bell'),
          h('div', null, h('h3', null, 'Needs a secure connection'),
            h('p', null, 'Notifications only work over HTTPS. On your computer run ', h('code', null, 'pilothouse remote tailscale'), ', then pair again.')));
        break;
      case 'unsupported':
        content = h('div', { class: 'group-pad notice' }, tile('gray', 'bell'),
          h('div', null, h('h3', null, 'Not supported here'), h('p', null, 'This browser can’t receive web notifications.')));
        break;
      case 'denied':
        content = h('div', { class: 'group-pad notice' }, tile('red', 'bell'),
          h('div', null, h('h3', null, 'Notifications are blocked'),
            h('p', null, 'Allow notifications for this site in your browser or system settings, then reload.')));
        break;
      case 'on':
        content = [
          h('div', { class: 'setting' }, tile('green', 'bell'),
            h('div', { class: 'label' }, h('b', null, 'Notifications are on'),
              h('span', null, /Android/.test(navigator.userAgent) || !/iPhone|iPad/.test(navigator.userAgent)
                ? 'Allow or deny right from the notification; answered ones update to “Answered ✓”'
                : 'Requests reach this device when you’re away'))),
          h('div', { class: 'group-pad' }, h('div', { class: 'row-btns' },
            h('button', { class: 'btn secondary', type: 'button', onclick: testPush }, 'Send a test'),
            h('button', { class: 'btn ghost', type: 'button', onclick: offPush }, 'Turn off'))),
        ];
        break;
      default:
        content = h('div', { class: 'group-pad' },
          h('div', { class: 'notice' }, tile('blue', 'bell'),
            h('div', null, h('h3', null, 'Get notified'),
              h('p', null, 'Get a notification when an agent needs you while you’re away. On Android and desktop you can answer right from it.'))),
          h('div', { class: 'row-btns' }, h('button', { class: 'btn primary', type: 'button', onclick: onPush }, 'Turn on notifications')));
    }
    fill(notif, ...[content].flat());
  }

  async function onPush(e) {
    const b = e.currentTarget;
    b.disabled = true;
    // Subscribing goes through the browser's push service and can take
    // many seconds: show that something is happening.
    fill(b, h('span', { class: 'spin', 'aria-hidden': 'true' }), 'Turning on…');
    b.setAttribute('aria-busy', 'true');
    const status = h('p', { class: 'group-note push-progress', role: 'status' },
      'Waiting for your browser to register with its push service…');
    if (b.parentNode) b.parentNode.after(status);
    try {
      await enablePush();
      toast('Notifications are on', 'ok');
    } catch (err) {
      toast(err.message || 'Couldn’t turn on notifications', 'bad');
    }
    renderNotif();
  }
  async function offPush() {
    await disablePush();
    toast('Notifications are off');
    renderNotif();
  }
  async function testPush(e) {
    const b = e.currentTarget;
    b.disabled = true;
    try {
      await api('POST', '/api/push/test');
      toast('Sent — it should arrive in a moment', 'ok');
    } catch (err) {
      toast(err.message, 'bad');
    }
    b.disabled = false;
  }

  function channels() {
    const row = (c, i, title, text, cmd) => h('div', { class: 'channel' }, tile(c, i),
      h('b', null, title), h('span', null, text, cmd ? [' ', h('code', null, cmd)] : null));
    return h('div', { class: 'group' },
      row('sky', 'message', 'Telegram', 'Two-way: buttons to allow, deny and answer questions, and replies to prompt the agent. Set it up on your computer with', 'pilothouse notify telegram'),
      row('amber', 'bell', 'ntfy', 'Push to the ntfy app, with action buttons on iPhone too. Set up by', 'pilothouse setup'),
      row('gray', 'desk', 'This computer', 'Desktop notifications when a native prompt waits while you’re at your desk.'),
    );
  }

  // ---- Presence ----
  function renderPresence() {
    const p = store.presence || { mode: 'auto', state: 'present' };
    const opts = [['here', 'desk', 'Here'], ['auto', 'auto', 'Auto'], ['away', 'away', 'Away']];
    const seg = h('div', { class: 'seg', role: 'radiogroup', 'aria-label': 'Presence' },
      opts.map(([mode, ic, label]) => h('button', {
        type: 'button', role: 'radio', 'aria-checked': String(p.mode === mode),
        onclick: () => setMode(mode),
      }, icon(ic, 'sm'), label)));
    const away = p.state === 'away';
    const why = p.mode === 'auto'
      ? (away ? 'Your computer has been idle' + (p.idle_secs ? ` for ${Math.round(p.idle_secs / 60)} min` : '') : 'You’re using your computer')
      : 'Set by hand';
    fill(presence, seg,
      h('div', { class: 'presence-now' }, h('span', { class: 'dot' + (away ? ' away' : '') }),
        h('span', null, away ? 'Requests come to your devices' : 'Requests open in the terminal', h('span', { class: 'sr-only' }, '. '))),
      h('p', { class: 'group-note' }, why + '. Auto switches to Away when your computer is idle.'));
  }

  async function setMode(mode) {
    const prev = store.presence;
    store.presence = { ...(prev || {}), mode };
    renderPresence();
    try {
      store.presence = await api('PUT', '/api/presence', { mode });
    } catch (err) {
      store.presence = prev;
      toast(err.message, 'bad');
    }
    renderPresence();
    refreshChrome();
  }

  // ---- Appearance ----
  function renderAppearance() {
    const t = theme();
    const opts = [['system', 'monitor', 'System'], ['light', 'sun', 'Light'], ['dark', 'moon', 'Dark']];
    fill(appearance,
      h('div', { class: 'seg', role: 'radiogroup', 'aria-label': 'Theme' },
        opts.map(([mode, ic, label]) => h('button', {
          type: 'button', role: 'radio', 'aria-checked': String(t === mode), 'data-theme-mode': mode,
          onclick: () => { setTheme(mode); renderAppearance(); },
        }, icon(ic, 'sm'), label))),
      h('p', { class: 'group-note' }, t === 'system' ? 'Follows this device’s light or dark setting.' : 'Saved on this device.'));
  }

  // ---- Device ----
  function renderDevice() {
    const d = store.me.device;
    const input = h('input', { class: 'input', id: 'device-name', value: d.name, maxlength: 60, autocomplete: 'off' });
    const save = h('button', { class: 'btn secondary', type: 'submit' }, 'Save');
    const form = h('form', { class: 'name-form' }, input, save);
    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      const name = input.value.trim();
      if (!name || name === store.me.device.name) return;
      save.disabled = true;
      try {
        store.me.device = await api('PATCH', '/api/device', { name });
        toast('Renamed', 'ok');
      } catch (err) {
        toast(err.message, 'bad');
      }
      save.disabled = false;
    });
    fill(device,
      h('div', { class: 'group-pad' }, h('label', { class: 'field-label', for: 'device-name' }, 'Name'), form),
      h('div', { class: 'setting' }, tile('green', 'key'),
        h('div', { class: 'label' }, h('b', null, 'Signing key'), h('span', null, 'Kept on this device; signs every answer, reply and rule')),
        h('div', { class: 'value' }, 'P-256')),
      h('div', { class: 'setting' }, tile('gray', 'phone'),
        h('div', { class: 'label' }, h('b', null, 'Paired'), h('span', null, new Date(d.created_at).toLocaleDateString())),
        h('div', { class: 'value' }, agoLong(d.created_at))));
  }

  function renderConn() {
    const me = store.me;
    fill(conn,
      h('div', { class: 'setting' }, tile('blue', 'desk'),
        h('div', { class: 'label' }, h('b', null, store.machine || me.machine), h('span', null, store.conn === 'live' ? 'Connected' : 'Reconnecting…')),
        h('div', { class: 'value' }, /^\d/.test(me.version) ? 'v' + me.version : me.version)),
      h('div', { class: 'setting' }, tile('gray', 'link'),
        h('div', { class: 'label' }, h('b', null, 'Address'), h('span', { class: 'mono' }, location.origin)),
        h('div', { class: 'value' }, me.public_url ? 'Tailnet' : 'Local only')));
  }

  function about() {
    return h('div', { class: 'group' },
      h('div', { class: 'setting' }, tile('green', 'shield'),
        h('div', { class: 'label' }, h('b', null, 'Pilothouse'),
          h('span', null, 'Open source. No account, no relay: your agents, rules and answers stay between your devices and your computer.'))),
      isWide() || matchMedia('(hover: hover)').matches
        ? h('button', { class: 'setting', type: 'button', onclick: () => openHelp() }, tile('gray', 'keyboard'),
          h('div', { class: 'label' }, h('b', null, 'Keyboard shortcuts'), h('span', null, 'Press ? anywhere')), icon('chev', 'sm', 'chev'))
        : null,
      h('div', { class: 'setting' }, tile('gray', 'info'),
        h('div', { class: 'label' }, h('b', null, 'Docs and source'), h('span', { class: 'mono' }, 'github.com/xasdta/pilothouse'))));
  }

  async function signOut() {
    if (!confirm('Sign out this device? It will stop receiving requests.')) return;
    try {
      await disablePush().catch(() => {});
      await api('POST', '/api/logout');
      await self.PilothouseKeys.clear().catch(() => {});
      location.assign('/pair');
    } catch (err) {
      toast(err.message, 'bad');
    }
  }

  const onTheme = () => renderAppearance();
  addEventListener('pilothouse:theme', onTheme);
  renderNotif();
  renderPresence();
  renderAppearance();
  renderDevice();
  renderConn();
  return {
    el: root,
    update(kind) {
      renderPresence();
      renderConn();
      if (kind === 'data' && !device.contains(document.activeElement)) renderDevice();
    },
    destroy() { removeEventListener('pilothouse:theme', onTheme); },
  };
}
