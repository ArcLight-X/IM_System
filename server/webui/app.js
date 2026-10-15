(function () {
  'use strict';

  var logEl = document.getElementById('log');
  var inputEl = document.getElementById('input');
  var modeEl = document.getElementById('mode');
  var targetEl = document.getElementById('target');
  var statusEl = document.getElementById('status');
  var meNameEl = document.getElementById('me-name');

  var session = '';
  var es = null;

  function setStatus(text, cls) {
    statusEl.textContent = text;
    statusEl.className = 'status' + (cls ? ' ' + cls : '');
  }

  function append(text, cls) {
    var div = document.createElement('div');
    div.className = 'msg' + (cls ? ' ' + cls : '');
    div.textContent = text;
    logEl.appendChild(div);
    logEl.scrollTop = logEl.scrollHeight;
    // 只保留最近 500 条，避免长时间运行占内存
    while (logEl.childElementCount > 500) {
      logEl.removeChild(logEl.firstChild);
    }
  }

  function system(text) { append(text, 'system'); }

  function connect(nick) {
    meNameEl.textContent = nick || '默认';
    setStatus('连接中…', '');
    es = new EventSource('/events?name=' + encodeURIComponent(nick || ''));
    es.addEventListener('init', function (e) {
      var data = JSON.parse(e.data);
      session = data.session;
      setStatus('已连接', 'ok');
      system('已连接到聊天服务器');
    });
    es.addEventListener('message', function (e) { append(e.data); });
    es.onerror = function () {
      setStatus('连接已断开', 'err');
      if (es) { es.close(); es = null; }
      session = '';
      system('与服务器的连接已断开，请刷新页面重连');
    };
  }

  function send(text) {
    text = String(text || '').replace(/\r?\n/g, ' ').trim();
    if (!text) { return; }
    if (!session) { system('尚未连接，请稍候'); return; }
    fetch('/send', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ session: session, text: text })
    }).then(function (resp) {
      if (!resp.ok) {
        resp.text().then(function (msg) { system('发送失败：' + msg); });
      }
    }).catch(function (err) {
      system('发送失败：' + err);
    });
  }

  function compose() {
    var value = inputEl.value;
    if (modeEl.value === 'private') {
      var target = targetEl.value.trim();
      if (!target) { system('请先填写私聊对象用户名'); return null; }
      return 'to|' + target + '|' + value;
    }
    if (modeEl.value === 'ai') { return '/ai ' + value; }
    return value;
  }

  function submit() {
    var text = compose();
    if (text === null) { return; }
    if (!text.trim()) { inputEl.value = ''; return; }
    send(text);
    inputEl.value = '';
    inputEl.focus();
  }

  document.getElementById('btn-send').addEventListener('click', submit);
  inputEl.addEventListener('keydown', function (e) {
    if (e.key === 'Enter') { e.preventDefault(); submit(); }
  });
  modeEl.addEventListener('change', function () {
    targetEl.classList.toggle('hidden', modeEl.value !== 'private');
    inputEl.placeholder = modeEl.value === 'ai' ? '向 AI 提问，回车发送' : '输入内容，回车发送';
  });
  document.getElementById('btn-who').addEventListener('click', function () { send('who'); });
  document.getElementById('btn-summary').addEventListener('click', function () { send('/summary 20'); });
  document.getElementById('btn-rename').addEventListener('click', function () {
    var newName = prompt('输入新的用户名：');
    if (newName) { send('rename|' + newName.trim()); }
  });
  document.getElementById('btn-new').addEventListener('click', function () {
    window.open('/', '_blank', 'width=560,height=760');
  });
  document.getElementById('btn-leave').addEventListener('click', function () {
    if (es) { es.close(); es = null; }
    session = '';
    setStatus('已断开', 'err');
    system('已主动断开连接，刷新页面可重新连接');
  });

  var nick = prompt('请输入昵称（留空使用默认）：', '用户' + Math.floor(Math.random() * 10000));
  connect(nick || '');
})();
