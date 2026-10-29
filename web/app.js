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
    theme: { background: '#0a0d14', foreground: '#e8ecf4', cursor: '#6d8dff', selectionBackground: 'rgba(109,141,255,0.35)' }
  });
  var fit = new FitCtor();
  term.loadAddon(fit);
  term.open($('term'));

  // Scrolling must not pop up the on-screen keyboard. After a scroll button or
  // a scroll drag, ignore the focus the terminal would otherwise receive (a tap
  // that is NOT a scroll still focuses it, so typing works as before).
  var noFocusUntil = 0;
  function quietScroll() { noFocusUntil = Date.now() + 700; }
  if (term.textarea) {
    term.textarea.addEventListener('focus', function () {
      if (Date.now() < noFocusUntil) term.textarea.blur();
    });
  }

  // Names and titles come from disk (conversation titles), so everything that
  // shows them is built with textContent / createElement, never innerHTML.
  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  }

  // A script error must never leave the UI silently dead: show it.
  window.addEventListener('error', function (ev) {
    try { banner('Ошибка в приложении: ' + (ev.message || 'неизвестная'), 'err'); } catch (e) { /* ignore */ }
  });
  window.addEventListener('unhandledrejection', function (ev) {
    try { banner('Ошибка: ' + ((ev.reason && ev.reason.message) || 'сбой запроса'), 'err'); } catch (e) { /* ignore */ }
  });

  // ---- PIN lock state -------------------------------------------------------
  // The unlock key lives only in memory: reloading or reopening the page
  // loses it, so the PIN is asked for again on every opening.
  var unlockKey = null;
  var lockEnabled = false;
  var idleSeconds = 0;
  var lockShown = false;
  var booted = false;

  // ---- API ------------------------------------------------------------------
  // X-CR is the CSRF marker the server requires on state-changing calls; a
  // cross-site page cannot send a custom header. referrerPolicy keeps the real
  // Origin on the request (the site-wide no-referrer would turn it into "null").
  // passive = background polling: tells the server not to count it as activity
  // (otherwise polling would keep the PIN unlocked forever).
  function api(method, path, body, raw, passive) {
    var headers = { 'X-CR': '1' };
    if (unlockKey) headers['X-Unlock'] = unlockKey;
    if (passive) headers['X-Passive'] = '1';
    var init = { method: method, headers: headers, credentials: 'same-origin', referrerPolicy: 'same-origin', cache: 'no-store' };
    if (raw) {
      headers['Content-Type'] = raw.type || 'application/octet-stream';
      init.body = raw;
    } else if (body !== undefined) {
      headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(body);
    }
    return fetch(path, init).then(function (r) {
      if (r.status === 423) { onLocked(); return { ok: false, status: 423, data: null }; }
      if (r.status === 401) { location.href = '/login'; return { ok: false, status: 401, data: null }; }
      return r.json().catch(function () { return null; }).then(function (data) {
        return { ok: r.ok, status: r.status, data: data };
      });
    });
  }

  // ---- messages --------------------------------------------------------------
  var ERR = {
    command_not_found: 'Программа не найдена на ноутбуке (claude или codex). Установите её или проверьте параметры --cmd и --codex-cmd сервера.',
    workdir_missing: 'Папка проекта недоступна: возможно, её удалили или переименовали.',
    start_failed: 'Не удалось запустить процесс. Подробности в консоли сервера на ноутбуке.',
    too_many_sessions: 'Слишком много открытых сессий. Закройте ненужные.',
    no_such_project: 'Такого проекта нет в списке сервера.',
    bad_resume_id: 'Некорректный идентификатор сессии.',
    too_large: 'Файл больше 10 МБ.',
    unsupported_type: 'Этот тип файла не поддерживается (разрешены картинки PNG/JPEG/GIF/WebP, PDF и текст).',
    empty: 'Файл пустой.',
    no_such_session: 'Эта сессия уже закрыта.',
    csrf: 'Запрос отклонён сервером. Обновите страницу.',
    bad_origin: 'Запрос отклонён сервером. Обновите страницу.',
    agents_unavailable: 'Не удалось получить список сессий с ноутбука (claude agents).',
    no_such_laptop_session: 'Эта сессия на ноутбуке уже закрыта.',
    unsafe_session_name: 'У этой фоновой сессии имя с неподходящими символами: переименуйте её на ноутбуке.',
    confirm_required: 'Нужно подтверждение.',
    confirm_busy_required: 'Нужно подтверждение: сессия сейчас работает.',
    cannot_continue_here: 'Эту сессию нельзя продолжить здесь (папка недоступна или нет команды claude). Окно на ноутбуке НЕ закрыто.',
    closed_not_resumed: 'Окно на ноутбуке уже закрыто, но продолжить не удалось. Разговор можно открыть через «Новая сессия» → «Продолжить прежнюю».',
    close_failed: 'Не удалось закрыть окно на ноутбуке. Подробности в консоли сервера.',
    open_on_laptop: 'Этот разговор сейчас открыт в окне на ноутбуке. Найдите его в разделе «Открыты на ноутбуке»: там его можно закрыть и продолжить здесь.',
    no_such_conversation: 'Такого разговора в истории нет.',
    machine_unavailable: 'Состояние ноутбука недоступно на этом сервере.',
    power_disabled: 'Сон и выключение отключены на сервере (--no-power).',
    bad_action: 'Неизвестное действие.',
    bad_code: 'Подтверждение устарело. Повторите действие.',
    power_failed: 'Ноутбук не выполнил команду. Подробности в консоли сервера.',
    nothing_to_cancel: 'Нечего отменять: выключение не запланировано.',
    too_many_attempts: 'Слишком много попыток. Подождите и повторите.',
    tool_unavailable: 'Этот инструмент не найден на ноутбуке. Установите его или проверьте параметры сервера (--codex-cmd, --cmd).',
    internal: 'Ошибка на сервере. Подробности в его консоли.'
  };
  function errText(code) { return ERR[code] || 'Ошибка: ' + code; }

  var bannerTimer = null;
  // kind 'err' stays until replaced; anything else fades.
  function banner(text, kind, withRestart) {
    var b = $('banner');
    clearTimeout(bannerTimer);
    b.textContent = '';
    b.className = kind === 'err' ? 'err' : '';
    b.appendChild(el('span', null, text));
    if (withRestart) {
      var btn = el('button', null, 'Перезапустить');
      btn.addEventListener('click', function () { sendCtrl({ type: 'restart' }); });
      b.appendChild(btn);
    }
    b.hidden = false;
    if (kind !== 'err') bannerTimer = setTimeout(hideBanner, 4000);
    scheduleFit();
  }
  function hideBanner() { $('banner').hidden = true; scheduleFit(); }

  // The dot shows the connection until it is up, then the session's pulse:
  // working (blue, pulsing), waiting for you (amber, fast), idle (green).
  var connState = 'off';
  var pulse = { state: 'idle', since: 0 };

  function setStatus(kind) { connState = kind; renderDot(); renderPulseText(); }
  function setPulse(state, since) {
    pulse = { state: state || 'idle', since: since || 0 };
    renderDot();
    renderPulseText();
  }
  function renderDot() {
    var k = connState;
    if (k === 'on') {
      k = pulse.state === 'working' ? 'work' : pulse.state === 'waiting' ? 'ask' : pulse.state === 'exited' ? 'off' : 'on';
    }
    $('dot').className = 'dot ' + k;
  }
  function fmtElapsed(ms) {
    var s = Math.max(0, Math.floor(ms / 1000));
    if (s < 60) return s + ' с';
    var m = Math.floor(s / 60);
    if (m < 60) return m + ':' + ('0' + (s % 60)).slice(-2);
    return Math.floor(m / 60) + ' ч ' + (m % 60) + ' м';
  }
  function renderPulseText() {
    var t = '';
    if (connState === 'on' && current) {
      var el = pulse.since ? Date.now() - pulse.since : 0;
      if (pulse.state === 'working') t = 'работает' + (pulse.since ? ' ' + fmtElapsed(el) : '');
      else if (pulse.state === 'waiting') t = 'ждёт вас' + (pulse.since ? ' ' + fmtElapsed(el) : '');
      else if (pulse.state === 'idle') t = pulse.since ? 'готово, ' + fmtElapsed(el) + ' назад' : 'ждёт ввода';
      else if (pulse.state === 'exited') t = 'завершена';
    }
    var p = $('pulse-text');
    if (t && curTool === 'codex') t = 'Codex · ' + t; // Claude is the default, so only Codex is labelled
    p.textContent = t;
    p.className = 'pulse ' + pulse.state;
  }
  setInterval(renderPulseText, 1000);

  function pulseLabel(s) {
    switch (s.pulse) {
      case 'working': return 'работает';
      case 'waiting': return 'ждёт вашего ответа';
      case 'exited': return 'завершена';
      default: return 'готова';
    }
  }

  // Output-based pulse (sessions without hooks) changes silently, so refresh
  // from the list now and then while the app is in use.
  setInterval(function () {
    if (document.hidden || lockShown || !current || !ws || ws.readyState !== 1) return;
    api('GET', '/api/sessions', undefined, undefined, true).then(function (r) {
      if (!r.ok || !Array.isArray(r.data)) return;
      sessions = r.data;
      var s = sessionById(current);
      if (s && s.pulse !== pulse.state) setPulse(s.pulse, s.pulse_since);
    });
  }, 5000);

  // ---- sessions ---------------------------------------------------------------
  var sessions = [];
  var current = null; // session id
  var ws = null;
  var retry = 0;
  var retryTimer = null;

  function loadSessions() {
    return api('GET', '/api/sessions').then(function (r) {
      if (r.ok && Array.isArray(r.data)) sessions = r.data;
      return sessions;
    });
  }

  function sessionById(id) {
    for (var i = 0; i < sessions.length; i++) if (sessions[i].id === id) return sessions[i];
    return null;
  }

  var TOOL_NAMES = { claude: 'Claude Code', codex: 'Codex' };
  function toolName(t) { return TOOL_NAMES[t] || TOOL_NAMES.claude; }
  var curTool = 'claude';

  function updateTitle() {
    var s = sessionById(current);
    curTool = s && s.tool ? s.tool : 'claude';
    $('sess-title').textContent = s ? s.project + ' · ' + s.title : 'Сессии';
    renderPulseText();
  }

  function openSession(id) {
    if (!sessionById(id)) return;
    closeSheet();
    current = id;
    retry = 0;
    try { history.replaceState(null, '', '#s=' + id); } catch (e) { /* ignore */ }
    hideBanner();
    updateTitle();
    connect();
    term.focus();
  }

  function dropCurrent() {
    current = null;
    if (ws) { ws.onclose = null; ws.close(); ws = null; }
    clearTimeout(retryTimer);
    term.reset();
    setStatus('off');
    updateTitle();
    try { history.replaceState(null, '', location.pathname); } catch (e) { /* ignore */ }
  }

  // ---- WebSocket ----------------------------------------------------------------
  function send(bytes) { if (ws && ws.readyState === WebSocket.OPEN) ws.send(bytes); }
  function sendText(str) { send(enc.encode(str)); }
  function sendCtrl(obj) { if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(obj)); }
  function sendSize() {
    if (term.cols > 0 && term.rows > 0) sendCtrl({ type: 'resize', cols: term.cols, rows: term.rows });
  }

  function connect() {
    clearTimeout(retryTimer);
    if (ws) { ws.onclose = null; ws.close(); ws = null; }
    if (!current) return;
    if (lockEnabled && !unlockKey) { showLock(); return; }
    setStatus('wait');
    var proto = location.protocol === 'https:' ? 'wss://' : 'ws://';
    // Browsers cannot set headers on a WebSocket; the unlock key travels as a
    // subprotocol (never in the URL, which ends up in logs and history).
    var protocols = lockEnabled ? ['cr-unlock', unlockKey] : undefined;
    var sock = new WebSocket(proto + location.host + '/ws?session=' + encodeURIComponent(current), protocols);
    sock.binaryType = 'arraybuffer';
    ws = sock;

    sock.onopen = function () {
      retry = 0;
      term.reset(); // the server replays its buffer right away
      fitNow();
      sendSize();
      setStatus('on');
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
      setStatus('off');
      scheduleReconnect();
    };
  }

  function exitBanner(code, err) {
    setStatus('off');
    if (err) banner(errText(err), 'err', true);
    else banner('Процесс завершился (код ' + code + ').', 'err', true);
  }

  function handleControl(m) {
    switch (m.type) {
      case 'status':
        setPulse(m.pulse, m.since);
        if (m.state === 'exited') exitBanner(m.code, m.err);
        else hideBanner();
        break;
      case 'pulse': setPulse(m.state, m.since); break;
      case 'reset': term.reset(); setStatus('on'); setPulse('idle', 0); hideBanner(); break;
      case 'exit': exitBanner(m.code, m.err); setPulse('exited', 0); break;
    }
  }

  function scheduleReconnect() {
    var delay = Math.min(30000, 1000 * Math.pow(2, retry++));
    // A refused handshake (closed session, expired login) looks like any other
    // failure to the browser, so ask the API what is going on.
    loadSessions().then(function () {
      if (current && !sessionById(current)) {
        dropCurrent();
        banner('Сессия закрыта.', 'err');
        showSessions();
      }
    }).catch(function () { /* offline; keep retrying */ });
    banner('Нет соединения. Повтор через ' + Math.round(delay / 1000) + ' с.', 'err');
    retryTimer = setTimeout(connect, delay);
  }

  // ---- bottom sheet ---------------------------------------------------------------
  function openSheet(title) {
    $('sheet-title').textContent = title;
    $('sheet-body').textContent = '';
    $('sheet').hidden = false;
    return $('sheet-body');
  }
  function closeSheet() { $('sheet').hidden = true; stopMachineRefresh(); }
  $('sheet-close').addEventListener('click', closeSheet);
  $('sheet').addEventListener('click', function (ev) { if (ev.target === $('sheet')) closeSheet(); });

  function row(title, sub, onTap) {
    var r = el('div', 'row');
    var main = el('button', 'row-main');
    main.appendChild(el('span', 'row-title', title));
    if (sub) main.appendChild(el('span', 'row-sub', sub));
    main.addEventListener('click', onTap);
    r.appendChild(main);
    return r;
  }

  function fmtDate(iso) {
    var d = new Date(iso);
    if (isNaN(d)) return '';
    return d.toLocaleString('ru-RU', { day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit' });
  }

  function showSessions() {
    var body = openSheet('Сессии');
    loadSessions().then(function () {
      body.textContent = '';
      if (sessions.length > 0) body.appendChild(el('p', 'section', 'Запущены здесь'));
      sessions.forEach(function (s) {
        var r = row(s.title, s.project + ' · ' + toolName(s.tool) + ' · ' + pulseLabel(s), function () { openSession(s.id); });
        r.querySelector('.row-title').insertBefore(el('span', 'dot-sm ' + s.pulse), r.querySelector('.row-title').firstChild);
        if (s.id === current) r.classList.add('current');
        var x = el('button', 'row-x', '✕');
        x.setAttribute('aria-label', 'Закрыть сессию');
        x.addEventListener('click', function () { closeSession(s.id); });
        r.appendChild(x);
        body.appendChild(r);
      });
      var add = el('button', 'primary', '＋ Новая сессия');
      add.addEventListener('click', newSession);
      body.appendChild(add);

      // Claude sessions open anywhere on the laptop.
      var head = el('p', 'section', 'Claude Code: открыты на ноутбуке');
      var box = el('div', 'laptop');
      box.appendChild(el('p', 'hint', 'Загрузка…'));
      body.appendChild(head);
      body.appendChild(box);
      var past = el('button', null, 'Прошлые разговоры из всех папок…');
      past.addEventListener('click', function () { showAllHistory(); });
      body.appendChild(past);
      api('GET', '/api/laptop').then(function (r) {
        box.textContent = '';
        var list = (r.ok && Array.isArray(r.data)) ? r.data : [];
        if (!r.ok) { box.appendChild(el('p', 'hint', 'Не удалось получить список с ноутбука.')); return; }
        if (list.length === 0) { box.appendChild(el('p', 'hint', 'Других сессий нет.')); return; }
        list.forEach(function (it) {
          var kind = it.kind === 'background' ? 'фоновая' : 'в окне';
          var state = it.status === 'busy' ? 'работает' : 'ждёт';
          // The conversation's title is what people recognise; the short handle is secondary.
          var title = it.title || it.name || it.folder;
          var sub = (it.name && it.name !== title ? it.name + ' · ' : '') + it.folder + ' · ' + kind + ' · ' + state;
          box.appendChild(row(title, sub, function () { takeLaptop(it); }));
        });
      });
    });
  }

  // Closed conversations from every folder on the laptop, not only open ones,
  // for the chosen tool.
  function showAllHistory(tool) {
    tool = (tool === 'codex' || tool === 'claude') ? tool : pref('tool', 'claude');
    var body = openSheet('Прошлые разговоры');
    var seg = el('div', 'seg');
    ['claude', 'codex'].forEach(function (t) {
      var b = el('button', t === tool ? 'on' : '', toolName(t));
      b.addEventListener('click', function () { setPref('tool', t); showAllHistory(t); });
      seg.appendChild(b);
    });
    body.appendChild(seg);
    var sb = el('div', 'search-box');
    var inp = el('input');
    inp.type = 'search';
    inp.placeholder = 'Поиск по названию или папке';
    inp.autocomplete = 'off';
    sb.appendChild(inp);
    body.appendChild(sb);
    var list = el('div', 'hist-list');
    list.appendChild(el('p', 'hint', 'Загрузка…'));
    body.appendChild(list);
    var items = [];
    function render(q) {
      list.textContent = '';
      q = q.toLowerCase();
      var shown = 0;
      items.forEach(function (it) {
        if (q && (it.title + ' ' + it.folder).toLowerCase().indexOf(q) < 0) return;
        shown++;
        list.appendChild(row(it.title, it.folder + ' · ' + fmtDate(it.modified), function () { openPast(it, tool); }));
      });
      if (!shown) list.appendChild(el('p', 'hint', items.length ? 'Ничего не найдено.' : 'История пуста.'));
    }
    api('GET', '/api/history/all?tool=' + encodeURIComponent(tool)).then(function (r) {
      items = (r.ok && Array.isArray(r.data)) ? r.data : [];
      render(inp.value);
    });
    inp.addEventListener('input', function () { render(inp.value); });
  }

  function openPast(it, tool) {
    banner('Открываю…');
    api('POST', '/api/history/open', { id: it.id, tool: tool }).then(function (r) {
      if (!r.ok) { closeSheet(); banner(errText(r.data && r.data.error), 'err'); return; }
      return loadSessions().then(function () { openSession(r.data.id); });
    });
  }

  // Open a laptop session here. An interactive one (a terminal window) is
  // closed first so two copies never write to the same conversation.
  function takeLaptop(it) {
    var body = { id: it.id };
    if (it.kind !== 'background') {
      var msg = 'Сессия «' + (it.title || it.name || it.folder) + '» открыта в окне на ноутбуке.\n\n' +
        'Окно будет ЗАКРЫТО, а разговор продолжится здесь. Несохранённое в окне может потеряться, а запущенные им команды и серверы могут остаться работать.';
      if (it.status === 'busy') msg += '\n\n⚠ Сейчас она РАБОТАЕТ: текущая задача будет прервана.';
      if (!confirm(msg + '\n\nПродолжить?')) return;
      body.confirm = true;
      body.confirm_busy = it.status === 'busy';
    }
    banner('Открываю…');
    api('POST', '/api/laptop/take', body).then(function (r) {
      if (!r.ok) { closeSheet(); banner(errText(r.data && r.data.error), 'err'); return; }
      return loadSessions().then(function () { openSession(r.data.id); });
    });
  }

  function closeSession(id) {
    if (!confirm('Закрыть сессию? Процесс claude в ней будет остановлен.')) return;
    api('DELETE', '/api/sessions/' + encodeURIComponent(id)).then(function () {
      if (id === current) dropCurrent();
      return loadSessions();
    }).then(function () {
      if (!current && sessions.length > 0) openSession(sessions[0].id);
      else showSessions();
    });
  }

  // ---- new session: tool -> project -> conversation ------------------------------------------
  // The first question is which tool to use. When only one is installed the
  // question is skipped.
  var TOOL_SUB = {
    claude: 'Anthropic · пульс сессии, уведомления о разрешениях и завершении',
    codex: 'OpenAI · уведомление о завершении хода'
  };

  function newSession() {
    api('GET', '/api/tools').then(function (r) {
      var list = (r.ok && Array.isArray(r.data)) ? r.data : [];
      var ok = list.filter(function (t) { return t.available; });
      if (ok.length === 0) { banner('На ноутбуке не найден ни Claude Code, ни Codex.', 'err'); return; }
      if (ok.length === 1) { pickProject(ok[0].id); return; }
      var body = openSheet('Чем пользоваться?');
      var last = pref('tool', 'claude');
      list.forEach(function (t) {
        var card = row(t.name, t.available ? (TOOL_SUB[t.id] || '') : 'не найден на ноутбуке', function () {
          if (!t.available) { banner(errText('tool_unavailable'), 'err'); return; }
          setPref('tool', t.id);
          pickProject(t.id);
        });
        card.classList.add('tool-card');
        if (!t.available) card.classList.add('disabled');
        else if (t.id === last) card.classList.add('current');
        body.appendChild(card);
      });
    });
  }

  function pickProject(tool) {
    api('GET', '/api/projects').then(function (r) {
      var list = (r.ok && Array.isArray(r.data)) ? r.data : [];
      if (list.length === 0) { banner('На сервере не задано ни одного проекта.', 'err'); return; }
      if (list.length === 1) { pickConversation(list[0].name, tool); return; }
      var body = openSheet(toolName(tool) + ': проект');
      list.forEach(function (p) {
        body.appendChild(row(p.name, '', function () { pickConversation(p.name, tool); }));
      });
    });
  }

  // Like `claude --resume` / `codex resume`: start fresh or continue one of the project's conversations.
  function pickConversation(project, tool) {
    var body = openSheet(toolName(tool) + ': ' + project);
    body.appendChild(el('p', 'hint', 'Загрузка…'));
    api('GET', '/api/history?project=' + encodeURIComponent(project) + '&tool=' + encodeURIComponent(tool)).then(function (r) {
      body.textContent = '';
      var fresh = el('button', 'primary', '＋ Новая сессия');
      fresh.addEventListener('click', function () { createSession(project, '', tool); });
      body.appendChild(fresh);
      var items = (r.ok && Array.isArray(r.data)) ? r.data : [];
      if (items.length > 0) {
        body.appendChild(el('p', 'section', 'Продолжить прежнюю'));
        if (tool === 'codex') {
          body.appendChild(el('p', 'hint', 'Если этот разговор открыт в окне Codex на ноутбуке, закройте окно: иначе два экземпляра будут писать в один разговор.'));
        }
      }
      items.forEach(function (it) {
        body.appendChild(row(it.title, fmtDate(it.modified), function () { createSession(project, it.id, tool); }));
      });
    });
  }

  function createSession(project, resume, tool) {
    api('POST', '/api/sessions', { project: project, resume: resume, tool: tool || 'claude' }).then(function (r) {
      if (!r.ok) { closeSheet(); banner(errText(r.data && r.data.error), 'err'); return; }
      return loadSessions().then(function () { openSession(r.data.id); });
    });
  }

  $('btn-sessions').addEventListener('click', showSessions);

  // ---- menu -------------------------------------------------------------------------
  function showMenu() {
    var body = openSheet('Меню');
    body.appendChild(row('Перезапустить процесс', 'завершит текущий и запустит заново', function () {
      closeSheet();
      if (!current) return;
      if (confirm('Перезапустить процесс? Текущая работа в нём будет прервана.')) sendCtrl({ type: 'restart' });
    }));
    body.appendChild(row('Прикрепить файл или фото', 'до 10 МБ: картинки, PDF, текст', function () {
      closeSheet();
      if (!current) { banner('Сначала откройте сессию.', 'err'); return; }
      $('file-input').click();
    }));
    body.appendChild(row('Состояние ноутбука', 'батарея, температура, нагрузка; сон и выключение', function () { showMachine(); }));
    body.appendChild(row('Читать вывод как текст', 'поиск, копирование, крупный шрифт', function () { closeSheet(); openReader(); }));
    var fr = el('div', 'row');
    var fl = el('div', 'row-main static');
    fl.appendChild(el('span', 'row-title', 'Размер шрифта'));
    var fv = el('span', 'row-sub', fontSize + ' px');
    fl.appendChild(fv);
    fr.appendChild(fl);
    [['A−', -1], ['A+', 1]].forEach(function (b) {
      var btn = el('button', 'row-x', b[0]);
      btn.addEventListener('click', function () { changeFont(b[1]); fv.textContent = fontSize + ' px'; });
      fr.appendChild(btn);
    });
    body.appendChild(fr);
    if (lockEnabled) {
      body.appendChild(row('Заблокировать сейчас', 'снова потребуется PIN', function () { closeSheet(); lockNow(); }));
    }
    var pushRow = row('Уведомления', 'проверяю…', togglePush);
    body.appendChild(pushRow);
    pushState().then(function (s) {
      pushRow.querySelector('.row-sub').textContent = s;
    });
    if (current) {
      body.appendChild(row('Закрыть эту сессию', 'остановит процесс', function () { closeSession(current); }));
    }
    body.appendChild(row('Выйти', 'на этом устройстве', function () { logout(false); }));
    body.appendChild(row('Выйти везде', 'отзовёт вход на всех устройствах', function () {
      if (confirm('Выйти на всех устройствах?')) logout(true);
    }));
  }

  function logout(all) {
    api('POST', '/api/logout' + (all ? '?all=1' : '')).then(function () { location.href = '/login'; });
  }
  $('btn-menu').addEventListener('click', showMenu);

  // ---- file upload --------------------------------------------------------------------
  $('file-input').addEventListener('change', function (ev) {
    var f = ev.target.files && ev.target.files[0];
    ev.target.value = '';
    if (!f || !current) return;
    if (f.size > 10 * 1024 * 1024) { banner(errText('too_large'), 'err'); return; }
    banner('Загрузка…');
    api('POST', '/api/upload?session=' + encodeURIComponent(current), undefined, f).then(function (r) {
      if (!r.ok) { banner(errText(r.data && r.data.error), 'err'); return; }
      // The path is typed into the terminal; Claude reads the file from there.
      sendText(r.data.path + ' ');
      banner('Файл загружен: ' + r.data.path);
      term.focus();
    }).catch(function () { banner('Не удалось загрузить файл: нет соединения.', 'err'); });
  });

  // ---- push notifications -------------------------------------------------------------
  function pushSupported() {
    return window.isSecureContext && 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window;
  }

  function swReg() {
    return navigator.serviceWorker.register('/sw.js').then(function () { return navigator.serviceWorker.ready; });
  }

  function pushState() {
    if (!pushSupported()) {
      return Promise.resolve(window.isSecureContext ? 'не поддерживается этим браузером' : 'нужен HTTPS (см. README)');
    }
    if (Notification.permission === 'denied') return Promise.resolve('запрещены в настройках браузера');
    return swReg().then(function (reg) { return reg.pushManager.getSubscription(); })
      .then(function (sub) { return sub ? 'включены (нажмите, чтобы выключить)' : 'выключены (нажмите, чтобы включить)'; })
      .catch(function () { return 'недоступны'; });
  }

  function b64ToBytes(s) {
    var pad = '='.repeat((4 - s.length % 4) % 4);
    var raw = atob((s + pad).replace(/-/g, '+').replace(/_/g, '/'));
    var out = new Uint8Array(raw.length);
    for (var i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
    return out;
  }

  function togglePush() {
    closeSheet();
    if (!pushSupported()) {
      banner(window.isSecureContext ? 'Этот браузер не поддерживает push-уведомления.' :
        'Уведомления работают только по HTTPS. Настройте `tailscale cert` (см. README).', 'err');
      return;
    }
    swReg().then(function (reg) {
      return reg.pushManager.getSubscription().then(function (sub) {
        if (sub) {
          return api('POST', '/api/push/unsubscribe', { endpoint: sub.endpoint }).then(function () {
            return sub.unsubscribe();
          }).then(function () { banner('Уведомления выключены.'); });
        }
        return Notification.requestPermission().then(function (perm) {
          if (perm !== 'granted') { banner('Разрешение на уведомления не выдано.', 'err'); return; }
          return api('GET', '/api/push/key').then(function (k) {
            if (!k.ok) throw new Error('key');
            return reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: b64ToBytes(k.data.key) });
          }).then(function (s) {
            return api('POST', '/api/push/subscribe', s.toJSON());
          }).then(function (r) {
            banner(r.ok ? 'Уведомления включены.' : errText(r.data && r.data.error), r.ok ? '' : 'err');
          });
        });
      });
    }).catch(function () { banner('Не удалось настроить уведомления.', 'err'); });
  }

  if ('serviceWorker' in navigator && window.isSecureContext) {
    navigator.serviceWorker.register('/sw.js').catch(function () { /* optional feature */ });
    navigator.serviceWorker.addEventListener('message', function (ev) {
      if (ev.data && ev.data.type === 'open-session') {
        loadSessions().then(function () { if (sessionById(ev.data.session)) openSession(ev.data.session); });
      }
    });
  }

  // ---- input ----------------------------------------------------------------------------
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
    var k = b.getAttribute('data-seq');
    // Voice and reader act on click: speech recognition needs a real user
    // activation, and cancelling pointerdown can suppress the click on touch.
    if (k === 'voice' || k === 'reader') return;
    ev.preventDefault();
    if (k === 'ctrl') { setCtrl(!ctrlOn); return; }
    // Local scrolling; keys are sent to the process only for full-screen apps.
    if (k === 'scroll-up') { quietScroll(); scrollPage(-1); return; }
    if (k === 'scroll-down') { quietScroll(); scrollPage(1); return; }
    if (k === 'scroll-end') { quietScroll(); if (!inAltScreen()) term.scrollToBottom(); return; }
    sendText(SEQ[k] !== undefined ? SEQ[k] : applyCtrl(k));
    term.focus();
  });

  // ---- scrolling --------------------------------------------------------------------------
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
      var seq = '\x1b[<' + (up ? 64 : 65) + ';' + Math.ceil(term.cols / 2) + ';' + Math.ceil(term.rows / 2) + 'M';
      for (var i = 0; i < count; i++) sendText(seq);
    } else {
      for (var j = 0; j < count; j++) sendText(up ? '\x1b[A' : '\x1b[B');
    }
  }

  function scrollPage(dir) { // dir: -1 older, 1 newer
    if (!inAltScreen()) { term.scrollPages(dir); return; }
    if (mouseTracking()) { scrollLines(dir * 5); return; }
    sendText(dir < 0 ? '\x1b[5~' : '\x1b[6~');
  }

  // Dragging a finger over the terminal scrolls. Handled in the capture phase
  // so xterm's own touch handling does not also act on it. A tap (little
  // movement) is left alone so it still focuses the terminal.
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
    quietScroll();
    ev.preventDefault();
    ev.stopPropagation();
    var lines = Math.trunc(dy / lineH);
    if (lines !== 0) {
      scrollLines(-Math.max(-20, Math.min(20, lines))); // finger down = older output
      touchY += lines * lineH;
    }
  }, { capture: true, passive: false });
  termEl.addEventListener('touchend', function () {
    if (touchMoved) quietScroll(); // the tap-synthesised click after a drag must not focus the terminal
    touchY = null;
  }, { capture: true, passive: true });

  // ---- layout -------------------------------------------------------------------------------
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

  // ---- screenshot overlay -----------------------------------------------------------------------
  var shotTimer = null;

  var shotUrl = null;
  // Refresh the laptop state first: a screen that is off gives a black frame,
  // so say so instead of showing nothing.
  function refreshShot(passive) {
    $('shot-msg').textContent = 'загрузка…';
    refreshMachine().then(function () {
      if ($('shot').hidden) return;
      var s = screenInfo(machine);
      if (s.key === 'off' || s.key === 'lid') {
        $('shot-img').removeAttribute('src');
        $('shot-msg').textContent = s.text + ': на скриншоте был бы чёрный кадр.';
        return;
      }
      doShot(passive);
    });
  }

  function doShot(passive) {
    var img = $('shot-img');
    $('shot-msg').textContent = 'загрузка…';
    var sel = $('shot-display');
    var d = sel.hidden ? 0 : parseInt(sel.value, 10) || 0;
    // Fetched with the unlock key header (an <img src> cannot send one) and shown from a blob.
    var headers = {};
    if (unlockKey) headers['X-Unlock'] = unlockKey;
    if (passive === true) headers['X-Passive'] = '1'; // auto-refresh must not keep the PIN unlocked
    fetch('/screenshot?display=' + d, { headers: headers, credentials: 'same-origin', cache: 'no-store' }).then(function (r) {
      if (r.status === 423) { onLocked(); throw new Error('locked'); }
      if (r.status === 401) { location.href = '/login'; throw new Error('auth'); }
      if (!r.ok) throw new Error('http ' + r.status);
      return r.blob();
    }).then(function (blob) {
      if ($('shot').hidden) return;
      if (shotUrl) URL.revokeObjectURL(shotUrl);
      shotUrl = URL.createObjectURL(blob);
      img.src = shotUrl;
      $('shot-msg').textContent = '';
    }).catch(function () {
      $('shot-msg').textContent = 'не удалось получить скриншот. Возможно, экран ноутбука заблокирован.';
    });
  }

  function loadDisplays() {
    var sel = $('shot-display');
    api('GET', '/api/displays').then(function (r) {
      var list = (r.ok && Array.isArray(r.data)) ? r.data : [];
      var prev = sel.value;
      sel.textContent = '';
      list.forEach(function (d) {
        var o = el('option', null, 'Экран ' + (d.index + 1) + ' (' + d.width + '×' + d.height + ')');
        o.value = String(d.index);
        sel.appendChild(o);
      });
      if (prev) sel.value = prev;
      sel.hidden = list.length < 2;
    });
  }

  function setAuto(on) {
    clearInterval(shotTimer);
    shotTimer = on ? setInterval(function () { refreshShot(true); }, 5000) : null;
  }

  function closeShot() {
    setAuto(false);
    $('shot-auto').checked = false;
    $('shot').hidden = true;
    $('shot-img').removeAttribute('src');
    if (shotUrl) { URL.revokeObjectURL(shotUrl); shotUrl = null; }
    term.focus();
  }

  $('btn-screen').addEventListener('click', function () {
    $('shot').hidden = false;
    loadDisplays();
    refreshShot();
  });
  $('shot-refresh').addEventListener('click', refreshShot);
  $('shot-display').addEventListener('change', refreshShot);
  $('shot-auto').addEventListener('change', function (ev) { setAuto(ev.target.checked); });
  $('shot-close').addEventListener('click', closeShot);
  $('shot-view').addEventListener('click', closeShot); // tap the picture to close

  // ---- PIN lock -----------------------------------------------------------------------------------
  var lastActivity = Date.now();
  var hiddenAt = 0;

  function showLock(msg) {
    lockShown = true;
    $('lock').hidden = false;
    $('lock-msg').textContent = msg || '';
    $('lock-pin').value = '';
    closeSheet(); closeShot(); closeReader(); closeCompose();
    term.reset(); // nothing readable stays behind the lock screen
    rdText = '';
    $('rd-text').textContent = '';
    $('compose-text').value = '';
    if (ws) { ws.onclose = null; ws.close(); ws = null; }
    clearTimeout(retryTimer);
    if (document.activeElement && document.activeElement.blur) document.activeElement.blur();
    setTimeout(function () { $('lock-pin').focus(); }, 50);
  }
  function hideLock() { lockShown = false; $('lock').hidden = true; }

  // A 423 from the server means the key expired (idle timeout) or was never given.
  function onLocked() { lockEnabled = true; unlockKey = null; if (!lockShown) showLock('Приложение заблокировано.'); }

  function lockNow() {
    api('POST', '/api/lock/lock');
    unlockKey = null;
    showLock();
  }

  $('lock-form').addEventListener('submit', function (ev) {
    ev.preventDefault();
    var pin = $('lock-pin').value;
    if (!pin) return;
    api('POST', '/api/lock/unlock', { pin: pin }).then(function (r) {
      $('lock-pin').value = '';
      if (r.ok && r.data && r.data.key) {
        unlockKey = r.data.key;
        idleSeconds = r.data.idle_seconds || 0;
        lastActivity = Date.now();
        hideLock();
        if (booted && current) { retry = 0; connect(); } else { startup(); }
        return;
      }
      if (r.status === 429) $('lock-msg').textContent = 'Слишком много попыток. Подождите и повторите.';
      else if (r.status === 403) $('lock-msg').textContent = 'Неверный PIN.';
      else if (r.status !== 401 && r.status !== 423) $('lock-msg').textContent = 'Не удалось проверить PIN.';
    }).catch(function () { $('lock-msg').textContent = 'Нет связи с сервером.'; });
  });

  ['pointerdown', 'keydown', 'touchstart'].forEach(function (t) {
    document.addEventListener(t, function () { lastActivity = Date.now(); }, true);
  });
  // Lock after a stretch without touching the app. The server enforces the same
  // timeout on its side; this just hides the screen promptly.
  setInterval(function () {
    if (lockEnabled && unlockKey && idleSeconds > 0 && Date.now() - lastActivity > idleSeconds * 1000) lockNow();
  }, 5000);

  // ---- font size (a local preference, not a secret) -------------------------------------------------
  function pref(k, d) {
    try { var v = localStorage.getItem('cr_' + k); return v === null ? d : JSON.parse(v); } catch (e) { return d; }
  }
  function setPref(k, v) { try { localStorage.setItem('cr_' + k, JSON.stringify(v)); } catch (e) { /* ignore */ } }
  var fontSize = Math.min(28, Math.max(10, parseInt(pref('fs', 14), 10) || 14));
  function applyFont() {
    term.options.fontSize = fontSize;
    $('rd-text').style.fontSize = fontSize + 'px';
    scheduleFit();
  }
  function changeFont(delta) {
    fontSize = Math.min(28, Math.max(10, fontSize + delta));
    setPref('fs', fontSize);
    applyFont();
  }

  // ---- text reader ------------------------------------------------------------------------------------
  // The terminal's own history as plain, selectable, wrapped text: easier to
  // read on a phone and to search or copy than the terminal grid.
  function termText() {
    var out = [];
    function dump(buf) {
      for (var i = 0; i < buf.length; i++) {
        var l = buf.getLine(i);
        if (!l) continue;
        var next = buf.getLine(i + 1);
        var t = l.translateToString(!(next && next.isWrapped));
        if (l.isWrapped && out.length) out[out.length - 1] += t; else out.push(t);
      }
    }
    dump(term.buffer.normal);
    if (term.buffer.active.type === 'alternate') {
      out.push('', '─── экран приложения ───');
      dump(term.buffer.alternate);
    }
    while (out.length && out[out.length - 1].trim() === '') out.pop();
    return out.join('\n');
  }

  var rdText = '';
  var rdMarks = [];
  var rdCur = -1;

  function openReader() {
    rdText = termText();
    $('reader').hidden = false;
    $('rd-find').value = '';
    renderReader('');
    var pre = $('rd-text');
    pre.scrollTop = pre.scrollHeight;
  }
  function closeReader() { $('reader').hidden = true; rdMarks = []; }

  function renderReader(q) {
    var pre = $('rd-text');
    pre.textContent = '';
    rdMarks = [];
    rdCur = -1;
    if (!q) { pre.textContent = rdText; $('rd-count').textContent = ''; return; }
    var hay = rdText.toLowerCase(), needle = q.toLowerCase();
    var pos = 0, i;
    while (rdMarks.length < 2000 && (i = hay.indexOf(needle, pos)) >= 0) {
      if (i > pos) pre.appendChild(document.createTextNode(rdText.slice(pos, i)));
      var m = el('mark', null, rdText.slice(i, i + needle.length));
      pre.appendChild(m);
      rdMarks.push(m);
      pos = i + needle.length;
    }
    pre.appendChild(document.createTextNode(rdText.slice(pos)));
    $('rd-count').textContent = rdMarks.length ? '0/' + rdMarks.length : 'нет';
    if (rdMarks.length) stepMatch(1);
  }

  function stepMatch(dir) {
    if (!rdMarks.length) return;
    if (rdCur >= 0) rdMarks[rdCur].classList.remove('cur');
    rdCur = (rdCur + dir + rdMarks.length) % rdMarks.length;
    var m = rdMarks[rdCur];
    m.classList.add('cur');
    m.scrollIntoView({ block: 'center' });
    $('rd-count').textContent = (rdCur + 1) + '/' + rdMarks.length;
  }

  function copyText(text) {
    var done = function () { banner('Скопировано.'); };
    var fail = function () {
      // Fallback for browsers without the async clipboard API.
      var ta = el('textarea');
      ta.value = text;
      document.body.appendChild(ta);
      ta.select();
      try { document.execCommand('copy'); done(); } catch (e) { banner('Не удалось скопировать.', 'err'); }
      document.body.removeChild(ta);
    };
    if (navigator.clipboard && window.isSecureContext) navigator.clipboard.writeText(text).then(done, fail); else fail();
  }

  $('rd-find').addEventListener('input', function (ev) { renderReader(ev.target.value); });
  $('rd-find').addEventListener('keydown', function (ev) { if (ev.key === 'Enter') { ev.preventDefault(); stepMatch(ev.shiftKey ? -1 : 1); } });
  $('rd-next').addEventListener('click', function () { stepMatch(1); });
  $('rd-prev').addEventListener('click', function () { stepMatch(-1); });
  $('rd-minus').addEventListener('click', function () { changeFont(-1); });
  $('rd-plus').addEventListener('click', function () { changeFont(1); });
  $('rd-close').addEventListener('click', function () { closeReader(); term.focus(); });
  $('rd-wrap').addEventListener('change', function (ev) { $('rd-text').classList.toggle('nowrap', !ev.target.checked); });
  $('rd-copy-all').addEventListener('click', function () { copyText(rdText); });
  $('rd-copy-tail').addEventListener('click', function () { copyText(rdText.split('\n').slice(-100).join('\n')); });

  // ---- laptop state and power ------------------------------------------------------------------
  var machine = null;
  var machineTimer = null;

  function stopMachineRefresh() { clearInterval(machineTimer); machineTimer = null; }

  function fmtMinutes(m) {
    if (m < 0) return '';
    var h = Math.floor(m / 60);
    return h > 0 ? h + ' ч ' + (m % 60) + ' м' : m + ' м';
  }
  function fmtUptime(sec) {
    var d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
    return d > 0 ? d + ' д ' + h + ' ч' : (h > 0 ? h + ' ч ' + m + ' м' : m + ' м');
  }

  // What the laptop's screen is doing, in words. A screen that is off or a
  // closed lid shows nothing at all; a locked session shows the lock screen.
  function screenInfo(m) {
    if (!m) return { key: 'on', text: '', level: '' };
    if (m.lid === 'closed') return { key: 'lid', text: 'Крышка закрыта', level: 'low' };
    if (m.display === 'off') return { key: 'off', text: 'Экран выключен', level: 'low' };
    if (m.locked) return { key: 'locked', text: 'Заблокирован', level: 'low' };
    if (m.display === 'dim') return { key: 'dim', text: 'Экран притушен', level: '' };
    return { key: 'on', text: '', level: '' };
  }

  function fmtClock(iso) {
    var d = new Date(iso);
    return isNaN(d) ? '' : d.toLocaleTimeString('ru-RU', { hour: '2-digit', minute: '2-digit' });
  }

  // A warning pill in the header whenever the screen is not simply on, so it is
  // visible without opening anything.
  function renderScreenPill() {
    var info = screenInfo(machine);
    var abnormal = info.key === 'lid' || info.key === 'off' || info.key === 'locked';
    var sp = $('scr-pill');
    sp.hidden = !abnormal;
    if (abnormal) $('scr-text').textContent = info.text;
    $('btn-screen').classList.toggle('off', abnormal);
  }

  // Battery pill in the header; hidden on machines without a battery.
  function renderPill() {
    renderScreenPill();
    var pill = $('btn-machine');
    var b = machine && machine.battery;
    if (!b || b.percent < 0) { pill.hidden = true; return; }
    pill.hidden = false;
    $('bat-text').textContent = b.percent + '%';
    $('bat-ico').querySelector('use').setAttribute('href', b.on_ac ? '#i-bolt' : '#i-battery');
    var cls = 'pill';
    if (!b.on_ac && b.percent <= 10) cls += ' crit';
    else if (!b.on_ac && b.percent <= 20) cls += ' low';
    else if (b.on_ac) cls += ' ok';
    pill.className = cls;
  }

  function refreshMachine() {
    return api('GET', '/api/machine', undefined, undefined, true).then(function (r) {
      if (r.ok && r.data) { machine = r.data; renderPill(); }
      return machine;
    }).catch(function () { return machine; });
  }
  setInterval(function () { if (!document.hidden && !lockShown && booted) refreshMachine(); }, 10000);
  $('btn-machine').addEventListener('click', function () { showMachine(); });
  $('scr-pill').addEventListener('click', function () { showMachine(); });

  function stat(label, value, sub, level, pct) {
    var c = el('div', 'stat' + (level ? ' ' + level : ''));
    c.appendChild(el('div', 'k', label));
    c.appendChild(el('div', 'v', value));
    if (sub) c.appendChild(el('div', 's', sub));
    if (pct !== undefined) {
      var bar = el('div', 'bar');
      var fill = el('i');
      fill.style.width = Math.max(0, Math.min(100, pct)) + '%';
      bar.appendChild(fill);
      c.appendChild(bar);
    }
    return c;
  }

  function actionButton(icon, text, onTap) {
    var b = el('button');
    var svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('class', 'ico sm');
    var use = document.createElementNS('http://www.w3.org/2000/svg', 'use');
    use.setAttribute('href', '#' + icon);
    svg.appendChild(use);
    b.appendChild(svg);
    b.appendChild(el('span', null, text));
    b.addEventListener('click', onTap);
    return b;
  }

  function showMachine() {
    var body = openSheet('Ноутбук');
    function render() {
      if (!machine) { body.textContent = ''; body.appendChild(el('p', 'hint', 'Загрузка…')); return; }
      var m = machine;
      body.textContent = '';

      // The laptop went to sleep and woke up while nobody was watching.
      if (m.resumed_at && Date.now() - new Date(m.resumed_at) < 30 * 60 * 1000) {
        var slept = el('div', 'notice warn');
        slept.appendChild(el('span', null, 'Ноутбук спал' + (m.slept_at ? ' с ' + fmtClock(m.slept_at) : '') + ' и проснулся в ' + fmtClock(m.resumed_at) + '. Пока он спал, сервер не работал.'));
        body.appendChild(slept);
      }

      if (m.shutdown_at) {
        var left = Math.max(0, Math.round((new Date(m.shutdown_at) - Date.now()) / 1000));
        var n = el('div', 'notice warn');
        n.appendChild(el('span', null, 'Выключение или перезагрузка запланированы через ' + left + ' с.'));
        var cancel = el('button', null, 'Отменить');
        cancel.addEventListener('click', function () {
          api('POST', '/api/power/cancel').then(function (r) {
            if (!r.ok) { banner(errText(r.data && r.data.error), 'err'); return; }
            banner('Выключение отменено.');
            refreshMachine().then(render);
          });
        });
        n.appendChild(cancel);
        body.appendChild(n);
      }

      var grid = el('div', 'stat-grid');
      var b = m.battery;
      if (b && b.percent >= 0) {
        var lvl = b.on_ac ? 'ok' : (b.percent <= 10 ? 'crit' : (b.percent <= 20 ? 'low' : ''));
        var sub = b.on_ac ? (b.charging ? 'заряжается, от сети' : 'от сети') : ('от батареи' + (b.minutes_left >= 0 ? ', осталось ' + fmtMinutes(b.minutes_left) : ''));
        if (b.saver) sub += ' · экономия';
        grid.appendChild(stat('Батарея', b.percent + '%', sub, lvl, b.percent));
      }
      if (m.thermal) {
        var t = m.thermal;
        var tl = t.throttled || t.celsius >= 85 ? 'crit' : (t.celsius >= 75 ? 'low' : 'ok');
        grid.appendChild(stat('Температура', Math.round(t.celsius) + ' °C', t.throttled ? 'процессор снижает частоту' : 'системная зона, норма ниже 75', tl));
      }
      grid.appendChild(stat('Процессор', m.cpu_percent + '%', '', m.cpu_percent >= 90 ? 'low' : '', m.cpu_percent));
      var mem = m.memory;
      if (mem && mem.total_mb > 0) {
        grid.appendChild(stat('Память', mem.percent + '%', (mem.used_mb / 1024).toFixed(1) + ' из ' + (mem.total_mb / 1024).toFixed(1) + ' ГБ', mem.percent >= 92 ? 'low' : '', mem.percent));
      }
      (m.disks || []).forEach(function (d) {
        if (d.total_gb < 20) return; // service partitions
        grid.appendChild(stat('Диск ' + d.name, d.free_gb.toFixed(0) + ' ГБ', 'свободно из ' + d.total_gb.toFixed(0) + ' ГБ', d.free_gb < 5 ? 'crit' : (d.free_gb < 15 ? 'low' : ''), d.percent));
      });
      grid.appendChild(stat('Работает', fmtUptime(m.uptime_sec), ''));
      var sc = screenInfo(m);
      var scrValue = { lid: 'крышка закрыта', off: 'выключен', locked: 'заблокирован', dim: 'притушен', on: 'включён' }[sc.key];
      var scrSub = sc.key === 'locked' ? 'скриншот покажет экран входа' : (sc.key === 'off' || sc.key === 'lid' ? 'на скриншоте будет чёрный кадр' : '');
      if (m.idle_sec >= 60) scrSub += (scrSub ? ' · ' : '') + 'нет ввода ' + fmtMinutes(Math.floor(m.idle_sec / 60));
      grid.appendChild(stat('Экран', scrValue, scrSub, sc.level));
      body.appendChild(grid);

      body.appendChild(row('Не давать ноутбуку засыпать', m.keep_awake ? 'включено: пока работает сервер' : 'выключено', function () {
        api('POST', '/api/machine/keepawake', { on: !m.keep_awake }).then(function (r) {
          if (!r.ok) { banner(errText(r.data && r.data.error), 'err'); return; }
          refreshMachine().then(render);
        });
      }));

      body.appendChild(el('p', 'section', 'Управление'));
      var acts = el('div', 'actions');
      acts.appendChild(actionButton('i-lock', 'Заблокировать экран', function () {
        if (!confirm('Заблокировать экран ноутбука? Скриншоты будут показывать экран входа.')) return;
        api('POST', '/api/machine/lock').then(function (r) {
          banner(r.ok ? 'Экран заблокирован.' : errText(r.data && r.data.error), r.ok ? '' : 'err');
          refreshMachine().then(render);
        });
      }));
      if (m.power_enabled) {
        acts.appendChild(actionButton('i-moon', 'Сон', function () { powerFlow('sleep'); }));
        acts.appendChild(actionButton('i-moon', 'Гибернация', function () { powerFlow('hibernate'); }));
        acts.appendChild(actionButton('i-refresh', 'Перезагрузить', function () { powerFlow('restart'); }));
        acts.appendChild(actionButton('i-power', 'Выключить', function () { powerFlow('shutdown'); }));
      }
      body.appendChild(acts);
      if (m.power_enabled) {
        body.appendChild(el('p', 'hint', 'После сна, гибернации и выключения связь с ноутбуком пропадёт, и с телефона его не разбудить: включать придётся руками.'));
      }
      body.appendChild(el('p', 'hint', 'Температура: самая горячая системная зона Windows, а не обязательно процессор.'));
    }
    render();
    refreshMachine().then(render);
    stopMachineRefresh();
    machineTimer = setInterval(function () { refreshMachine().then(function () { if (!$('sheet').hidden) render(); }); }, 5000);
  }

  var POWER_TEXT = {
    sleep: { title: 'Отправить ноутбук в сон?', warn: 'Связь прервётся. Разбудить ноутбук с телефона нельзя: нужно открыть крышку или нажать кнопку.', btn: 'Усыпить' },
    hibernate: { title: 'Гибернация?', warn: 'Состояние сохранится на диск, ноутбук выключится. Связь прервётся, включать придётся вручную.', btn: 'В гибернацию' },
    restart: { title: 'Перезагрузить ноутбук?', warn: 'Все сессии claude и codex будут завершены. Отсрочка 30 секунд, её можно отменить.', btn: 'Перезагрузить' },
    shutdown: { title: 'Выключить ноутбук?', warn: 'Все сессии claude и codex будут завершены, включать придётся вручную. Отсрочка 30 секунд, её можно отменить.', btn: 'Выключить' }
  };

  // Two steps: the server hands out a one-time code (30 s), which is then
  // presented together with the PIN when one is set.
  function powerFlow(action) {
    api('POST', '/api/power/prepare', { action: action }).then(function (r) {
      if (!r.ok) { banner(errText(r.data && r.data.error), 'err'); return; }
      var info = r.data;
      var text = POWER_TEXT[action];
      var body = openSheet(text.title);
      var box = el('div', 'confirm');
      box.appendChild(el('p', 'warn-text', text.warn));
      var pin = null;
      if (info.needs_pin) {
        box.appendChild(el('p', null, 'Введите PIN для подтверждения:'));
        pin = el('input');
        pin.type = 'password';
        pin.setAttribute('inputmode', 'numeric');
        pin.setAttribute('autocomplete', 'off');
        pin.maxLength = 12;
        box.appendChild(pin);
      }
      var force = null;
      if (action === 'shutdown' || action === 'restart') {
        var lab = el('label', 'check');
        force = el('input');
        force.type = 'checkbox';
        lab.appendChild(force);
        lab.appendChild(el('span', null, 'закрыть программы принудительно (несохранённое пропадёт)'));
        box.appendChild(lab);
      }
      var msg = el('p', 'err');
      box.appendChild(msg);
      var btns = el('div', 'btns');
      var no = el('button', null, 'Отмена');
      no.addEventListener('click', function () { showMachine(); });
      var yes = el('button', 'danger', text.btn);
      yes.addEventListener('click', function () {
        msg.textContent = '';
        yes.disabled = true;
        api('POST', '/api/power/execute', {
          action: action, code: info.code,
          pin: pin ? pin.value : '', force: force ? force.checked : false
        }).then(function (x) {
          yes.disabled = false;
          if (x.ok) {
            closeSheet();
            if (info.irreversible) banner('Ноутбук засыпает. Связь прервётся.', 'err');
            else { banner('Запланировано через ' + (x.data.delay_sec || 30) + ' с. Отменить можно в разделе «Ноутбук».'); refreshMachine(); }
            return;
          }
          var code = x.data && x.data.error;
          if (x.status === 403 && code === 'bad_pin') { msg.textContent = 'Неверный PIN.'; if (pin) { pin.value = ''; pin.focus(); } return; }
          if (x.status === 401 || x.status === 423) return; // the app locks or logs out
          msg.textContent = errText(code);
        });
      });
      btns.appendChild(no);
      btns.appendChild(yes);
      box.appendChild(btns);
      body.appendChild(box);
      if (pin) setTimeout(function () { pin.focus(); }, 50);
    });
  }

  // ---- voice dictation --------------------------------------------------------------------------------
  // The browser's speech recognition turns speech into text that lands in an
  // editable box. Nothing is sent until you press "Отправить".
  var SR = window.SpeechRecognition || window.webkitSpeechRecognition;
  var recog = null;
  var listening = false;
  var voiceBase = '';

  var VOICE_ERR = {
    'not-allowed': 'Нет доступа к микрофону: разрешите его для этого сайта в настройках браузера.',
    'service-not-allowed': 'Нет доступа к микрофону или распознаванию речи.',
    'no-speech': 'Речь не распознана. Попробуйте ещё раз.',
    'audio-capture': 'Микрофон не найден.',
    'network': 'Нет связи с сервисом распознавания речи (нужен интернет).',
    'language-not-supported': 'Этот язык не поддерживается.'
  };

  function openCompose(startListening) {
    $('compose').hidden = false;
    scheduleFit();
    if (startListening) startVoice(); else $('compose-text').focus();
  }
  function closeCompose() {
    stopVoice();
    $('compose').hidden = true;
    scheduleFit();
  }

  function setListening(on) {
    listening = on;
    $('voice-toggle').classList.toggle('on', on);
  }

  function startVoice() {
    if (!SR) { banner('Этот браузер не умеет распознавать речь. Используйте микрофон на клавиатуре телефона.', 'err'); return; }
    if (!window.isSecureContext) { banner('Голосовой ввод работает только по HTTPS.', 'err'); return; }
    stopVoice();
    var ta = $('compose-text');
    voiceBase = ta.value && !/\s$/.test(ta.value) ? ta.value + ' ' : ta.value;
    recog = new SR();
    recog.lang = $('voice-lang').value;
    recog.interimResults = true;
    recog.continuous = false;
    recog.maxAlternatives = 1;
    recog.onresult = function (ev) {
      var text = '';
      for (var i = 0; i < ev.results.length; i++) text += ev.results[i][0].transcript;
      ta.value = voiceBase + text;
    };
    recog.onerror = function (ev) { banner(VOICE_ERR[ev.error] || ('Ошибка распознавания: ' + ev.error), 'err'); };
    recog.onend = function () { setListening(false); recog = null; };
    try { recog.start(); setListening(true); } catch (e) { setListening(false); }
  }
  function stopVoice() {
    if (recog) { try { recog.stop(); } catch (e) { /* ignore */ } }
    setListening(false);
  }

  function composeText() { return $('compose-text').value.replace(/\s*\n\s*/g, ' ').trim(); }

  $('voice-toggle').addEventListener('click', function () { if (listening) stopVoice(); else startVoice(); });
  // Key-bar buttons that need a real click (see the pointerdown handler).
  $('keys').addEventListener('click', function (ev) {
    var b = ev.target.closest('button');
    if (!b) return;
    var k = b.getAttribute('data-seq');
    if (k === 'voice') openCompose(true);
    else if (k === 'reader') openReader();
  });
  $('compose-close').addEventListener('click', function () { closeCompose(); term.focus(); });
  $('compose-paste').addEventListener('click', function () {
    var t = composeText();
    if (!t) return;
    if (!ws || ws.readyState !== 1) { banner('Нет соединения с сессией.', 'err'); return; }
    stopVoice();
    sendText(t);
    $('compose-text').value = '';
  });
  $('compose-send').addEventListener('click', function () {
    var t = composeText();
    if (!t) return;
    if (!ws || ws.readyState !== 1) { banner('Нет соединения с сессией.', 'err'); return; }
    stopVoice();
    sendText(t);
    setTimeout(function () { sendText('\r'); }, 80); // Enter as a separate keypress
    $('compose-text').value = '';
  });

  // ---- lifecycle ---------------------------------------------------------------------------------
  // Reconnect promptly when the phone wakes up or regains the network.
  function wake() { if (current && !ws && !lockShown) { retry = 0; connect(); } }
  document.addEventListener('visibilitychange', function () {
    if (document.hidden) { hiddenAt = Date.now(); return; }
    // Coming back after more than 30 s counts as opening the app again.
    if (lockEnabled && unlockKey && hiddenAt && Date.now() - hiddenAt > 30000) { lockNow(); return; }
    wake();
  });
  window.addEventListener('online', wake);

  function startup() {
    booted = true;
    applyFont();
    refreshMachine();
    var m = /[#&]s=([0-9a-f]+)/.exec(location.hash);
    loadSessions().then(function () {
      if (m && sessionById(m[1])) openSession(m[1]);
      else if (sessions.length > 0) openSession(sessions[sessions.length - 1].id);
      else newSession();
    });
  }

  function boot() {
    api('GET', '/api/lock/status').then(function (r) {
      var st = (r.ok && r.data) || {};
      lockEnabled = !!st.enabled;
      idleSeconds = st.idle_seconds || 0;
      if (lockEnabled && !st.unlocked) { showLock(); return; }
      startup();
    });
  }
  boot();
})();
