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
  var polishDraft = '';
  var myName = '';

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

  function messageType(text) {
    if (text.indexOf('[AI]') === 0) { return 'ai'; }
    if (/^\[.+对.+说\]：/.test(text)) { return 'sent'; }
    if (text.indexOf('成功更改用户名为 ') === 0 ||
        /^(你被踢出|服务器正在关闭|格式有误|用法：|该用户名不存在|当前用户名已被使用|用户名未变化|消息内容不能为空)/.test(text) ||
        /^\[[^\]]+\]:已(上线|下线)$/.test(text) ||
        /^\[[^\]]+\]:在线\.\.$/.test(text)) { return 'system'; }
    if (text.indexOf('[' + myName + ']:') === 0 && myName) { return 'sent'; }
    return 'received';
  }

  function connect(nick) {
    meNameEl.textContent = '连接中…';
    setStatus('连接中…', '');
    es = new EventSource('/events?name=' + encodeURIComponent(nick || ''));
    es.addEventListener('init', function (e) {
      var data = JSON.parse(e.data);
      session = data.session;
      myName = data.name;
      meNameEl.textContent = myName;
      setStatus('已连接', 'ok');
      system('已连接到聊天服务器');
    });
    es.addEventListener('message', function (e) {
      // 改名确认前仍用旧用户名；失败时不误认为已经改名。
      if (e.data.indexOf('成功更改用户名为 ') === 0) {
        myName = e.data.slice('成功更改用户名为 '.length);
        meNameEl.textContent = myName;
      }
      append(e.data, messageType(e.data));
      var prefix = '[AI] 润色建议：';
      if (polishDraft && e.data.indexOf(prefix) === 0) {
        if (inputEl.value === polishDraft) {
          inputEl.value = e.data.slice(prefix.length);
          inputEl.focus();
        } else {
          system('草稿已变化，润色建议未自动覆盖输入框');
        }
        polishDraft = '';
      } else if (polishDraft && (e.data.indexOf('[AI] 润色失败') === 0 ||
          e.data.indexOf('[AI] 未配置模型服务') === 0 ||
          e.data.indexOf('[AI] 上一个请求还在处理中') === 0)) {
        polishDraft = '';
      }
    });
    es.onerror = function () {
      polishDraft = '';
      setStatus('连接已断开', 'err');
      if (es) { es.close(); es = null; }
      session = '';
      myName = '';
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
        if (text.indexOf('/polish ') === 0) { polishDraft = ''; }
        resp.text().then(function (msg) { system('发送失败：' + msg); });
      } else if (text.indexOf('/ai ') === 0) {
        append('我对 AI 说：' + text.slice(4), 'ai');
      }
    }).catch(function (err) {
      if (text.indexOf('/polish ') === 0) { polishDraft = ''; }
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
  document.getElementById('btn-polish').addEventListener('click', function () {
    var draft = inputEl.value.trim();
    if (!draft) { system('请先输入要润色的内容'); return; }
    if (!session) { system('尚未连接，请稍候'); return; }
    if (polishDraft) { system('上一次润色还在处理中'); return; }
    polishDraft = inputEl.value;
    send('/polish ' + draft);
  });
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
    polishDraft = '';
    if (es) { es.close(); es = null; }
    session = '';
    myName = '';
    setStatus('已断开', 'err');
    system('已主动断开连接，刷新页面可重新连接');
  });

  var nick = prompt('请输入昵称（留空使用默认）：', '用户' + Math.floor(Math.random() * 10000));
  connect(nick || '');
})();
