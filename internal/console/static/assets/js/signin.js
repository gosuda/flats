// Portal sign-in in the console (docs/spec/portal-sign-in.md): the Sign-in
// row of the Settings tab of a flat, with the form that issues an access link
// and the button that ends every session and link (rules 49 to 54 and 63),
// and the words of a set_signin approval (rule 64). The console reads the
// setting and decides its approvals; a change is requested with flats signin
// set on the host. Entries, subjects, issuers and client IDs are text that
// people typed, so they only ever become text nodes (dom.js).
'use strict';

import { h, clear, icon, dateTime, timeEl } from './dom.js';
import { api } from './api.js';
import { confirmDialog, errorPanel, toast, busy, fill } from './ui.js';

// SIGN_IN_SCOPE is the first text of rule 63; changeText is the second.
const SIGN_IN_SCOPE = 'Sign-in protects the Portal address of this flat only. Its private routes, previews, the console and MCP are not behind sign-in.';
const changeText = () => h('p', { class: 'muted small' },
  'Change it with ', h('code', { text: 'flats signin set' }), ' on the host. Each change waits for your approval here.');

const signInOn = (s) => !!(s && s.provider);
const PROVIDERS = { oidc: 'OpenID Connect', credential: 'Access links' };
const providerName = (s) => (signInOn(s) ? PROVIDERS[s.provider] || s.provider : 'Off');

// issuerHost is the host of an issuer URL, or the issuer itself when it does
// not parse.
function issuerHost(issuer) {
  try { return new URL(issuer).host || String(issuer); } catch { return String(issuer || ''); }
}

// providerText names the provider of a setting as rule 63 does: OpenID
// Connect with the host of its issuer, Access links, or Off.
function providerText(s) {
  return s && s.provider === 'oidc' ? `${PROVIDERS.oidc} · ${issuerHost(s.issuer)}` : providerName(s);
}

const entries = (n) => (n === 1 ? '1 entry' : `${n} entries`);
// allowOf is the allowlist of a setting, or null for a setting that is on and
// holds only its count, as the routes outside the console show it (rule 58).
const allowOf = (s) => (s && Array.isArray(s.allow) ? s.allow : signInOn(s) ? null : []);
const countOf = (s) => (allowOf(s) ? allowOf(s).length : Number(s.count) || 0);

const entryList = (list) => (list.length
  ? h('ul', { class: 'plain-list signin-entries' }, list.map((e) => h('li', null, h('code', { text: e }))))
  : h('span', { class: 'muted', text: 'none' }));

// --- approvals (rule 64) ---

// describeSignIn is the title of a set_signin approval of flat with the
// params p.
export function describeSignIn(flat, p) {
  const from = signInOn(p.from);
  const to = signInOn(p.to);
  if (to && !from) return `Turn on sign-in for ${flat}`;
  if (from && !to) return `Turn off sign-in for ${flat}: anyone can open its Portal address`;
  return `Change the sign-in of ${flat}`;
}

// entryChanges lists the entries that a request adds and removes, or is null
// when the approval does not hold them.
function entryChanges(from, to) {
  const a = allowOf(from);
  const b = allowOf(to);
  if (!a || !b) return null;
  return { added: b.filter((e) => !a.includes(e)), removed: a.filter((e) => !b.includes(e)) };
}

// widening says why a request widens who can open the Portal address of a
// flat: sign-in goes off, an entry is added, or the provider, the issuer or
// the client ID changes. Turning sign-in on narrows it, so it says nothing.
export function widening(p) {
  const from = p.from;
  const to = p.to;
  if (!signInOn(from)) return [];
  if (!signInOn(to)) return ['Sign-in goes off: anyone can open its Portal address while the flat is Public.'];
  const out = [];
  if (from.provider !== to.provider) out.push(`The provider changes from ${providerName(from)} to ${providerName(to)}.`);
  else if (to.provider === 'oidc') {
    if (from.issuer !== to.issuer) out.push(`The issuer changes from ${from.issuer} to ${to.issuer}.`);
    if (from.client_id !== to.client_id) out.push(`The client ID changes from ${from.client_id} to ${to.client_id}.`);
  }
  const changes = entryChanges(from, to);
  if (!changes) out.push('The console cannot compare the entries of this request.');
  else if (changes.added.length) out.push(`${changes.added.length === 1 ? '1 entry is' : `${changes.added.length} entries are`} added to the allowlist.`);
  return out;
}

