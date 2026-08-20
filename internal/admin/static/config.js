// Dynamic config editor: loads the server config via /api/config, renders
// editable rows for routes and NAT clients, and saves via /api/config/save.
(function () {
  let cfg = null;

  function $(sel) { return document.querySelector(sel); }
  function setBasic() {
    const map = {
      'listen': cfg.listen, 'public_addr': cfg.public_addr || '',
      'cert_file': cfg.cert_file || '', 'key_file': cfg.key_file || '',
      'cert_check_seconds': cfg.cert_check_seconds || 30,
      'dial_timeout_seconds': cfg.dial_timeout_seconds || 10,
      'log_file': cfg.log_file || '',
      'log_max_bytes': cfg.log_max_bytes || '',
      'log_max_files': cfg.log_max_files || ''
    };
    // additional plain string fields without number parsing
    const mapStr = { 'deploy_dir': (cfg.deploy_dir)||'' };
    document.querySelectorAll('input[data-f]').forEach(inp => {
      const f = inp.dataset.f;
      if (f === 'admin.listen') inp.value = cfg.admin ? (cfg.admin.listen || '') : '';
      else if (f === 'admin.tls') inp.checked = !!(cfg.admin && cfg.admin.tls);
      else if (f === 'admin.cert_file') inp.value = cfg.admin ? (cfg.admin.cert_file || '') : '';
      else if (f === 'admin.key_file') inp.value = cfg.admin ? (cfg.admin.key_file || '') : '';
      else if (f === 'admin.username') inp.value = cfg.admin ? (cfg.admin.username || '') : '';
      else if (f === 'admin.newpassword') inp.value = '';
      else if (f === 'deploy_dir') inp.value = mapStr[f];
      else inp.value = map[f] !== undefined ? map[f] : '';
    });
  }
  function readBasic() {
    const m = {};
    document.querySelectorAll('input[data-f]').forEach(inp => {
      const f = inp.dataset.f;
      if (f === 'admin.listen') cfg.admin = cfg.admin || {}, cfg.admin.listen = inp.value;
      else if (f === 'admin.tls') cfg.admin = cfg.admin || {}, cfg.admin.tls = inp.checked;
      else if (f === 'admin.cert_file') cfg.admin = cfg.admin || {}, cfg.admin.cert_file = inp.value;
      else if (f === 'admin.key_file') cfg.admin = cfg.admin || {}, cfg.admin.key_file = inp.value;
      else if (f === 'admin.username') cfg.admin = cfg.admin || {}, cfg.admin.username = inp.value;
      else if (f === 'admin.newpassword') newPassword = inp.value;
      else m[f] = inp.value;
    });
    cfg.listen = m.listen; cfg.public_addr = m.public_addr;
    cfg.cert_file = m.cert_file; cfg.key_file = m.key_file;
    cfg.cert_check_seconds = parseInt(m.cert_check_seconds) || 30;
    cfg.dial_timeout_seconds = parseInt(m.dial_timeout_seconds) || 10;
    cfg.log_file = m.log_file;
    cfg.log_max_bytes = parseInt(m.log_max_bytes) || 0;
    cfg.log_max_files = parseInt(m.log_max_files) || 0;
    if (m.log_file === '') delete cfg.log_file;
    if (cfg.log_max_bytes <= 0) delete cfg.log_max_bytes;
    if (cfg.log_max_files <= 0) delete cfg.log_max_files;
    // deploy_dir is a plain string
    const dd = document.querySelector('input[data-f="deploy_dir"]');
    if (dd && dd.value) cfg.deploy_dir = dd.value; else delete cfg.deploy_dir;
  }

  function row(html) { const d = document.createElement('div'); d.innerHTML = html.trim(); return d.firstChild; }

  window.addRoute = function () {
    if (!cfg.routes) cfg.routes = [];
    const r = { name: '', listen_port: '', target: '' };
    cfg.routes.push(r);
    renderRoutes();
  };
  window.delRoute = function (i) {
    const nm = cfg.routes[i] && cfg.routes[i].name;
    DSH.confirm('删除路由“' + (nm||i) + '”？', {danger:true, okText:'删除'}).then(ok=>{ if(ok){ cfg.routes.splice(i, 1); renderRoutes(); } });
  };
  function renderRoutes() {
    const box = $('#routeRows'); box.innerHTML = '';
    if (!cfg.routes || !cfg.routes.length) { box.innerHTML = '<p class="muted">暂无路由</p>'; return; }
    cfg.routes.forEach((r, i) => {
      const el = row(`
        <div class="route-row">
          <input class="r-name" placeholder="名称" value="${esc(r.name||'')}">
          <input class="r-port" type="number" min="1" max="65535" placeholder="端口" value="${r.listen_port||''}">
          <select class="r-mode">
            <option value="target">服务端目标</option>
            <option value="nat">NAT 服务</option>
          </select>
          <input class="r-target" placeholder="host:port" value="${esc(r.target||'')}">
          <input class="r-natclient" placeholder="NAT id" value="${esc(r.nat_client||'')}">
          <input class="r-service" placeholder="服务名" value="${esc(r.service||'')}">
          <button class="btn btn-danger btn-sm" onclick="delRoute(${i})">删除</button>
        </div>`);
      box.appendChild(el);
      const mode = (r.nat_client) ? 'nat' : 'target';
      el.querySelector('.r-mode').value = mode;
      applyMode(el, mode);
      el.querySelector('.r-mode').addEventListener('change', e => applyMode(el, e.target.value));
      // Write edits straight back to cfg.routes[i] so re-rendering (e.g. adding
      // another row) never wipes what the user already typed.
      el.querySelector('.r-name').addEventListener('input', e => r.name = e.target.value);
      el.querySelector('.r-port').addEventListener('input', e => r.listen_port = parseInt(e.target.value) || 0);
      el.querySelector('.r-target').addEventListener('input', e => r.target = e.target.value);
      el.querySelector('.r-natclient').addEventListener('input', e => r.nat_client = e.target.value);
      el.querySelector('.r-service').addEventListener('input', e => r.service = e.target.value);
    });
  }
  function applyMode(el, mode) {
    const nat = mode === 'nat';
    el.querySelector('.r-target').style.display = nat ? 'none' : '';
    el.querySelector('.r-natclient').style.display = nat ? '' : 'none';
    el.querySelector('.r-service').style.display = nat ? '' : 'none';
    el.querySelector('.r-natclient').required = nat;
    el.querySelector('.r-service').required = nat;
  }
  function readRoutes() {
    const rows = $('#routeRows').children;
    const out = [];
    for (const el of rows) {
      if (!el.classList || !el.classList.contains('route-row')) continue;
      const mode = el.querySelector('.r-mode').value;
      const r = { name: el.querySelector('.r-name').value, listen_port: parseInt(el.querySelector('.r-port').value) || 0 };
      if (mode === 'target') { r.target = el.querySelector('.r-target').value; }
      else { r.nat_client = el.querySelector('.r-natclient').value; r.service = el.querySelector('.r-service').value; }
      out.push(r);
    }
    cfg.routes = out;
  }

  window.addClient = function () {
    if (!cfg.clients) cfg.clients = {};
    DSH.prompt('NAT 客户端 ID（新，唯一）', 'nat-' + (Object.keys(cfg.clients).length + 1), {okText:'添加'}).then(id=>{
      if (!id || !id.trim()) return;
      id = id.trim();
      if (cfg.clients[id]){ DSH.alert('已存在同名 NAT 客户端：'+id); return; }
      cfg.clients[id] = { secret: randTokenHex(24), services: [] };
      renderClients();
    });
  };
  // Generate a strong random secret for a NAT client token.
  function randTokenHex(n){
    if (window.crypto && window.crypto.getRandomValues){
      var a = new Uint8Array(n); window.crypto.getRandomValues(a);
      return Array.prototype.map.call(a, function(b){ return ('0'+b.toString(16)).slice(-2); }).join('');
    }
    var s=''; for (var i=0;i<n*2;i++) s += '0123456789abcdef'[Math.floor(Math.random()*16)];
    return s;
  }
  window.genSecret = function (id) {
    if (!cfg.clients[id]) return;
    cfg.clients[id].secret = randTokenHex(24);
    renderClients();
    DSH.toast('已生成新的令牌并回填', 'ok');
  };
  window.delClient = function (id) {
    DSH.confirm('删除 NAT 客户端 ' + id + ' 及其全部服务？', {danger:true, okText:'删除'}).then(ok=>{
      if(!ok) return; delete cfg.clients[id]; renderClients();
    });
  };
  window.addService = function (id) {
    if (!cfg.clients[id].services) cfg.clients[id].services = [];
    cfg.clients[id].services.push({ name: '', port: '', addr: '' });
    renderClients();
  };
  window.delService = function (id, i) {
    DSH.confirm('删除服务？', {danger:true, okText:'删除'}).then(ok=>{ if(ok){ cfg.clients[id].services.splice(i, 1); renderClients(); } });
  };
  function renderClients() {
    const box = $('#clientRows'); box.innerHTML = '';
    if (!cfg.clients || !Object.keys(cfg.clients).length) { box.innerHTML = '<p class="muted">暂无 NAT 客户端</p>'; return; }
    for (const id of Object.keys(cfg.clients)) {
      const nc = cfg.clients[id];
      const el = row(`
        <div class="client-card">
          <div class="client-head">
            <strong>${esc(id)}</strong>
            <input class="c-secret" type="text" placeholder="secret（令牌）" value="${esc(nc.secret||'')}">
            <button class="btn btn-ghost btn-sm" onclick="genSecret('${id}')">生成</button>
            <button class="btn btn-danger btn-sm" onclick="delClient('${id}')">删除客户端</button>
          </div>
          <div class="services" data-id="${id}"></div>
          <button class="btn btn-ghost btn-sm" onclick="addService('${id}')">+ 服务</button>
        </div>`);
      box.appendChild(el);
      const servDiv = el.querySelector('.services');
      function renderSvc() {
        servDiv.innerHTML = '';
        (nc.services||[]).forEach((svc, i) => {
          servDiv.appendChild(row(`
            <div class="svc-row">
              <input class="s-name" placeholder="名称" value="${esc(svc.name||'')}">
              <input class="s-port" type="number" min="1" max="65535" placeholder="本地后端端口(非监听)" value="${svc.port||''}">
              <input class="s-addr" placeholder="本地地址(默认 127.0.0.1:端口)" value="${esc(svc.addr||'')}">
              <button class="btn btn-danger btn-sm" onclick="delService('${id}',${i})">删除</button>
            </div>`));
          const rowEl = servDiv.lastChild;
          // keep edits in cfg.clients live so re-render (add service/client) doesn't lose them
          rowEl.querySelector('.s-name').addEventListener('input', e => svc.name = e.target.value);
          rowEl.querySelector('.s-port').addEventListener('input', e => svc.port = parseInt(e.target.value) || 0);
          rowEl.querySelector('.s-addr').addEventListener('input', e => svc.addr = e.target.value);
        });
      }
      renderSvc();
      // persist service edits on save via read
      el.querySelector('.c-secret').addEventListener('input', e => nc.secret = e.target.value);
    }
  }
  function readClients() {
    const cards = $('#clientRows').children;
    const out = {};
    for (const card of cards) {
      if (!card.classList || !card.classList.contains('client-card')) continue;
      const head = card.querySelector('.client-head strong').textContent;
      const id = head;
      const nc = { secret: card.querySelector('.c-secret').value, services: [] };
      card.querySelectorAll('.services .svc-row').forEach(sr => {
        nc.services.push({
          name: sr.querySelector('.s-name').value,
          port: parseInt(sr.querySelector('.s-port').value) || 0,
          addr: sr.querySelector('.s-addr').value
        });
      });
      out[id] = nc;
    }
    cfg.clients = out;
  }

  let newPassword = '';
  function esc(s) { return String(s).replace(/&/g,'&amp;').replace(/"/g,'&quot;').replace(/</g,'&lt;').replace(/>/g,'&gt;'); }
  function msg(text, isErr) {
    DSH.toast(text, isErr ? 'err' : 'ok');
  }

  window.save = function () {
    readBasic(); readRoutes(); readClients();
    cfg.admin = cfg.admin || {};
    delete cfg.admin.password_hash; // never send hash; server preserves it
    fetch('/api/config/save', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({ config: cfg, new_password: newPassword })
    }).then(r => r.json()).then(res => {
      if (res.ok) { msg('已保存并应用', false); loadConfig(); }
      else msg('保存失败: ' + (res.error || ''), true);
    }).catch(e => msg('网络错误: ' + e, true));
  };

  function loadConfig() {
    fetch('/api/config').then(r => r.json()).then(data => { cfg = data; setBasic(); renderRoutes(); renderClients(); })
      .catch(e => msg('加载配置失败', true));
  }

  // --- file selector modal ---
  let fsField = '';
  // Ensure the modal exists in the DOM. Under SPA navigation only <main id="view">
  // is swapped, and this modal lives outside it, so create it lazily if missing
  // (idempotent — never duplicates when re-initialising the page).
  function ensureFsModal() {
    let m = document.getElementById('fsModal');
    if (m) return m;
    m = document.createElement('div');
    m.className = 'fs-bg';
    m.id = 'fsModal';
    m.innerHTML =
      '<div class="fs-frame">' +
        '<div class="fs-top"><strong id="fsTitle">选择文件</strong><button type="button" class="btn btn-ghost btn-sm" onclick="closeFs()">关闭</button></div>' +
        '<iframe class="fs-iframe" id="fsIframe"></iframe>' +
      '</div>';
    document.body.appendChild(m);
    return m;
  }
  window.pickFile = function (field) {
    fsField = field;
    const modal = ensureFsModal();
    document.getElementById('fsTitle').textContent = '选择文件 -> ' + field;
    document.getElementById('fsIframe').src = '/files?select=1&field=' + field;
    modal.classList.add('open');
  };
  window.closeFs = function () {
    const modal = document.getElementById('fsModal');
    if (!modal) return;
    modal.classList.remove('open');
    document.getElementById('fsIframe').src = 'about:blank';
  };
  // Bind the global postMessage listener only once (SPA swaps re-run this file).
  if (!window.__cfgMsgBound) {
    window.__cfgMsgBound = true;
    window.addEventListener('message', function (ev) {
      if (ev.data && ev.data.type === 'fileSelect') {
        // fill the matching input[data-f]
        const inp = document.querySelector('input[data-f="' + ev.data.field + '"]');
        if (inp) { inp.value = ev.data.path; }
        window.closeFs();
      }
    });
  }

  // set forward-reference for admin fields handled in readBasic
  loadConfig();
})();
