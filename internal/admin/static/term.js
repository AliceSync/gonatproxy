// Terminal JS: uses fetch + ReadableStream to consume the SSE stream from
// /api/term/run?cmd=... and decode base64 output chunks live.
(function () {
  const out = document.getElementById('termOut');
  const input = document.getElementById('cmdInput');
  const errBox = document.getElementById('termErr');
  let busy = false;

  function b64decode(b64){ try { return atob(b64); } catch(e){ return ''; } }

  function append(text) {
    out.textContent += text;
    out.scrollTop = out.scrollHeight;
  }

  async function run(cmd) {
    if (busy) return;
    busy = true; errBox.textContent='';
    append('❯ ' + cmd + '\n');
    document.getElementById('cmdForm').classList.add('busy');
    const resp = await fetch('/api/term/run?cmd=' + encodeURIComponent(cmd), {method:'POST'});
    if (!resp.ok || !resp.body) {
      const txt = await resp.text().catch(()=>'');
      append((txt || ('错误 ' + resp.status)) + '\n');
      busy = false; document.getElementById('cmdForm').classList.remove('busy');
      return;
    }
    const reader = resp.body.getReader();
    const decoder = new TextDecoder();
    let buf = '';
    try {
      while (true) {
        const {done, value} = await reader.read();
        if (done) break;
        buf += decoder.decode(value, {stream:true});
        // SSE events separated by blank line
        let idx;
        while ((idx = buf.indexOf('\n\n')) !== -1) {
          const block = buf.slice(0, idx); buf = buf.slice(idx+2);
          const dataLine = block.split('\n').find(l => l.startsWith('data:'));
          if (!dataLine) continue;
          const payload = dataLine.slice(5).trim();
          if (block.startsWith('event: done')) {
            append('\n[exit code: ' + payload + ']\n');
          } else {
            append(b64decode(payload));
          }
        }
      }
    } finally { reader.releaseLock(); }
    busy = false; document.getElementById('cmdForm').classList.remove('busy');
    input.focus();
  }

  document.getElementById('cmdForm').addEventListener('submit', function(e){
    e.preventDefault();
    const cmd = input.value.trim();
    if (!cmd) return;
    input.value = '';
    run(cmd);
  });
  input.focus();
})();