// secretMismatch is the text of rule 27 for the oidc setting to of flat slug
// when the stored client secret is bound to another issuer or client ID, or
// ''. requested says that to waits for approval; otherwise it is stored.
export function secretMismatch(slug, to, secret, requested) {
  if (!secret || !to || to.provider !== 'oidc' || (secret.issuer === to.issuer && secret.client_id === to.client_id)) return '';
  const cmd = `flats signin secret set ${slug} --issuer ${to.issuer} --client-id ${to.client_id}`;
  return `The stored client secret belongs to issuer ${secret.issuer} and client ID ${secret.client_id}, not to ` +
    (requested ? `the requested setting. If you approve, the Portal route of ${slug} stops until the operator sets the client secret for issuer ${to.issuer} with ${cmd} on the host.`
      : `this setting, so the Portal route of ${slug} is stopped until the operator sets the client secret for issuer ${to.issuer} with ${cmd} on the host.`);
}

// signInExpired reports a pending set_signin approval whose sign-in changed
// after it was requested; the host refuses it as stale (rule 18). view is the
// sign-in of the flat now (rule 56), or null.
export function signInExpired(a, p, view) {
  return a.action === 'set_signin' && a.status === 'pending' && !!view && typeof p.seq === 'number' && view.seq !== p.seq;
}

// settingFact shows one setting of an approval.
function settingFact(s) {
  if (!signInOn(s)) return h('span', { text: 'Off' });
  return h('div', null, h('div', { text: providerText(s) }),
    s.provider === 'oidc' ? h('div', { class: 'muted small' }, 'Issuer ', h('code', { text: s.issuer || '' }), ', client ID ', h('code', { text: s.client_id || '' })) : null,
    h('div', { class: 'muted small', text: entries(countOf(s)) }));
}

// signInFacts are the rows of the approval page of a set_signin approval: the
// setting when requested, the requested one, and the entries that the request
// adds and removes.
export function signInFacts(p) {
  const rows = [['Sign-in when requested', settingFact(p.from)], ['Requested sign-in', settingFact(p.to)]];
  const changes = entryChanges(p.from, p.to);
  if (changes) rows.push(['Added entries', entryList(changes.added)], ['Removed entries', entryList(changes.removed)]);
  return rows;
}

// signInNotes are the warnings of a pending set_signin approval of flat slug:
// what widens access, and the text of rule 27 when the stored client secret
// (from view, the sign-in of the flat now) has another binding than the
// requested setting.
export function signInNotes(slug, p, view) {
  const reasons = widening(p);
  const mismatch = secretMismatch(slug, p.to, view && view.client_secret, true);
  return [
    reasons.length ? h('div', { class: 'alert alert-warn', role: 'alert' },
      h('strong', { text: `This change widens who can open the Portal address of ${slug}.` }),
      h('ul', { class: 'compact' }, reasons.map((r) => h('li', { text: r })))) : null,
    mismatch ? h('p', { class: 'alert alert-warn', text: mismatch }) : null,
  ].filter(Boolean);
}

// signInConfirmNotice is the warning of the confirmation before a set_signin
// approval is approved, or ''.
export function signInConfirmNotice(slug, p, view) {
  const reasons = widening(p);
  return [reasons.length ? `This change widens who can open the Portal address of ${slug}. ${reasons.join(' ')}` : '',
    secretMismatch(slug, p.to, view && view.client_secret, true)].filter(Boolean).join(' ');
}

// --- the Sign-in row (rule 63) ---

// signInRow is the row Sign-in of the Settings tab of flat, below Sharing,
// from view, the answer of GET /signin (rule 56), or the error of that
// request. It shows the provider, the client ID, the entries, the client
// secret binding, the callback URLs and the texts of rule 63; for credential
// the form Issue a link, and always the button End all sessions and links,
// which also ends the links of an earlier setting. While sign-in is off it
// reads Off, with the same texts and the same button.
export function signInRow(flat, view) {
  const title = h('div', null, h('span', { class: 'setting-title', text: 'Sign-in' }),
    h('div', { class: 'muted small', text: 'Who can open its Portal address' }));
  if (!view || view instanceof Error) {
    return h('div', { class: 'setting-row setting-signin' }, title,
      h('div', { class: 'setting-sharing' }, errorPanel(view || new Error('the host sent no sign-in'), 'Cannot load the sign-in')));
  }
  const on = signInOn(view);
  let issue = null;
  if (view.provider === 'credential') {
    issue = h('button', { type: 'button', class: 'btn btn-small', text: 'Issue a link…' });
    issue.addEventListener('click', () => busy(issue, () => linkDialog(flat, view.allow || [])));
  }
  const revoke = h('button', { type: 'button', class: 'btn btn-small btn-danger-text', text: 'End all sessions and links' });
  revoke.addEventListener('click', () => busy(revoke, () => endSessions(flat)));
  return h('div', { class: 'setting-row setting-signin' }, title,
    h('div', { class: 'setting-sharing' },
      h('div', { class: 'setting-control' }, h('span', { class: on ? null : 'muted', text: providerText(view) }), issue, revoke),
      on || view.client_secret ? facts(flat, view) : null,
      h('p', { class: 'muted small', text: SIGN_IN_SCOPE }),
      changeText()));
}

