(function () {
  'use strict';

  var $ = function (id) { return document.getElementById(id); };
  var enc = new TextEncoder();

  var TermCtor = window.Terminal.Terminal || window.Terminal;
  var FitCtor = window.FitAddon.FitAddon || window.FitAddon;

  var term = new TermCtor({
    cursorBlink: true,
    fontSize: 14,
    scrollback: 5000,
    theme: { background: '#0d1117', foreground: '#e6edf3' }
  });
  var fit = new FitCtor();
  term.loadAddon(fit);
  term.open($('term'));

  // ---- connection state -------------------------------------------------
  var ws = null;
  var retry = 0;
  var retryTimer = null;

  function setStatus(kind, text) {
    $('dot').className = 'dot ' + kind;
    $('status').textContent = text;
  }

  function send(bytes) {
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(bytes);
  }

  function sendText(str) { send(enc.encode(str)); }

  function sendCtrl(obj) {
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(obj));
  }

  function sendSize() {
    if (term.cols > 0 && term.rows > 0) {
      sendCtrl({ type: 'resize', cols: term.cols, rows: term.rows });
    }
  }

  function connect() {
    clearTimeout(retryTimer);
    setStatus('wait', 'подключение…');
    var proto = location.protocol === 'https:' ? 'wss://' : 'ws://';
    var sock = new WebSocket(proto + location.host + '/ws');
    sock.binaryType = 'arraybuffer';
    ws = sock;

    sock.onopen = function () {
      retry = 0;
      term.reset(); // the server replays its buffer right away
      fitNow();
      sendSize();
      setStatus('on', 'подключено');
    };

    sock.onmessage = function (ev) {
      if (typeof ev.data === 'string') {
        var m;
        try { m = JSON.parse(ev.data); } catch (e) { return; }
        handleControl(m);
      } else {
        term.write(new Uint8Array(ev.data));
      }
    };

    sock.onclose = function () {
      if (ws !== sock) return;
      ws = null;
      setStatus('off', 'нет соединения');
      scheduleReconnect();
    };
  }

  function handleControl(m) {
    switch (m.type) {
      case 'status':
        if (m.state === 'exited') setStatus('off', 'процесс завершён (' + m.code + ')');
        break;
      case 'reset': term.reset(); setStatus('on', 'перезапущено'); break;
      case 'exit': setStatus('off', 'процесс завершён (' + m.code + ')'); break;
      case 'error': setStatus('off', 'не удалось запустить процесс'); break;
    }
  }

  function scheduleReconnect() {
    var delay = Math.min(30000, 1000 * Math.pow(2, retry++));
    // A rejected handshake looks like any other failure to the browser; ask
    // the server whether the session cookie is still valid.
    fetch('/', { redirect: 'manual', cache: 'no-store' }).then(function (r) {
      if (r.type === 'opaqueredirect' || r.status === 401) {
        location.href = '/login';
      }
    }).catch(function () { /* offline; just keep retrying */ });
    setStatus('off', 'нет соединения, повтор через ' + Math.round(delay / 1000) + ' с');
    retryTimer = setTimeout(connect, delay);
  }

  // ---- input --------------------------------------------------------------
  var ctrlOn = false;

  function setCtrl(on) {
    ctrlOn = on;
    $('key-ctrl').classList.toggle('on', on);
  }

  // Sticky Ctrl turns the next single character into its control code.
  function applyCtrl(s) {
    if (ctrlOn && s.length === 1) {
      var c = s.toUpperCase().charCodeAt(0);
      if (c >= 0x40 && c <= 0x5f) { setCtrl(false); return String.fromCharCode(c & 0x1f); }
      if (s === ' ') { setCtrl(false); return '\x00'; }
    }
    return s;
  }

  term.onData(function (s) { sendText(applyCtrl(s)); });
  term.onBinary(function (s) {
    var b = new Uint8Array(s.length);
    for (var i = 0; i < s.length; i++) b[i] = s.charCodeAt(i) & 0xff;
    send(b);
  });
  term.onResize(sendSize);

  var SEQ = {
    esc: '\x1b', tab: '\t', enter: '\r',
    'c-c': '\x03', 'c-d': '\x04',
    up: '\x1b[A', down: '\x1b[B', right: '\x1b[C', left: '\x1b[D'
  };

  // pointerdown + preventDefault keeps focus in the terminal so the
  // on-screen keyboard does not collapse when a key button is tapped.
  $('keys').addEventListener('pointerdown', function (ev) {
    var b = ev.target.closest('button');
    if (!b) return;
    ev.preventDefault();
    var k = b.getAttribute('data-seq');
    if (k === 'ctrl') { setCtrl(!ctrlOn); return; }
    // Local scrolling through the xterm scrollback; nothing is sent to the process.
    if (k === 'scroll-up') { scrollPage(-1); return; }
    if (k === 'scroll-down') { scrollPage(1); return; }
    if (k === 'scroll-end') { if (!inAltScreen()) term.scrollToBottom(); return; }
    sendText(SEQ[k] !== undefined ? SEQ[k] : applyCtrl(k));
    term.focus();
  });

  // ---- scrolling ------------------------------------------------------------
  // A full-screen app (alternate screen) has no scrollback in xterm, so do what
  // a real terminal does: send it mouse-wheel events if it asked for mouse
  // tracking, otherwise arrow / PageUp / PageDown keys. In the normal screen
  // just scroll xterm's own history.
  function inAltScreen() { return term.buffer.active.type === 'alternate'; }
  function mouseTracking() { return term.modes && term.modes.mouseTrackingMode !== 'none'; }

  function scrollLines(n) { // n < 0: toward older output
    if (n === 0) return;
    if (!inAltScreen()) { term.scrollLines(n); return; }
    var up = n < 0, count = Math.abs(n);
    if (mouseTracking()) {
      var seq = '[<' + (up ? 64 : 65) + ';' + Math.ceil(term.cols / 2) + ';' + Math.ceil(term.rows / 2) + 'M';
      for (var i = 0; i < count; i++) sendText(seq);
    } else {
      for (var j = 0; j < count; j++) sendText(up ? '[A' : '[B');
    }
  }

  function scrollPage(dir) { // dir: -1 older, 1 newer
    if (!inAltScreen()) { term.scrollPages(dir); return; }
    if (mouseTracking()) { scrollLines(dir * 5); return; }
    sendText(dir < 0 ? '[5~' : '[6~');
  }

  // ---- touch scrolling ------------------------------------------------------
  // Dragging a finger over the terminal scrolls the scrollback. Handled in the
  // capture phase so xterm's own touch handling does not also act on it.
  // A tap (little movement) is left alone so it still focuses the terminal.
  var touchY = null;
  var touchMoved = false;
  var termEl = $('term');
  termEl.addEventListener('touchstart', function (ev) {
    if (ev.touches.length !== 1) { touchY = null; return; }
    touchY = ev.touches[0].clientY;
    touchMoved = false;
  }, { capture: true, passive: true });
  termEl.addEventListener('touchmove', function (ev) {
    if (touchY === null || ev.touches.length !== 1) return;
    var lineH = Math.max(8, termEl.clientHeight / Math.max(1, term.rows));
    var dy = ev.touches[0].clientY - touchY;
    if (!touchMoved && Math.abs(dy) < 8) return;
    touchMoved = true;
    ev.preventDefault();
    ev.stopPropagation();
    var lines = Math.trunc(dy / lineH);
    if (lines !== 0) {
      scrollLines(-Math.max(-20, Math.min(20, lines))); // finger down = older output
      touchY += lines * lineH;
    }
  }, { capture: true, passive: false });
  termEl.addEventListener('touchend', function () { touchY = null; }, { capture: true, passive: true });

  // ---- layout -------------------------------------------------------------
  var fitPending = false;
  function fitNow() {
    try { fit.fit(); } catch (e) { /* not laid out yet */ }
  }
  function scheduleFit() {
    if (fitPending) return;
    fitPending = true;
    requestAnimationFrame(function () { fitPending = false; fitNow(); });
  }
  function onViewport() {
    var vv = window.visualViewport;
    if (vv) document.documentElement.style.setProperty('--vvh', vv.height + 'px');
    scheduleFit();
  }
  if (window.visualViewport) {
    window.visualViewport.addEventListener('resize', onViewport);
    window.visualViewport.addEventListener('scroll', onViewport);
  }
  window.addEventListener('resize', onViewport);
  window.addEventListener('orientationchange', onViewport);
  onViewport();

  // ---- restart ------------------------------------------------------------
  $('btn-restart').addEventListener('click', function () {
    if (confirm('Перезапустить процесс? Текущая сессия будет завершена.')) {
      sendCtrl({ type: 'restart' });
    }
  });

  // ---- screenshot overlay -------------------------------------------------
  var shotTimer = null;

  function refreshShot() {
    var img = $('shot-img');
    $('shot-msg').textContent = 'загрузка…';
    img.onload = function () { $('shot-msg').textContent = ''; };
    img.onerror = function () { $('shot-msg').textContent = 'не удалось получить скриншот'; };
    // The cache-buster is just a counter; the server sends no-store anyway.
    img.src = '/screenshot?t=' + Date.now();
  }

  function setAuto(on) {
    clearInterval(shotTimer);
    shotTimer = on ? setInterval(refreshShot, 5000) : null;
  }

  function closeShot() {
    setAuto(false);
    $('shot-auto').checked = false;
    $('shot').hidden = true;
    $('shot-img').removeAttribute('src');
    term.focus();
  }

  $('btn-screen').addEventListener('click', function () {
    $('shot').hidden = false;
    refreshShot();
  });
  $('shot-refresh').addEventListener('click', refreshShot);
  $('shot-auto').addEventListener('change', function (ev) { setAuto(ev.target.checked); });
  $('shot-close').addEventListener('click', closeShot);
  $('shot-view').addEventListener('click', closeShot); // tap the picture to close

  // Reconnect promptly when the phone wakes up or regains the network.
  document.addEventListener('visibilitychange', function () {
    if (!document.hidden && !ws) { retry = 0; connect(); }
  });
  window.addEventListener('online', function () { if (!ws) { retry = 0; connect(); } });

  connect();
})();
