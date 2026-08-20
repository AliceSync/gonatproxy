// File manager JS (vanilla). In select mode, clicking a file posts the absolute
// path to the parent window/postMessage target so the config page can fill it.
(function () {
  // Remember the current directory across SPA swaps (this file is re-run on each
  // visit, so persist state on window rather than in a per-run closure).
  let cur = window.__filesCur || '/';
  const select = new URLSearchParams(location.search).get('select') === '1';
  const field = new URLSearchParams(location.search).get('field') || '';
  const listBody = document.getElementById('listBody');
  const breadcrumb = document.getElementById('breadcrumb');

  function showErr(m){ DSH.toast(m, 'err'); }
  function fmt(m){ return new Date(m).toLocaleString(); }
  const esc = s => String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));

  function renderCrumbs() {
    // served by API crumbs
  }

  function render(data) {
    cur = data.path;
    window.__filesCur = cur;
    // breadcrumb: root '/' then each segment; the CSS adds the '/' separators
    breadcrumb.innerHTML = '<a href="#" data-p="/">/</a>';
    data.crumbs.forEach(c => {
      const a = document.createElement('a'); a.href='#'; a.textContent=c.name; a.dataset.p=c.path;
      breadcrumb.appendChild(a);
    });
    breadcrumb.querySelectorAll('a').forEach(a => a.addEventListener('click', e => { e.preventDefault(); load(a.dataset.p); }));

    listBody.innerHTML = '';
    if (data.path !== '/') {
      listBody.appendChild(row({name:'..', dir:true, path:parentPath(data.path), special:true}));
    }
    data.entries.forEach(en => listBody.appendChild(row(en)));
  }

  function parentPath(p){ const parts=p.replace(/\/+$/,'').split('/'); parts.pop(); return parts.join('/') || '/'; }

  function row(en) {
    const tr = document.createElement('tr');
    tr.className = en.dir ? 'dir' : '';
    const nameTd = document.createElement('td');
    nameTd.innerHTML = (en.dir ? '📁 ' : '📄 ') + esc(en.name);
    nameTd.className='name';
    if (en.dir) {
      nameTd.onclick = () => load(en.path);
      nameTd.style.cursor='pointer';
    } else if (select) {
      nameTd.title = '选择此文件';
      nameTd.onclick = () => selectPath(en.path);
      nameTd.style.cursor='pointer'; nameTd.style.color='var(--accent)';
    } else {
      nameTd.onclick = () => { /* single click = maybe edit */ };
    }
    tr.appendChild(nameTd);
    tr.appendChild(td(en.dir?'目录':'文件'));
    tr.appendChild(td(en.dir?'':fmtSize(en.size)));
    tr.appendChild(td(en.dir?'':en.mod_time));
    const ops = document.createElement('td'); ops.className='row-ops';
    if (!en.dir) {
      const edit=btn('编辑',()=>editFile(en.path));
      const dl=btn('下载',()=>location.href='/api/files/download?path='+encodeURIComponent(en.path));
      ops.appendChild(edit); ops.appendChild(dl);
    }
    const rn=btn('重命名',()=>renameFile(en.path, en.name, en.dir));
    rn.className='btn btn-ghost btn-sm'; // neutral for rename
    ops.appendChild(rn);
    if (!en.special) {
      const del=btn('删除',()=>delFile(en.path));
      ops.appendChild(del);
    }
    tr.appendChild(ops);
    return tr;
  }
  function td(t){ const x=document.createElement('td'); x.textContent=t; return x; }
  function btn(t,cb){ const b=document.createElement('button'); b.className='btn btn-danger btn-sm'; b.textContent=t; b.onclick=cb; return b; }
  function fmtSize(n){ if(n<1024) return n+' B'; return (n/1024).toFixed(1)+' KB'; }

  function renameFile(path, name, isDir){
    DSH.prompt('新名称（保留扩展名）', name, {title: isDir?'重命名目录':'重命名文件', okText:'重命名'}).then(n=>{
      if(!n || n===name) return;
      fetch('/api/files/rename',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({path, new_name:n})})
        .then(r=>r.json()).then(d=>{ if(d.ok) DSH.toast('已重命名','ok'); else showErr(d.error||'重命名失败'); load(cur); })
        .catch(e=>showErr('网络错误: '+e));
    });
  }

  function load(p){ fetch('/api/files/list?path='+encodeURIComponent(p)).then(r=>r.json()).then(d=>{ if(d.error){showErr(d.error);return;} render(d); }).catch(e=>showErr('加载失败')); }

  // editor modal (create once; SPA swaps re-run this file so guard by id)
  let modal = document.getElementById('edShell');
  if (!modal) {
    modal = document.createElement('div'); modal.className='modal-bg hidden'; modal.id='edShell';
    modal.innerHTML = `<div class="modal">
      <div class="modal-head"><strong id="edTitle">编辑</strong><button class="btn btn-ghost btn-sm" onclick="document.querySelector('.modal-bg').classList.add('hidden')">关闭</button></div>
      <textarea id="edContent" class="ed-content"></textarea>
      <div class="modal-foot"><button class="btn btn-primary" id="edSave">保存</button></div>
    </div>`;
    document.body.appendChild(modal);
  }
  function editFile(path){
    fetch('/api/files/read?path='+encodeURIComponent(path)).then(r=>r.json()).then(d=>{
      document.getElementById('edTitle').textContent = path;
      document.getElementById('edContent').value = d.content||'';
      modal.classList.remove('hidden');
      modal.onsaved = undefined;
      document.getElementById('edSave').onclick = () => {
        fetch('/api/files/write',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({path, content:document.getElementById('edContent').value})})
          .then(r=>r.json()).then(d=>{ if(!d.ok){showErr(d.error||'保存失败');} else { modal.classList.add('hidden'); } });
      };
    }).catch(e=>showErr('读取失败'));
  }

  function delFile(path){
    DSH.confirm('删除 ' + path + ' ？\n此操作不可撤销。', {danger:true, okText:'删除'}).then(ok=>{
      if(!ok) return;
      fetch('/api/files/delete?path='+encodeURIComponent(path)).then(r=>r.json()).then(d=>{ if(!d.ok){showErr(d.error);} load(cur); }).catch(e=>showErr('删除失败'));
    });
  }
  window.mkdir = ()=>{
    DSH.prompt('新目录绝对路径（在当前目录下则填相对名）', cur+'/newdir', {okText:'创建'}).then(p=>{
      if(!p) return;
      fetch('/api/files/mkdir',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({path:p.replace(/\/+$/,'')})})
        .then(r=>r.json()).then(d=>{ if(!d.ok) showErr(d.error||'失败'); load(cur); }).catch(e=>showErr('创建失败'));
    });
  };
  window.newFile = ()=>{
    DSH.prompt('新文件绝对路径', cur+'/newfile', {okText:'创建'}).then(p=>{
      if(!p) return;
      fetch('/api/files/new',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({path:p})})
        .then(r=>r.json()).then(d=>{ if(!d.ok) showErr(d.error||'失败'); load(cur); }).catch(e=>showErr('创建失败'));
    });
  };

  // upload with live progress + speed
  document.getElementById('fileUp').addEventListener('change', function(e){
    const input = this;
    const files = [].slice.call(input.files||[]);
    if (!files.length) return;
    const fd = new FormData(); fd.append('dir', cur);
    let total = 0; for (const f of files) { fd.append('file', f); total += f.size||0; }
    const running = files.length>1 ? ' 已选 '+files.length+' 个文件' : '';
    const progress = mkProgressBar();
    progress.textContent = '上传中… 0%'+running;

    const xhr = new XMLHttpRequest();
    xhr.open('POST', '/api/files/upload');
    let lastLoaded = 0, lastTime = performance.now(), speed = 0;
    xhr.upload.onprogress = function(ev){
      if (!ev.lengthComputable) return;
      const now = performance.now();
      const dt = (now - lastTime)/1000;
      if (dt > 0) { speed = (ev.loaded - lastLoaded)/dt; lastTime = now; lastLoaded = ev.loaded; }
      const pct = total>0 ? Math.round((ev.loaded/ev.total)*100) : 0;
      progress.textContent = '上传 ' + pct + '%' + running + '  ' + fmtSpeed(speed) + (speed>0?'/s':'');
      progress.style.width = pct + '%';
      styleProgress(progress, pct);
    };
    xhr.onload = function(){
      if (xhr.status===200){ try{ const d=JSON.parse(xhr.responseText); if(d.ok) DSH.toast('上传完成','ok'); else DSH.toast(d.error||'上传失败','err'); }catch(_){ DSH.toast('上传失败','err'); } }
      else DSH.toast('上传失败 ('+xhr.status+')','err');
      hideProgress(progress); load(cur); input.value='';
    };
    xhr.onerror = function(){ DSH.toast('网络错误，上传中断','err'); hideProgress(progress); input.value=''; };
    xhr.onabort = function(){ hideProgress(progress); };
    xhr.send(fd);
  });

  function fmtSpeed(b){ if(!isFinite(b)||b<0) return ''; if(b<1024) return Math.round(b)+' B'; if(b<1024*1024) return (b/1024).toFixed(1)+' KB'; return (b/1024/1024).toFixed(1)+' MB'; }
  function mkProgressBar(){
    let host = document.querySelector('.up-host');
    if (!host){ host=document.createElement('div'); host.className='up-host'; document.body.appendChild(host); }
    const p = document.createElement('div'); p.className='up-progress'; host.appendChild(p);
    return p;
  }
  function styleProgress(p, pct){ p.style.setProperty('--pct', pct+'%'); p.setAttribute('data-pct', pct); }
  function hideProgress(p){ p.style.width='100%'; p.classList.add('up-done'); setTimeout(()=>p.remove(), 400); }

  window.cd = p => { load(p); };
  document.getElementById('goTo').addEventListener('keydown', e=>{ if(e.key==='Enter') load(document.getElementById('goTo').value); });

  // select the CURRENT directory (for directory-valued fields like deploy_dir)
  function selectPath(path){
    try { parent.postMessage({type:'fileSelect', path, field}, '*'); window.close(); } catch(e){}
  }
  window.pickCurrentDir = () => selectPath(cur);

  load('/');
})();
