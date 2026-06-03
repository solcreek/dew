/* =========================================================================
   Dew landing — interactions: theme, animated terminals, reveals, copy, tabs.
   Vanilla JS. The Tweaks panel (React island) calls into the globals at bottom.
   ========================================================================= */
(function () {
  'use strict';

  var root = document.documentElement;
  var motionOn = function () {
    return root.getAttribute('data-motion') !== 'off' &&
      !window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  };

  /* ---------------------------------------------------------- THEME */
  function applyTheme(theme) {
    root.setAttribute('data-theme', theme);
    try { localStorage.setItem('dew-theme', theme); } catch (e) {}
  }
  (function initTheme() {
    var saved;
    try { saved = localStorage.getItem('dew-theme'); } catch (e) {}
    if (saved) root.setAttribute('data-theme', saved);
  })();
  var tt = document.getElementById('theme-toggle');
  if (tt) tt.addEventListener('click', function () {
    applyTheme(root.getAttribute('data-theme') === 'dark' ? 'light' : 'dark');
  });

  /* ---------------------------------------------------------- COPY BUTTONS */
  document.querySelectorAll('[data-copy]').forEach(function (box) {
    var btn = box.querySelector('.copy');
    if (!btn) return;
    btn.addEventListener('click', function () {
      var text = box.getAttribute('data-copy');
      navigator.clipboard && navigator.clipboard.writeText(text);
      var lbl = btn.querySelector('.lbl');
      var prev = lbl ? lbl.textContent : '';
      btn.classList.add('done');
      if (lbl) lbl.textContent = 'Copied';
      setTimeout(function () { btn.classList.remove('done'); if (lbl) lbl.textContent = prev; }, 1400);
    });
  });

  /* ---------------------------------------------------------- INSTALL TABS */
  var INSTALL = {
    mac: '$ curl -fsSL https://dewvm.dev/install.sh | sh',
    win: '$ irm https://dewvm.dev/install.ps1 | iex',
    npm: '$ npx dew --help'
  };
  var tabs = document.getElementById('install-tabs');
  if (tabs) {
    var cmdEl = document.getElementById('install-cmd');
    var copyEl = document.getElementById('install-copy');
    tabs.querySelectorAll('.tab').forEach(function (tab) {
      tab.addEventListener('click', function () {
        tabs.querySelectorAll('.tab').forEach(function (t) { t.classList.remove('active'); });
        tab.classList.add('active');
        var os = tab.getAttribute('data-os');
        var full = INSTALL[os];
        var visible = full.replace(/^\$ /, '');
        cmdEl.innerHTML = '<span class="sigil">$ </span>' + visible.replace(/&/g, '&amp;').replace(/</g, '&lt;');
        copyEl.setAttribute('data-copy', visible);
      });
    });
  }

  /* ---------------------------------------------------------- SCROLL REVEALS */
  var io = ('IntersectionObserver' in window) ? new IntersectionObserver(function (entries) {
    entries.forEach(function (e) {
      if (e.isIntersecting) { e.target.classList.add('is-in'); io.unobserve(e.target); }
    });
  }, { threshold: 0.12, rootMargin: '0px 0px -8% 0px' }) : null;
  document.querySelectorAll('.reveal:not(.is-in)').forEach(function (el) {
    if (io && motionOn()) io.observe(el); else el.classList.add('is-in');
  });
  // Belt-and-suspenders: reveal anything already within the viewport, on load + scroll.
  function revealInView() {
    if (!motionOn()) { document.querySelectorAll('.reveal:not(.is-in)').forEach(function (el) { el.classList.add('is-in'); }); return; }
    var h = window.innerHeight || document.documentElement.clientHeight;
    document.querySelectorAll('.reveal:not(.is-in)').forEach(function (el) {
      var r = el.getBoundingClientRect();
      if (r.top < h * 0.92 && r.bottom > 0) { el.classList.add('is-in'); if (io) io.unobserve(el); }
    });
  }
  window.addEventListener('scroll', revealInView, { passive: true });
  window.addEventListener('resize', revealInView, { passive: true });
  revealInView();

  /* ---------------------------------------------------------- TERMINAL ENGINE */
  // line: { p:true } => prompt+command typed; otherwise output. cls = span class.
  var SCRIPTS = {
    split: [
      { cmd: 'dew up' },
      { out: 'detected: vite (npm)', cls: 'c-mut', d: 360 },
      { out: '✓ vm ready · 30ms', cls: 'c-ok', d: 520 },
      { out: '✓ deps installed', cls: 'c-ok', d: 420 },
      { out: '✓ ', cls: 'c-ok', url: 'http://localhost:5173', d: 360 }
    ],
    main: [
      { cmd: 'dew up' },
      { out: 'detected: vite (npm)', cls: 'c-mut', d: 340 },
      { out: '✓ vm ready  ', cls: 'c-ok', tail: 'profile node · 31 MB', d: 480 },
      { out: '✓ deps installed', cls: 'c-ok', d: 420 },
      { out: '✓ hot reload wired to editor', cls: 'c-ok', d: 420 },
      { out: '', d: 120 },
      { out: '  ➜  local:   ', cls: 'c-mut', url: 'http://localhost:5173', d: 320 },
      { out: '  ➜  network: ', cls: 'c-mut', tail: 'use --network to expose', d: 160 }
    ],
    devenv: [
      { cmd: 'dew up --with postgres' },
      { out: 'detected: next.js (npm)', cls: 'c-mut', d: 360 },
      { out: '✓ vm ready · 30ms', cls: 'c-ok', d: 460 },
      { out: '✓ postgres:16 attached  ', cls: 'c-ok', tail: '→ :5432', d: 520 },
      { out: 'installing deps', cls: 'c-mut', d: 360 },
      { out: '✓ deps installed', cls: 'c-ok', d: 360 },
      { out: '', d: 100 },
      { out: '✓ ', cls: 'c-ok', url: 'http://localhost:3000', d: 320 }
    ]
  };

  function esc(s) { return (s || '').replace(/&/g, '&amp;').replace(/</g, '&lt;'); }

  function renderFinal(body, script) {
    var html = '';
    script.forEach(function (step) {
      if (step.cmd != null) {
        html += '<div><span class="c-prompt">$</span> <span class="c-cmd">' + esc(step.cmd) + '</span></div>';
      } else {
        html += '<div><span class="' + (step.cls || '') + '">' + esc(step.out) + '</span>' +
          (step.url ? '<span class="c-url">' + esc(step.url) + '</span>' : '') +
          (step.tail ? '<span class="c-mut">' + esc(step.tail) + '</span>' : '') + '</div>';
      }
    });
    html += '<span class="cursor blink"></span>';
    body.innerHTML = html;
  }

  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }

  function animate(body, script, token) {
    body.innerHTML = '';
    var cur = document.createElement('div');
    body.appendChild(cur);
    var cursor = document.createElement('span');
    cursor.className = 'cursor blink';

    function alive() { return body.__token === token; }

    return (async function () {
      for (var i = 0; i < script.length && alive(); i++) {
        var step = script[i];
        var line = document.createElement('div');
        body.appendChild(line);
        if (step.cmd != null) {
          line.innerHTML = '<span class="c-prompt">$</span> <span class="c-cmd"></span>';
          var dst = line.querySelector('.c-cmd');
          dst.appendChild(cursor);
          for (var c = 0; c < step.cmd.length && alive(); c++) {
            dst.insertBefore(document.createTextNode(step.cmd[c]), cursor);
            await sleep(34 + Math.random() * 36);
          }
          await sleep(380);
        } else {
          var inner = '<span class="' + (step.cls || '') + '">' + esc(step.out) + '</span>';
          if (step.url) inner += '<span class="c-url">' + esc(step.url) + '</span>';
          if (step.tail) inner += '<span class="c-mut">' + esc(step.tail) + '</span>';
          line.innerHTML = inner;
          line.appendChild(cursor);
          await sleep(step.d || 300);
        }
      }
      if (cur && cur.parentNode === body && !cur.textContent) body.removeChild(cur);
    })();
  }

  function play(termEl) {
    var body = termEl.querySelector('[data-term-body]');
    var key = termEl.getAttribute('data-term');
    var script = SCRIPTS[key];
    if (!body || !script) return;
    body.__token = (body.__token || 0) + 1;
    var token = body.__token;
    if (!motionOn()) { renderFinal(body, script); return; }
    animate(body, script, token);
  }
  window.__dewPlayTerm = play;

  // Animate terminals when they scroll into view (once). Hero terminal plays on load.
  var termObserver = ('IntersectionObserver' in window) ? new IntersectionObserver(function (entries) {
    entries.forEach(function (e) {
      if (e.isIntersecting && !e.target.__played) {
        e.target.__played = true;
        play(e.target);
        termObserver.unobserve(e.target);
      }
    });
  }, { threshold: 0.3 }) : null;

  function bootTerminals() {
    document.querySelectorAll('.term[data-term]').forEach(function (t) {
      // play the visible hero terminal right away; observe the rest
      var hero = document.getElementById('hero');
      var inHero = hero && hero.contains(t);
      var visible = t.offsetParent !== null;
      if (inHero && visible) { t.__played = true; play(t); }
      else if (termObserver) termObserver.observe(t);
      else { t.__played = true; play(t); }
    });
  }
  if (document.readyState !== 'loading') bootTerminals();
  else document.addEventListener('DOMContentLoaded', bootTerminals);

  /* ---------------------------------------------------------- HERO SWITCH (for Tweaks) */
  window.__dewSetHero = function (dir) {
    var hero = document.getElementById('hero');
    if (!hero) return;
    hero.setAttribute('data-hero', dir);
    // play whichever terminal is now visible in the hero
    requestAnimationFrame(function () {
      hero.querySelectorAll('.term[data-term]').forEach(function (t) {
        if (t.offsetParent !== null) { t.__played = true; play(t); }
      });
    });
  };
  window.__dewSetTheme = applyTheme;
  window.__dewSetMotion = function (on) {
    root.setAttribute('data-motion', on ? 'on' : 'off');
    if (on) {
      document.querySelectorAll('.term[data-term]').forEach(function (t) {
        if (t.offsetParent !== null) play(t);
      });
    }
  };
  window.__dewSetAccent = function (hex) {
    document.documentElement.style.setProperty('--lime', hex);
  };
})();
