// app.js — SPA client-side router for the admin console.
//
// Loaded SYNCHRONOUSLY in <head> (before page scripts) so that `DSHNav` is
// defined by the time a page's own scripts run. It intercepts clicks on
// `.nav-links` anchors that point at console pages and, instead of a full page
// reload, fetches the target page and swaps only `<main id="view">` plus the
// page's scripts. `history.pushState` keeps the URL and browser back/forward
// working; a lost session falls back to a normal reload of /login.
//
// Lifecycle: a page may register a cleanup callback via `DSHNav.onLeave(fn)`
// (e.g. to close its EventSource / interval) which is invoked immediately
// before the router swaps to the next page. This keeps resources from leaking
// as the user navigates around without full reloads.
(function () {
  'use strict';
  if (window.DSHNav) return; // idempotent: never re-init on SPA swaps

  var SPA_PATHS = ['/dashboard', '/config', '/files', '/deploy', '/services', '/system'];
  // Scripts that are global/idempotent and must NOT be re-executed on a swap.
  var IGNORED = ['app.js', 'dialog.js'];

  var leave = []; // pending "leave" cleanup callbacks

  function gotoLogin() { window.location.replace('/login'); }

  function setActive(path) {
    document.querySelectorAll('.nav-links a').forEach(function (a) {
      var href = a.getAttribute('href') || '';
      a.classList.toggle('active', href === path);
    });
  }

  // ---- route progress bar ----
  function ensureBar() {
    var b = document.getElementById('routeBar');
    if (b) return b;
    b = document.createElement('div');
    b.id = 'routeBar';
    b.className = 'route-bar';
    document.body.appendChild(b);
    return b;
  }
  function beginNav() {
    var b = ensureBar();
    b.classList.remove('on', 'done');
    // force reflow so the transition actually runs
    void b.offsetWidth;
    b.classList.add('on');
  }
  function endNav() {
    var b = ensureBar();
    b.classList.add('done');
    setTimeout(function () { b.classList.remove('on', 'done'); }, 600);
  }

  function fireLeave() {
    var pending = leave;
    leave = [];
    pending.forEach(function (fn) { try { fn(); } catch (e) {} });
  }

  // Extract the page scripts we must re-run (skipping idempotent/global ones),
  // preserving their source order.
  function stealScripts(doc) {
    var out = [];
    doc.querySelectorAll('script').forEach(function (s) {
      var src = s.getAttribute('src') || '';
      for (var i = 0; i < IGNORED.length; i++) {
        if (src.indexOf(IGNORED[i]) >= 0) return;
      }
      out.push({ src: src, text: s.textContent || '' });
    });
    return out;
  }

  // Execute scripts sequentially. External scripts fire onload/onerror before
  // the next one runs; inline scripts run immediately.
  function execScripts(scripts, done) {
    var i = 0;
    function next() {
      if (i >= scripts.length) { done && done(); return; }
      var s = scripts[i++];
      if (s.src) {
        var el = document.createElement('script');
        el.src = s.src;
        el.onload = next;
        el.onerror = next;
        document.body.appendChild(el);
      } else {
        var inl = document.createElement('script');
        inl.textContent = s.text;
        document.body.appendChild(inl);
        next();
      }
    }
    next();
  }

  function load(url, push) {
    beginNav();
    fetch(url, { credentials: 'same-origin', headers: { 'X-Requested-With': 'fetch' } })
      .then(function (resp) { return resp.text(); })
      .then(function (html) {
        var doc = new DOMParser().parseFromString(html, 'text/html');
        // If the session expired the server 302'd us to the login page; detect
        // it by the auth layout and do a full reload so cookies reset cleanly.
        var bodyCls = doc.body ? String(doc.body.className) : '';
        if (bodyCls.indexOf('auth-body') >= 0) { gotoLogin(); return; }

        var newMain = doc.querySelector('main');
        var view = document.getElementById('view');
        if (!newMain || !view) { gotoLogin(); return; }

        var scripts = stealScripts(doc);
        fireLeave();
        view.innerHTML = newMain.innerHTML;
        // re-trigger a subtle enter transition on the swapped content
        view.classList.remove('v-enter');
        void view.offsetWidth;
        view.classList.add('v-enter');
        document.title = doc.title || document.title;
        setActive(url.split('?')[0]);
        if (push) history.pushState({ path: url }, '', url);
        window.scrollTo(0, 0);
        execScripts(scripts, endNav);
      })
      .catch(function () { endNav(); });
  }

  window.DSHNav = {
    onLeave: function (fn) { leave.push(fn); }
  };

  document.addEventListener('click', function (e) {
    var a = e.target && e.target.closest ? e.target.closest('a') : null;
    if (!a) return;
    var href = a.getAttribute('href');
    if (!href) return;
    if (href.indexOf('http') === 0 || href.charAt(0) !== '/') return;
    if (href === '/logout') return; // full navigation clears the session
    if (SPA_PATHS.indexOf(href) === -1) return; // downloads/other = default
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return; // new tab
    e.preventDefault();
    load(href, true);
  });

  window.addEventListener('popstate', function () {
    var p = location.pathname;
    if (SPA_PATHS.indexOf(p) !== -1) load(p, false);
  });
})();
