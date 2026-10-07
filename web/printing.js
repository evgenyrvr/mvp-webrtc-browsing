'use strict';
window.attachPrinting = function(channel) {
  const panel = document.getElementById('print-documents');
  const note = document.createElement('p'); panel.replaceChildren(note); panel.hidden = true;
  let active = true, job = null, serial = Promise.resolve();
  const documents = new Map();
  channel.binaryType = 'arraybuffer';
  const hex = bytes => Array.from(bytes, n => n.toString(16).padStart(2, '0')).join('');
  function updateNote() {
    note.textContent = job ? 'Получение документа для локальной печати…' : [...documents.values()].some(item => !item.opened) ? 'Документ готов. Откройте PDF и выберите печать в локальном браузере.' : '';
    note.hidden = !note.textContent; panel.hidden = !job && !documents.size;
  }
  const clear = () => { job = null; for (const item of documents.values()) URL.revokeObjectURL(item.url); documents.clear(); panel.replaceChildren(note); updateNote(); };
  function ack(id) { if (channel.readyState === 'open') channel.send(JSON.stringify({v:1,type:'job-ack',id})); }
  async function receive(data) {
    if (!active) return;
    if (typeof data === 'string') {
      if (data.length > 1024) throw Error('Некорректное сообщение печати');
      const m = JSON.parse(data);
      if (m.v !== 1) throw Error('Неизвестный протокол печати');
      if (m.type === 'print-revoked') { clear(); return; }
      if (m.type === 'job-start') {
        if (!/^[a-f0-9]{32}$/.test(m.id) || !/^[a-f0-9]{64}$/.test(m.sha256) || !Number.isInteger(m.size) || m.size < 10 || m.size > 25*1024*1024) throw Error('Некорректный размер документа');
        job = {id:m.id, sha:m.sha256, bytes:new Uint8Array(m.size), offset:0};
        updateNote();
      } else if (m.type === 'job-end') {
        if (!job || job.id !== m.id || job.offset !== job.bytes.length) throw Error('Документ получен не полностью');
        const complete = job; job = null;
        const sum = hex(new Uint8Array(await crypto.subtle.digest('SHA-256', complete.bytes)));
        if (!active) return;
        if (sum !== complete.sha) throw Error('Контрольная сумма документа не совпадает');
        if (!documents.has(m.id)) {
          if (documents.size >= 2) { const [id, old] = documents.entries().next().value; URL.revokeObjectURL(old.url); old.row.remove(); documents.delete(id); }
          const url = URL.createObjectURL(new Blob([complete.bytes], {type:'application/pdf'}));
          const row = document.createElement('p'), link = document.createElement('a'), remove = document.createElement('button');
          link.href = url; link.target = '_blank'; link.rel = 'noopener'; link.textContent = 'Открыть PDF для печати';
          remove.type = 'button'; remove.textContent = 'Готово'; remove.title = 'Убрать документ после печати';
          link.addEventListener('click', () => { const item = documents.get(m.id); if (item) item.opened = true; updateNote(); });
          remove.onclick = () => { URL.revokeObjectURL(url); row.remove(); documents.delete(m.id); updateNote(); };
          row.append(link, ' ', remove); panel.append(row); documents.set(m.id, {url,row,opened:false});
        }
        ack(m.id); updateNote();
      } else throw Error('Неизвестное сообщение печати');
    } else {
      const bytes = new Uint8Array(data);
      if (!job || bytes.length <= 20 || bytes.length > 16384 || hex(bytes.subarray(0,16)) !== job.id || new DataView(data).getUint32(16) !== job.offset || job.offset + bytes.length - 20 > job.bytes.length) throw Error('Некорректная часть документа');
      job.bytes.set(bytes.subarray(20), job.offset); job.offset += bytes.length - 20;
    }
  }
  channel.onmessage = event => { serial = serial.then(() => receive(event.data)).catch(error => { job = null; note.textContent = error.message; note.hidden = false; panel.hidden = false; channel.close(); }); };
  return () => { active = false; channel.onmessage = null; clear(); note.textContent = ''; };
};
