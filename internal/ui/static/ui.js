// thread splice UI behaviour. It lives here because the page's CSP allows no
// inline script; htmx does the requests and swaps. Everything below is event
// delegation or observes swapped-in content, so nothing needs re-binding.
(() => {
  'use strict';
  const MAX_ROWS = 1000; // older rows drop off a long-running page
  const GROUP_GAP = 300; // seconds; the server groups with the same gap
  const $ = (sel, root = document) => root.querySelector(sel);
  const reduced = matchMedia('(prefers-reduced-motion: reduce)');

  // Shortcut words for this device; the stylesheet shows the matching hint.
  const mac = /mac|iphone|ipad|ipod/i.test(navigator.userAgentData?.platform || navigator.platform || '');
  document.documentElement.dataset.os = mac ? 'mac' : 'other';
  const copyKeys = mac ? '⌘C' : 'Ctrl+C';

  // Transient feedback goes through the one role=status element. Clearing it
  // first makes a repeat of the same words count as new.
  const announcer = $('#announce');
  let said;
  const announce = (msg) => {
    if (!announcer) return;
    announcer.textContent = '';
    clearTimeout(said);
    said = setTimeout(() => { announcer.textContent = msg; }, 60);
  };

  // ---- copy and reveal ----

  // A Copy button copies its data-copy-target, else the code next to it.
  const copySource = (b) => {
    if (b.dataset.copyTarget) return document.querySelector(b.dataset.copyTarget);
    const box = b.closest('.code-block, .snippet');
    return box && (box.querySelector('pre code') || box.querySelector('pre'));
  };
  const reveal = (input, show) => {
    input.type = show ? 'text' : 'password';
    document.querySelectorAll('[data-reveal="' + CSS.escape(input.id) + '"]').forEach((b) => {
      b.setAttribute('aria-pressed', String(show));
      const lbl = $('.lbl', b);
      if (lbl) lbl.textContent = show ? 'Hide' : 'Reveal';
    });
  };
  const feedback = (b, label, ok) => {
    const lbl = $('.lbl', b);
    b.classList.toggle('copied', ok);
    if (lbl) lbl.textContent = label;
    clearTimeout(b.tsTimer);
    b.tsTimer = setTimeout(() => {
      b.classList.remove('copied');
      if (lbl) lbl.textContent = 'Copy';
    }, 1500);
  };
  // Without the clipboard, select the text for the user's own shortcut.
  const selectText = (el) => {
    if (el.matches('input, textarea')) {
      if (el.type === 'password') reveal(el, true); // a masked field can't be copied from
      el.focus();
      el.select();
      return;
    }
    const r = document.createRange();
    r.selectNodeContents(el);
    getSelection().removeAllRanges();
    getSelection().addRange(r);
  };
  const copy = (b) => {
    const el = copySource(b);
    if (!el) return;
    const text = el.matches('input, textarea') ? el.value : el.textContent;
    const manual = () => {
      selectText(el);
      feedback(b, 'Press ' + copyKeys, false);
      announce('Selected. Press ' + copyKeys + ' to copy.');
    };
    if (!navigator.clipboard || !window.isSecureContext) {
      manual();
      return;
    }
    navigator.clipboard.writeText(text).then(() => {
      feedback(b, 'Copied', true);
      announce('Copied to the clipboard.');
    }, manual);
  };

  // ---- dialogs and the members drawer ----

  // Below 1200 px the members panel is a drawer over the page. While it's
  // open the page behind is dimmed and inert, so focus stays in the drawer.
  const drawer = $('#members');
  const drawerMode = matchMedia('(max-width: 1199px)');
  const drawerOpen = () => !!drawer && drawer.classList.contains('open');
  const setDrawer = (open, moveFocus = true) => {
    if (!drawer) return;
    drawer.classList.toggle('open', open);
    document.querySelectorAll('[data-drawer]').forEach((t) => t.setAttribute('aria-expanded', String(open)));
    for (const el of [$('#nav'), $('.conv')]) el?.toggleAttribute('inert', open);
    if (!moveFocus) return;
    if (open) $('#members-h')?.focus();
    else $('[data-drawer]')?.focus();
  };
  drawerMode.addEventListener('change', () => { if (!drawerMode.matches && drawerOpen()) setDrawer(false, false); });
  const openDialog = (opener) => {
    const d = document.getElementById(opener.dataset.dialog);
    if (!d || d.open) return;
    // A dialog hands focus back to whatever had it when it opened; Safari
    // doesn't focus a clicked button, so give it to the opener first.
    opener.focus({ preventScroll: true });
    d.showModal();
    // Nothing is the default for a removal: focus rests on the question.
    ($('[data-initial]', d) || d).focus();
  };

  document.addEventListener('click', (e) => {
    const t = e.target;
    let b;
    if ((b = t.closest('[data-copy]'))) return copy(b);
    if ((b = t.closest('[data-reveal]'))) {
      const input = document.getElementById(b.dataset.reveal);
      if (input) reveal(input, input.type === 'password');
      return;
    }
    if ((b = t.closest('[data-dialog]'))) return openDialog(b);
    if (t.closest('[data-close]')) return t.closest('dialog')?.close();
    if (t.closest('[data-drawer]')) return setDrawer(!drawerOpen());
    if (t.closest('[data-close-drawer]')) return setDrawer(false);
    if ((b = t.closest('a[href="#compose-text"]'))) {
      const box = $('#compose-text');
      if (box) {
        e.preventDefault();
        box.focus();
      }
      return;
    }
    // A click on a dialog's backdrop lands on the dialog itself, outside its box.
    if (t instanceof HTMLDialogElement && t.open) {
      const r = t.getBoundingClientRect();
      if (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom) t.close();
      return;
    }
    document.querySelectorAll('details.switcher[open]').forEach((d) => { if (!d.contains(t)) d.open = false; });
  });
  // With the drawer open the page behind is dimmed: a click there closes the
  // drawer and does nothing else (capture phase, before the target sees it).
  document.addEventListener('click', (e) => {
    const t = e.target;
    if (!drawerOpen() || drawer.contains(t) || t.closest('[data-drawer], dialog') || getComputedStyle(drawer).position !== 'fixed') return;
    e.preventDefault();
    e.stopPropagation();
    setDrawer(false);
  }, true);

  document.addEventListener('keydown', (e) => {
    const t = e.target;
    // Ctrl/Cmd+Enter in a textarea submits its form (through htmx when it has hx-post).
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey) && t.matches('textarea') && t.form) {
      e.preventDefault();
      t.form.requestSubmit();
      return;
    }
    if (e.key !== 'Escape' || $('dialog[open]')) return; // a dialog closes itself on Esc
    const sw = $('details.switcher[open]');
    if (sw) {
      sw.open = false;
      $('summary', sw).focus();
    } else if (drawerOpen()) {
      setDrawer(false);
    }
  });

  // ---- the conversation ----

  const feed = $('#feed');
  const scroll = $('#scroll');
  const pill = $('#new-msgs');

  const snippet = (s) => {
    s = s.replace(/\s+/g, ' ').trim();
    return s.length > 60 ? s.slice(0, 60).trimEnd() + '…' : s;
  };
  // link does for a row that arrived later what the server did for the rows
  // it sent together: group it under the header above it when it continues
  // the same sender's run, and tie a reply to its ask.
  const link = (row) => {
    const prev = row.previousElementSibling;
    const cont = !!prev && prev.matches('.msg') && !prev.matches('.bad') && !row.matches('.bad') &&
      !!row.dataset.g && prev.dataset.g === row.dataset.g && Math.abs(row.dataset.t - prev.dataset.t) < GROUP_GAP;
    row.classList.toggle('cont', cont);
    const asked = row.dataset.re && document.getElementById('m-' + row.dataset.re);
    if (!asked || asked === row) return;
    asked.classList.add('answered');
    const q = $('[data-quote]', row);
    const text = $('.bubble .text, .bubble .code code', asked);
    if (q && text) {
      q.textContent = '“' + snippet(text.textContent) + '”';
      q.removeAttribute('data-quote');
    }
  };

  // The getting-started steps tick off as members and messages arrive.
  const steps = () => {
    const list = $('#checklist');
    if (!list) return;
    const panel = $('#participants');
    const done = (id, yes) => { if (yes) document.getElementById(id)?.classList.add('done'); };
    if (panel) {
      done('step-members', Number(panel.dataset.others) > 0);
      done('step-session', Number(panel.dataset.sessions) > 0);
    }
    done('step-message', !!feed?.querySelector('.msg'));
    if (!list.querySelector('.step:not(.done)') && !list.classList.contains('all-done')) {
      list.classList.add('all-done');
      announce('All set: every getting-started step is done.');
    }
  };

  if (feed && scroll) {
    // The feed scrolls inside its column, or with the page on narrow and short windows.
    const box = () => (getComputedStyle(scroll).overflowY === 'visible' ? document.scrollingElement : scroll);
    const atBottom = () => {
      const b = box();
      return b.scrollHeight - b.scrollTop - b.clientHeight < 80;
    };
    const toBottom = (smooth) => {
      const b = box();
      b.scrollTo({ top: b.scrollHeight, behavior: smooth && !reduced.matches ? 'smooth' : 'auto' });
    };
    let follow = true;
    const onScroll = () => {
      follow = atBottom();
      if (follow && pill) pill.hidden = true;
    };
    scroll.addEventListener('scroll', onScroll, { passive: true });
    addEventListener('scroll', onScroll, { passive: true });
    // Follow only while the reader is at the bottom; the composer growing or
    // the window resizing mustn't leave them stranded above it.
    new ResizeObserver(() => { if (follow) toBottom(false); }).observe(scroll);
    toBottom(false);
    new MutationObserver((records) => {
      let fresh = false;
      for (const r of records) {
        for (const n of r.addedNodes) {
          if (n.nodeType === 1 && n.matches('.msg')) {
            link(n);
            fresh = true;
          }
        }
      }
      const rows = feed.getElementsByClassName('msg');
      while (rows.length > MAX_ROWS) rows[0].remove();
      if (!fresh) return;
      steps();
      if (follow) toBottom(false);
      else if (pill) pill.hidden = false;
    }).observe(feed, { childList: true });
    pill?.addEventListener('click', () => {
      toBottom(true);
      pill.hidden = true;
      announce('Showing the newest messages.');
    });
  }

  // ---- swapped-in content ----

  // The compose form's recipients follow the members panel. The chosen
  // recipient always stays, so a direct message never turns into one for everyone.
  const recipients = (panel) => {
    const to = $('#compose select[name=to]');
    if (!to || panel.querySelector(':scope > .error')) return;
    const opts = [to.options[0], ...[...panel.querySelectorAll('.tree [data-to]')].map((li) => new Option(li.dataset.name, li.dataset.to))];
    if (to.selectedIndex > 0 && !opts.some((o) => o.value === to.value)) opts.push(to.selectedOptions[0]);
    const key = (os) => [...os].map((o) => o.value + '\n' + o.text).join('\n');
    if (key(opts) === key(to.options)) return;
    const value = to.value;
    to.replaceChildren(...opts);
    to.value = value;
  };

  let refocus = false; // after a removal, focus goes back to the members list
  let sessions = ($('#sessions')?.dataset.names || '').split(' ').filter(Boolean);
  document.addEventListener('htmx:load', (e) => {
    const el = e.detail.elt;
    if (el.id === 'participants') {
      recipients(el);
      steps();
      if (refocus) {
        refocus = false;
        $('#members-h')?.focus();
      }
    } else if (el.id === 'sessions') {
      const now = (el.dataset.names || '').split(' ').filter(Boolean);
      now.filter((n) => !sessions.includes(n)).forEach((n) => announce(n + ' joined this room.'));
      sessions = now;
    } else if (el.matches('.invite') && el.parentElement?.id === 'invite-result') {
      announce('Invite created. It is masked; Copy copies it as it is.');
    }
  });

  // ---- requests ----

  // A tab nobody looks at stops polling: the relay counts every poll as the
  // member being online. So does a page with a dialog open, which a refresh
  // of the panel around it would close. Coming back catches up at once.
  // Submitting a form again clears the notice the last attempt left.
  document.addEventListener('htmx:beforeRequest', (e) => {
    const elt = e.detail.elt;
    if (e.detail.requestConfig?.verb === 'get' && (document.hidden || $('dialog[open]'))) {
      e.preventDefault();
      return;
    }
    if (elt.closest?.('dialog')) refocus = true;
    const n = $('#notice');
    if (n && elt.matches('form')) n.textContent = '';
  });
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) document.body.dispatchEvent(new Event('ts-sent')); // the feed's poll-now trigger
  });

  // Say so when the UI server is gone, or was restarted: a new run has a new
  // link, and until it is opened every answer is a 401. Those are never
  // swapped in, so this page stays as it was and picks up again once the new
  // link has been opened in this browser.
  // (htmx:afterRequest can't be used: elements that swap themselves out with
  // outerHTML, like the pollers, are detached by then and it doesn't bubble.)
  const offline = $('#offline');
  const expired = $('#expired');
  if (offline && expired) {
    document.addEventListener('htmx:sendError', () => { offline.hidden = false; });
    document.addEventListener('htmx:beforeSwap', (e) => {
      const gone = e.detail.xhr.status === 401;
      offline.hidden = true;
      expired.hidden = !gone;
      if (gone) e.detail.shouldSwap = false;
    });
  }

  // Plain (non-htmx) forms create rooms and join them: show that it's
  // working, and block double submits.
  document.addEventListener('submit', (e) => {
    const f = e.target;
    if (f.hasAttribute('hx-post')) return;
    f.classList.add('busy');
    setTimeout(() => f.querySelectorAll('button').forEach((b) => { b.disabled = true; }));
  });
  addEventListener('pageshow', (e) => {
    if (!e.persisted) return;
    document.querySelectorAll('form.busy').forEach((f) => f.classList.remove('busy'));
    document.querySelectorAll('form button:disabled').forEach((b) => { b.disabled = false; });
  });

  // ---- fields ----

  document.addEventListener('input', (e) => {
    const t = e.target;
    // "Sessions you connect join as <name>.<label>" follows the name typed.
    if (t.matches('[data-machine-name]')) {
      t.form?.querySelectorAll('[data-machine-echo]').forEach((s) => { s.textContent = t.value.trim() || 'name'; });
    }
    // The composer grows with its text where CSS field-sizing can't do it.
    if (t.matches('textarea[data-grow]') && !CSS.supports('field-sizing', 'content')) {
      t.style.height = 'auto';
      t.style.height = Math.min(t.scrollHeight + 2, innerHeight * 0.4) + 'px';
    }
  });
})();