function facts(flat, view) {
  const rows = [];
  if (view.provider === 'oidc') rows.push(['Issuer', h('code', { text: view.issuer })], ['Client ID', h('code', { text: view.client_id })]);
  if (signInOn(view)) {
    const list = view.allow || [];
    rows.push(['Allowlist', h('div', null, h('span', { text: entries(list.length) }), entryList(list))]);
  }
  if (view.provider === 'oidc' || view.client_secret) rows.push(['Client secret', secretFact(flat, view)]);
  if (view.provider === 'oidc') rows.push(['Callback URLs', callbacks(view.callback_urls || [])]);
  return h('dl', { class: 'facts signin-facts' }, rows.map(([k, v]) => [h('dt', { text: k }), h('dd', null, v)]));
}

// secretFact says whether a client secret is stored and for which issuer and
// client ID; it never has the value.
function secretFact(flat, view) {
  const cs = view.client_secret;
  if (!cs) return h('span', { class: 'muted', text: 'None stored. Flats signs in at the provider as a public client with PKCE.' });
  const mismatch = secretMismatch(flat.slug, view, cs, false);
  return h('div', null,
    h('div', null, 'Stored for issuer ', h('code', { text: cs.issuer }), ' and client ID ', h('code', { text: cs.client_id }), ', set ', timeEl(cs.updated_at)),
    view.provider !== 'oidc' ? h('div', { class: 'muted small', text: 'Only an OpenID Connect sign-in with this issuer and client ID uses it.' }) : null,
    mismatch ? h('p', { class: 'alert alert-warn small', text: mismatch }) : null);
}

function callbacks(urls) {
  if (!urls.length) return h('span', { class: 'muted', text: 'None yet: they appear once the flat has a Portal route.' });
  return h('div', null, entryList(urls), h('div', { class: 'muted small', text: 'Register each at the provider.' }));
}

// endSessions ends every sign-in session and access link of flat after a
// confirmation that says what ends (rule 54), and shows the result.
async function endSessions(flat) {
  const ok = await confirmDialog({
    title: `End all sessions and links of ${flat.slug}?`,
    body: [
      h('p', { text: `Every visitor who signed in on the Portal address of ${flat.slug} is signed out, and every access link of ${flat.slug} stops working, also a link of an earlier sign-in setting.` }),
      h('p', { class: 'muted', text: 'Visitors on the allowlist can sign in again, and you can issue new links. The Portal address stays the same.' }),
    ],
    confirmLabel: 'End all sessions and links',
    danger: true,
  });
  if (!ok) return;
  try {
    const res = await api.revokeSignIn(flat.slug);
    const msg = (res && res.message) || 'every session and access link ended';
    // The sessions end also when the gate cannot be built again; the message then says that the route is stopped.
    toast(msg[0].toUpperCase() + msg.slice(1) + '.', msg.includes('is stopped') ? 'error' : 'success');
  } catch (err) {
    toast(err.message, 'error');
  }
}

// EXPIRY_UNITS are the units of the expiry of a link; the host takes 1 minute
// to 90 days (rule 50).
const EXPIRY_UNITS = [['minutes', 60e3], ['hours', 3600e3], ['days', 86400e3]];

