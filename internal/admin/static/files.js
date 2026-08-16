// File manager JS (vanilla). In select mode, clicking a file posts the absolute
// path to the parent window/postMessage target so the config page can fill it.
(function () {
  let cur = '/';
  const select = new URLSearchParams(location.search).get('select') === '1';
  const field = new URLSearchParams(location.search).get('field') || '';
  const listBody = document.getElementById('listBody');
  const breadcrumb = document.getElementById('breadcrumb');
  const errBox = document.getElementById('err');

  function showErr(m){ errBox.textContent=m; errBox.classList.remove('hidden'); setTimeout(()=>errBox.classList.add('hidden'), 4000); }
  function fmt(m){ return new Date(m).toLocaleString(); }
  const esc = s => String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));

  function renderCrumbs() {
    // served by API crumbs
  }

  function render(data) {
    cur = data.path;
    // breadcrumb
    breadcrumb.innerHTML = '<a href="#" data-p="/">/</a>';
    data.crumbs.forEach(c => {
      const a = document.createElement('a'); a.href='#'; a.textContent='/'+c.name; a.dataset.p=c.path;
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
      nameTd.onclick = () => { try { parent.postMessage({type:'fileSelect', path:en.path, field}, '*'); window.close(); } catch(e){} };
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

  function load(p){ fetch('/api/files/list?path='+encodeURIComponent(p)).then(r=>r.json()).then(d=>{ if(d.error){showErr(d.error);return;} render(d); }).catch(e=>showErr('加载失败')); }

  // editor modal
  const modal = document.createElement('div'); modal.className='modal-bg hidden';
  modal.innerHTML = `<div class="modal">
      <div class="modal-head"><strong id="edTitle">编辑</strong><button class="btn btn-ghost btn-sm" onclick="document.querySelector('.modal-bg').classList.add('hidden')">关闭</button></div>
      <textarea id="edContent" class="ed-content"></textarea>
      <div class="modal-foot"><button class="btn btn-primary" id="edSave">保存</button></div>
    </div>`;
  document.body.appendChild(modal);
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
    if(!confirm('删除 '+path+' ？此操作不可撤销。')) return;
    fetch('/api/files/delete?path='+encodeURIComponent(path)).then(r=>r.json()).then(d=>{ if(!d.ok){showErr(d.error);} load(cur); }).catch(e=>showErr('删除失败'));
  }
  window.mkdir = ()=>{
    const p = prompt('新目录绝对路径（在当前目录下则填相对名）', cur+'/newdir');
    if(!p) return;
    fetch('/api/files/mkdir',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({path:p.replace(/\/+$/,'')})})
      .then(r=>r.json()).then(d=>{ if(!d.ok) showErr(d.error||'失败'); load(cur); }).catch(e=>showErr('创建失败'));
  };
  window.newFile = ()=>{
    const p = prompt('新文件绝对路径', cur+'/newfile');
    if(!p) return;
    fetch('/api/files/new',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({path:p})})
      .then(r=>r.json()).then(d=>{ if(!d.ok) showErr(d.error||'失败'); load(cur); }).catch(e=>showErr('创建失败'));
  };

  document.getElementById('fileUp').addEventListener('change', function(e){
    const fd = new FormData(); fd.append('dir', cur);
    for (const f of this.files) fd.append('file', f);
    fetch('/api/files/upload',{method:'POST', body: fd}).then(r=>r.json()).then(d=>{ if(!d.ok) showErr(d.error||'上传失败'); load(cur); this.value=''; })
      .catch(e=>showErr('上传失败'));
  });

  window.cd = p => { load(p); };
  document.getElementById('goTo').addEventListener('keydown', e=>{ if(e.key==='Enter') load(document.getElementById('goTo').value); });

  load('/');
})();
