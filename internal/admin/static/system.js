// System page JS: upload a new binary (optionally apply+restart immediately),
// apply a previously staged update, and standalone restart.
(function () {
  const res = document.getElementById('res');
  const fileInput = document.getElementById('binFile');
  const restartCheck = document.getElementById('restartNow');
  const applyBtn = document.getElementById('applyBtn');

  function msg(m, ok){ res.textContent = m; res.className = 'alert ' + (ok ? 'alert-success' : 'alert-error'); res.classList.remove('hidden'); setTimeout(() => res.classList.add('hidden'), 9000); }

  function refreshStatus(){
    fetch('/api/system/status').then(r=>r.json()).then(d=>{
      const sysd = document.getElementById('probeSystemd');
      if (d.managed){ sysd.textContent='systemd 管理'; sysd.className='badge badge-ok'; }
      else if (d.daemon==='1'){ sysd.textContent='start 守护进程'; sysd.className='badge badge-warn'; }
      else { sysd.textContent='前台进程'; sysd.className='badge badge-info'; }
      document.getElementById('probeText').innerHTML =
        'unit <code>'+d.unit+'</code> &nbsp; 配置 <code>'+d.config+'</code> &nbsp; 可执行文件 <code>'+d.exe+'</code>';
      const hint = document.getElementById('restartHint');
      hint.textContent = '重启始终采用 fork（self-fork），不检查/不委托 systemctl——即使当前是 systemd 管理也是一样。systemd 生命周期（启用/启动/停止等）请在【服务】页管理。';
      const hasStaged = d.staged==='1';
      applyBtn.disabled = !hasStaged;
      document.getElementById('stagedNote').textContent = hasStaged ? '已有待应用的更新文件：点击【应用已上传的更新并重启】生效。' : '';
    }).catch(()=>{});
  }

  // upload: if restart checked -> replace+restart (with confirm), else only stage.
  function uploadUpdate(){
    const f = fileInput.files[0];
    if (!f){ msg('请先选择要上传的程序文件', false); return; }
    const doRestart = restartCheck.checked;
    const doWork = () => {
      const fd = new FormData();
      fd.append('file', f);
      fd.append('restart', doRestart ? '1':'0');
      fd.append('apply', doRestart ? '1':'0');
      msg(doRestart ? '正在替换并重启…' : '正在上传（暂存）…', true);
      fetch('/api/system/update', {method:'POST', body:fd}).then(r=>r.json()).then(d=>{
        if (d.ok){ msg(d.ok, true); if (d.restarting) setTimeout(()=>location.href='/dashboard', 2500); else refreshStatus(); }
        else msg(d.error || '上传失败', false);
      }).catch(e=>msg('网络错误: '+e, false));
    };
    if (doRestart) DSH.confirm('将替换当前程序并立即重启（mv 替换 + fork 启动），现有连接会断开。\n确定继续？', {danger:true, okText:'替换并重启'}).then(ok=>{ if(ok) doWork(); });
    else doWork();
  }

  function applyStaged(){
    DSH.confirm('应用已上传的更新并重启？此操作会替换当前程序，现有连接会断开。', {danger:true, okText:'应用并重启'}).then(ok=>{
      if(!ok) return;
      fetch('/api/system/apply', {method:'POST'}).then(r=>r.json()).then(d=>{
        if (d.ok){ msg(d.ok, true); if (d.restarting) setTimeout(()=>location.href='/dashboard', 2500); }
        else msg(d.error || '应用失败', false);
      }).catch(e=>msg('网络错误: '+e, false));
    });
  }

  function restartNowFn(){
    DSH.confirm('立即重启当前服务？现有连接会断开。', {danger:true, okText:'立即重启'}).then(ok=>{
      if(!ok) return;
      fetch('/api/system/restart', {method:'POST'}).then(r=>r.json()).then(d=>{
        msg(d.ok || '正在重启…', true); if (d.restarting) setTimeout(()=>location.href='/dashboard', 2500);
      }).catch(e=>msg('网络错误: '+e, false));
    });
  }

  window.uploadUpdate = uploadUpdate;
  window.applyStaged  = applyStaged;
  window.restartNowFn = restartNowFn;

  fileInput.addEventListener('change', function(){ refreshStatus(); });
  refreshStatus();
})();