// linkDialog is the form Issue a link (rules 49 to 52): a subject of the
// allowlist and an expiry, 7 days unless changed. The answer holds the link,
// which the dialog shows once with a copy button. The link lives only in the
// text of linkEl, which the copy button reads and the closing of the dialog
// empties: nothing stores, logs or navigates with it, it is never a link that
// the browser could open or keep in its history, and the promise resolves
// with nothing.
function linkDialog(flat, subjects) {
  return new Promise((resolve) => {
    let linkEl = null;
    const id = 'signin-link-' + Math.random().toString(36).slice(2);
    const subject = h('select', { id: id + '-subject' }, subjects.map((s) => h('option', { value: s, text: s })));
    subject.value = subjects[0] || '';
    const amount = h('input', { id: id + '-amount', type: 'number', min: '1', step: '1', inputmode: 'numeric', value: '7', autocomplete: 'off' });
    const unit = h('select', { id: id + '-unit', 'aria-label': 'Unit of the expiry' }, EXPIRY_UNITS.map(([value]) => h('option', { value, text: value })));
    unit.value = 'days';
    const problem = h('div');
    const body = h('div', { class: 'dialog-body' },
      h('p', { text: `The link signs its holder in on the Portal address of ${flat.slug} as the subject that you choose, until it expires.` }),
      h('div', { class: 'field' }, h('label', { for: id + '-subject', text: 'Subject' }), subject),
      h('div', { class: 'field' }, h('label', { for: id + '-amount', text: 'Expires in' }),
        h('div', { class: 'inline-form' }, amount, unit),
        h('span', { class: 'hint', text: 'From 1 minute to 90 days.' })),
      problem);
    const issue = h('button', { type: 'submit', class: 'btn btn-primary', text: 'Issue link' });
    const cancel = h('button', { type: 'button', class: 'btn', text: 'Cancel' });
    const actions = h('div', { class: 'dialog-actions' }, cancel, issue);
    const form = h('form', { autocomplete: 'off' }, h('h2', { id: id + '-title', text: `Issue an access link for ${flat.slug}` }), body, actions);
    const dlg = h('dialog', { class: 'dialog signin-link', 'aria-labelledby': id + '-title' }, form);
    let sending = false;

    async function send() {
      clear(problem);
      const n = Number(amount.value);
      const unitMS = (EXPIRY_UNITS.find(([value]) => value === unit.value) || [])[1];
      if (!Number.isSafeInteger(n) || n < 1 || !unitMS) {
        problem.appendChild(h('p', { class: 'field-error', role: 'alert', text: 'Use a whole number of 1 or more.' }));
        return;
      }
      // RFC 3339 in whole seconds, as a credential holds them.
      const expires = new Date(Date.now() + n * unitMS).toISOString().replace(/\.\d+Z$/, 'Z');
      sending = true;
      cancel.disabled = true;
      let res;
      try {
        res = await api.issueSignInLink(flat.slug, subject.value, expires);
      } catch (err) {
        problem.appendChild(errorPanel(err, 'The link was not issued'));
        return;
      } finally {
        sending = false;
        cancel.disabled = false;
      }
      if (!dlg.open) return;
      if (!res || typeof res.url !== 'string' || !res.url) {
        problem.appendChild(errorPanel(new Error('the answer of the host holds no link'), 'The link was not issued'));
        return;
      }
      show(res);
    }
    // show puts the link where the form was, once.
    function show(res) {
      linkEl = h('code', { text: res.url });
      const copyText = h('span', { text: 'Copy' });
      const copy = h('button', { type: 'button', class: 'btn btn-small', 'aria-label': 'Copy the access link' }, icon('copy'), copyText);
      copy.addEventListener('click', async () => {
        try {
          await navigator.clipboard.writeText(linkEl.textContent);
          copyText.textContent = 'Copied';
        } catch {
          toast('Copy is unavailable here; select the link and press Ctrl/Cmd+C.', 'error');
        }
      });
      const close = h('button', { type: 'button', class: 'btn btn-primary', text: 'Close' });
      close.addEventListener('click', () => dlg.close());
      fill(body,
        h('div', { class: 'copyable' }, h('div', { class: 'copy-label', text: 'Access link' }),
          h('div', { class: 'copy-row' }, h('pre', { class: 'snippet', tabindex: '0' }, linkEl), copy)),
        h('p', { class: 'alert alert-warn', text: `Anyone who has this link can open ${flat.slug} as ${res.subject} until ${dateTime(res.expires_at)}. It is not shown again.` }));
      fill(actions, close);
      close.focus();
    }

    form.addEventListener('submit', (e) => {
      e.preventDefault();
      if (!sending) busy(issue, send);
    });
    cancel.addEventListener('click', () => dlg.close());
    // A dismissal while the request runs would issue a link that nobody sees.
    dlg.addEventListener('cancel', (e) => { if (sending) e.preventDefault(); });
    dlg.addEventListener('close', () => {
      if (linkEl) linkEl.textContent = '';
      linkEl = null;
      clear(body);
      clear(dlg);
      dlg.remove();
      resolve();
    });
    document.body.appendChild(dlg);
    dlg.showModal();
    subject.focus();
  });
}
