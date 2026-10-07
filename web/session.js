'use strict';
(() => {
  let disposePrinting;
  let selectedLayout = 'us';
  function showLayout() {
    for (const code of ['us', 'ru']) {
      const button = document.getElementById('layout-' + code);
      button.classList.toggle('selected-layout', code === selectedLayout);
      button.setAttribute('aria-pressed', String(code === selectedLayout));
    }
  }
  function chooseLayout(code) {
    if (!control || control.readyState !== 'open') return;
    send(control, {type:'layout', code}); selectedLayout = code; showLayout(); stage.focus();
  }
  showLayout();
  let id, csrf, ws, pc, control, pointer, heartbeat, mediaWatch, stableConnection, retry, sequence = 0, epoch = 0, attempts = 0, stopped = true;
  const stage = document.getElementById('remote-stage'), video = document.getElementById('remote-video'), state = document.getElementById('session-state');
  const status = text => { state.textContent = text; };
  function dispose() {
    if (disposePrinting) { disposePrinting(); disposePrinting = null; }
    clearInterval(heartbeat); clearInterval(mediaWatch); clearTimeout(stableConnection); clearTimeout(retry);
    if (ws) { ws.onclose = null; ws.close(); ws = null; }
    if (pc) { pc.onconnectionstatechange = null; pc.close(); pc = null; }
    control = pointer = null; video.srcObject = null;
  }
  function close() { stopped = true; epoch++; dispose(); }
  function send(channel, event) {
    if (!channel || channel.readyState !== 'open') return;
    if (channel.bufferedAmount > 65536) { reconnect('Сеть перегружена, восстанавливаем подключение…'); return; }
    channel.send(JSON.stringify({ v: 1, ...event }));
  }
  function reset() { send(control, { type: 'reset' }); }
  function reconnect(reason) {
    if (stopped) return;
    epoch++; dispose(); attempts++;
    if (attempts > 3) { status('Не удалось восстановить связь. Нажмите «Переподключить» в течение минуты.'); return; }
    status(reason); retry = setTimeout(connect, attempts * 1000);
  }
  async function gathered(connection) {
    if (connection.iceGatheringState === 'complete') return;
    await new Promise((resolve, reject) => {
      const timeout = setTimeout(() => { connection.removeEventListener('icegatheringstatechange', changed); reject(new Error('Не удалось собрать сетевые адреса')); }, 15000);
      function changed() { if (connection.iceGatheringState === 'complete') { clearTimeout(timeout); connection.removeEventListener('icegatheringstatechange', changed); resolve(); } }
      connection.addEventListener('icegatheringstatechange', changed); changed();
    });
  }
  function connect() {
    if (stopped) return;
    dispose(); const current = ++epoch;
    if (!window.isSecureContext || !window.RTCPeerConnection) { status('Для удалённого приложения откройте HTTPS-адрес сервера.'); return; }
    status('Подключение к приложению…');
    const socket = new WebSocket(`${location.protocol === 'https:' ? 'wss:' : 'ws:'}//${location.host}/api/v1/sessions/${id}/signal`); ws = socket;
    socket.onopen = () => {
      socket.send(JSON.stringify({ csrf_token: csrf }));
      heartbeat = setInterval(() => { if (socket.readyState === WebSocket.OPEN) socket.send(JSON.stringify({ type: 'ping' })); }, 2000);
    };
    socket.onmessage = async message => {
      if (epoch !== current) return;
      try {
        const event = JSON.parse(message.data);
        if (event.type === 'config') {
          const connection = new RTCPeerConnection({ ...event.rtc, iceServers: event.rtc.iceServers || [] }); pc = connection;
          connection.addTransceiver('video', { direction: 'recvonly' });
          control = connection.createDataChannel('control', { ordered: true });
          control.onopen = () => { if (epoch === current) send(control, {type:'layout', code:selectedLayout}); };
          disposePrinting = window.attachPrinting(connection.createDataChannel('print', { ordered: true }));
          pointer = connection.createDataChannel('pointer', { ordered: false, maxRetransmits: 0 });
          connection.ontrack = event => { if (epoch === current) { video.srcObject = event.streams[0] || new MediaStream([event.track]); video.play().catch(error => { if (epoch === current && error.name === 'NotAllowedError') status('Нажмите на изображение для воспроизведения'); }); } };
          connection.onconnectionstatechange = () => {
            if (epoch !== current) return;
            if (connection.connectionState === 'connected') {
              clearTimeout(stableConnection);
              stableConnection = setTimeout(() => { if (epoch === current && connection.connectionState === 'connected') attempts = 0; }, 10000);
              status('Соединение установлено, ожидаем изображение…');
              clearInterval(mediaWatch); let lastFrames = 0, stalled = 0;
              mediaWatch = setInterval(async () => {
                if (epoch !== current) return;
                try {
                  const stats = await connection.getStats(); if (epoch !== current) return;
                  let frames = 0;
                  stats.forEach(item => { if (item.type === 'inbound-rtp' && item.kind === 'video') frames += item.framesDecoded || 0; });
                  if (frames > lastFrames) { stalled = 0; status('Подключено · нажмите на изображение для управления'); }
                  else if (++stalled >= 5) status('Изображение не поступает. Нажмите «Переподключить»; если это не помогло, проверьте доступность сети.');
                  lastFrames = frames;
                } catch (_) { /* Connection may close while collecting statistics. */ }
              }, 2000);
            }
            if (connection.connectionState === 'failed') reconnect('Соединение прервано, восстанавливаем…');
            if (connection.connectionState === 'disconnected') status('Временная потеря связи…');
          };
          await connection.setLocalDescription(await connection.createOffer()); await gathered(connection);
          if (epoch === current && socket.readyState === WebSocket.OPEN) socket.send(JSON.stringify({ type: 'offer', sdp: connection.localDescription }));
        } else if (event.type === 'answer') {
          await pc.setRemoteDescription(event.sdp);
        } else if (event.type === 'error') {
          reconnect(event.message || 'Ошибка подключения');
        }
      } catch (error) { if (epoch === current) reconnect(error.message); }
    };
    socket.onclose = event => {
      if (epoch !== current) return;
      if (event.code === 4003) { close(); window.dispatchEvent(new Event('mvp-auth-expired')); return; }
      if (event.code === 4001) {
        close(); status('Приложение открыто в другой вкладке или на другом компьютере. Нажмите «Переподключить», чтобы продолжить здесь.');
        return;
      }
      if (event.code === 4002) {
        close(); status('Приложение завершено. Вернитесь в каталог и запустите его снова.');
        return;
      }
      reconnect('Связь с сервером потеряна, пробуем восстановить…');
    };
  }
  function coordinates(e) {
    const r = video.getBoundingClientRect(), ratio = (video.videoWidth || 1280) / (video.videoHeight || 720);
    let width = r.width, height = width / ratio; if (height > r.height) { height = r.height; width = height * ratio; }
    const x = (e.clientX - r.left - (r.width - width) / 2) / width, y = (e.clientY - r.top - (r.height - height) / 2) / height;
    return { x: Math.max(0, Math.min(1, x)), y: Math.max(0, Math.min(1, y)) };
  }
  stage.addEventListener('pointermove', e => send(pointer, { type: 'pointer', seq: ++sequence, ...coordinates(e) }));
  stage.addEventListener('pointerdown', e => { if (e.button > 2) return; e.preventDefault(); stage.focus(); if (video.paused && video.srcObject) video.play().catch(() => status('Не удалось начать воспроизведение')); stage.setPointerCapture(e.pointerId); send(control, { type: 'button', seq: ++sequence, down: true, button: e.button, ...coordinates(e) }); });
  stage.addEventListener('pointerup', e => { if (e.button > 2) return; e.preventDefault(); send(control, { type: 'button', seq: ++sequence, down: false, button: e.button, ...coordinates(e) }); if (stage.hasPointerCapture(e.pointerId)) stage.releasePointerCapture(e.pointerId); });
  stage.addEventListener('pointercancel', reset);
  stage.addEventListener('contextmenu', e => e.preventDefault());
  stage.addEventListener('wheel', e => { e.preventDefault(); send(control, { type: 'wheel', delta: e.deltaY === 0 ? 0 : e.deltaY > 0 ? 1 : -1 }); }, { passive: false });
  stage.addEventListener('keydown', e => { e.preventDefault(); if (!e.repeat) send(control, { type: 'key', code: e.code, down: true }); });
  stage.addEventListener('keyup', e => { e.preventDefault(); send(control, { type: 'key', code: e.code, down: false }); });
  stage.addEventListener('blur', reset); window.addEventListener('blur', reset);
  document.addEventListener('visibilitychange', () => { if (document.hidden) reset(); });
  window.addEventListener('pagehide', close);
  document.getElementById('layout-us').onclick = () => chooseLayout('us');
  document.getElementById('layout-ru').onclick = () => chooseLayout('ru');
  document.getElementById('remote-address').onclick = () => { stage.focus(); for (const [code, down] of [['ControlLeft', true], ['KeyL', true], ['KeyL', false], ['ControlLeft', false]]) send(control, { type: 'key', code, down }); };
  document.getElementById('remote-fullscreen').onclick = () => stage.requestFullscreen().catch(() => status('Полноэкранный режим недоступен'));
  document.getElementById('reconnect-app').onclick = () => { stopped = false; attempts = 0; connect(); };
  window.mvpSession = { start(sessionID, token) { if (!stopped && id === sessionID) return; close(); id = sessionID; csrf = token; stopped = false; attempts = 0; connect(); }, close };
})();
