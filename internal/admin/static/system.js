// System page JS: upload binary, apply+restart, standalone restart.
(function () {
  const res = document.getElementById('res');
  const confirmBox = document.getElementById('restartConfirm');
  const restartCheck = document.getElementById('restartNow');
  const fileInput = document.getElementById('binFile');

  function msg(m, ok){ res.textContent = m; res.className = 'alert ' + (ok ? 'alert-success' : 'alert-error'); res.classList.remove('hidden'); setTimeout(() => res.classList.add('hidden'), 8000); }

  function restartNowFn(){ fetch('/api/system/restart', {method:'POST'}).then(r=>r.json()).then(d=>{
    msg(d.ok || '正在重启…', true); if(d.restarting) setTimeout(()=>location.href='/dashboard', 2500);
  }).catch(e=>msg('网络错误: '+e, false)); }

  // process status probe
  fetch('/api/system/status').then(r=>r.json()).then(d=>{
    const sysd = document.getElementById('probeSystemd');
    if (d.managed) { sysd.textContent='systemd 管理'; sysd.className='badge badge-ok'; }
    else if (d.daemon==='1') { sysd.textContent='start 守护进程'; sysd.className='badge badge-warn'; }
    else { sysd.textContent='前台进程'; sysd.className='badge badge-info'; }
    document.getElementById('probeText').innerHTML =
      'unit <code>'+d.unit+'</code> &nbsp; 配置 <code>'+d.config+'</code> &nbsp; 可执行文件 <code>'+d.exe+'</code>';
    const hint = document.getElementById('restartHint');
    if (d.managed) hint.textContent = '当前由 systemd 管理：重启会通过 systemctl restart 执行。';
    else hint.textContent = '当前非 systemd 管理：重启将 fork 新进程（不会接管 systemd 单位）。建议在【服务】页启用 systemd 模式。';
  }).catch(()=>{});

  function postUpdate(restart, apply){
    const f = fileInput.files[0];
    if (!f){ msg('请先选择要上传的程序文件', false); return; }
    if (restart && !confirm('⚠️ 将替换当前程序并立即重启（fork 启动），现有连接会断开。确定继续？')) { return; }
    const fd = new FormData();
    fd.append('file', f);
    fd.append('restart', restart ? '1':'0');
    fd.append('apply', apply ? '1':'0');
    msg('上传中…', true);
    fetch('/api/system/update', {method:'POST', body:fd}).then(r=>r.json()).then(d=>{
      if (d.ok){ msg(d.ok, true); if (d.restarting) setTimeout(()=>location.href='/dashboard', 2500); }
      else msg(d.error || '上传失败', false);
    }).catch(e=>msg('网络错误: '+e, false));
  }

  restartCheck.addEventListener('change', function(){ confirmBox.classList.toggle('hidden', !this.checked); });
  confirmBox.classList.add('hidden');

  window.up1      = () => postUpdate(false, false);   // 仅暂存
  window.up2      = () => postUpdate(true, true);     // 上传并立即重启
  window.applied  = () => { if (confirm('应用暂存更新并重启？')) {
    fetch('/api/system/apply', {method:'POST'}).then(r=>r.json()).then(d=>{
      if (d.ok){ msg(d.ok, true); if (d.restarting) setTimeout(()=>location.href='/dashboard', 2500); }
      else msg(d.error || '应用失败', false);
    }).catch(e=>msg('网络错误: '+e, false));
  }};
  window.restartNowFn = () => { if (confirm('立即重启当前服务？现有连接会断开。')) restartNowFn(); };
})();
