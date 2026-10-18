// thread splice UI behaviour. It lives here because the page's CSP allows no
// inline script; htmx does the requests and swaps.
(() => {
  'use strict';
  const MAX_ROWS = 1000; // older rows drop off a long-running page

  // Feed: follow new messages only while the reader is at the bottom.
  const feed = document.getElementById('feed');
  if (feed) {
    let follow = true;
    feed.scrollTop = feed.scrollHeight;
    feed.addEventListener('scroll', () => {
      follow = feed.scrollHeight - feed.scrollTop - feed.clientHeight < 48;
    }, { passive: true });
    new MutationObserver(() => {
      while (feed.children.length > MAX_ROWS) feed.firstElementChild.remove();
      if (follow) feed.scrollTop = feed.scrollHeight;
    }).observe(feed, { childList: true });
  }

  // Copy buttons: <button data-copy="element id">.
  const select = (el) => {
    if (el.select) { el.select(); return; }
    const r = document.createRange();
    r.selectNodeContents(el);
    getSelection().removeAllRanges();
    getSelection().addRange(r);
  };
  document.addEventListener('click', (e) => {
    const b = e.target.closest('[data-copy]');
    const el = b && document.getElementById(b.dataset.copy);
    if (!el) return;
    const text = el.matches('input, textarea') ? el.value : el.textContent;
    const label = (s) => { b.textContent = s; setTimeout(() => { b.textContent = 'Copy'; }, 1600); };
    const manual = () => { select(el); label('Press Ctrl+C'); };
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(() => label('Copied'), manual);
    } else {
      manual();
    }
  });

  // Ctrl/Cmd+Enter in a textarea submits its form (through htmx when it has hx-post).
  document.addEventListener('keydown', (e) => {
    const t = e.target;
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey) && t.matches('textarea') && t.form) {
      e.preventDefault();
      t.form.requestSubmit();
    }
  });

  // Plain (non-htmx) forms create rooms and identities: block double submits.
  document.addEventListener('submit', (e) => {
    const f = e.target;
    if (f.hasAttribute('hx-post')) return;
    setTimeout(() => f.querySelectorAll('button').forEach((b) => { b.disabled = true; }));
  });
  addEventListener('pageshow', (e) => {
    if (e.persisted) document.querySelectorAll('form button:disabled').forEach((b) => { b.disabled = false; });
  });

  // Say so when the UI server is gone, or was restarted: a new run has a new
  // link, and until it is opened every answer is a 401. Those are never
  // swapped in, so this page stays as it was and picks up again once the new
  // link has been opened in this browser.
  // (htmx:afterRequest can't be used: elements that swap themselves out with
  // outerHTML, like the pollers, are detached by then and it doesn't bubble.)
  const offline = document.getElementById('offline');
  const expired = document.getElementById('expired');
  if (offline && expired) {
    document.addEventListener('htmx:sendError', () => { offline.hidden = false; });
    document.addEventListener('htmx:beforeSwap', (e) => {
      const gone = e.detail.xhr.status === 401;
      offline.hidden = true;
      expired.hidden = !gone;
      if (gone) e.detail.shouldSwap = false;
    });
  }

  // A tab nobody looks at stops polling: the relay counts every poll as the
  // identity being online. Coming back catches up at once. Submitting a form
  // again clears the notice the last attempt left.
  document.addEventListener('htmx:beforeRequest', (e) => {
    if (document.hidden && e.detail.requestConfig?.verb === 'get') {
      e.preventDefault();
      return;
    }
    const n = document.getElementById('notice');
    if (n && e.detail.elt.matches('form')) n.textContent = '';
  });
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) document.body.dispatchEvent(new Event('ts-sent')); // the feed's poll-now trigger
  });

  // Forms marked data-reset start over after a success (adding an agent).
  document.addEventListener('htmx:afterRequest', (e) => {
    if (e.detail.successful && e.detail.elt.matches('form[data-reset]')) e.detail.elt.reset();
  });

  // The compose form's recipients follow the participants panel. The chosen
  // recipient always stays, so a direct message never turns into a room-wide one.
  document.addEventListener('htmx:load', (e) => {
    const panel = e.detail.elt;
    const to = document.querySelector('#compose select[name=to]');
    if (panel.id !== 'participants' || !to || !panel.querySelector('.peer')) return;
    const opts = [to.options[0], ...[...panel.querySelectorAll('[data-to]')].map((li) => new Option(li.dataset.name, li.dataset.to))];
    if (to.selectedIndex > 0 && !opts.some((o) => o.value === to.value)) opts.push(to.selectedOptions[0]);
    const key = (os) => [...os].map((o) => o.value + '\n' + o.text).join('\n');
    if (key(opts) === key(to.options)) return;
    const value = to.value;
    to.replaceChildren(...opts);
    to.value = value;
  });
})();
