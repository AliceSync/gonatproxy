// Reusable styled dialogs (confirm / prompt / alert) replacing native
// window.alert/confirm/prompt. API (each returns a Promise):
//   DSH.confirm(message, {title, okText, cancelText, danger})
//   DSH.prompt  (message, defaultValue, {title, okText, cancelText, placeholder})
//   DSH.alert   (message, {title, okText})
// A single shared overlay keeps dialogs from stacking unpredictably.
(function () {
  if (window.DSH) return;
  window.DSH = {};

  const overlay = document.createElement('div');
  overlay.className = 'dlg-bg hidden';
  overlay.innerHTML =
    '<div class="dlg">' +
      '<div class="dlg-head"><strong class="dlg-title"></strong><button type="button" class="btn btn-ghost btn-sm dlg-x" aria-label="关闭">✕</button></div>' +
      '<div class="dlg-body dlg-msg"></div>' +
      '<div class="dlg-extra hidden"><input type="text" class="dlg-input" autocomplete="off"></div>' +
      '<div class="dlg-foot"></div>' +
    '</div>';
  document.body.appendChild(overlay);

  let current = null; // {resolve, isPrompt}

  function cancel() { if (current) current.resolve(current.isPrompt ? null : false); close(); }

  function finish(v) { if (current) current.resolve(v); close(); }

  function close() {
    current = null;
    overlay.classList.add('hidden');
  }

  function open(o) {
    overlay.querySelector('.dlg-title').textContent = o.title || '提示';
    overlay.querySelector('.dlg-msg').textContent = o.message; // safe text, not HTML
    const extra = overlay.querySelector('.dlg-extra');
    const input = overlay.querySelector('.dlg-input');
    const isPrompt = o.input != null;
    if (isPrompt) {
      extra.classList.remove('hidden');
      input.value = o.input;
      input.placeholder = o.placeholder || '';
    } else {
      extra.classList.add('hidden');
      input.value = '';
    }
    input.onkeydown = null;
    const foot = overlay.querySelector('.dlg-foot');
    foot.innerHTML = '';
    const addBtn = (label, cls, fn) => {
      const b = document.createElement('button');
      b.className = 'btn ' + cls; b.textContent = label;
      b.addEventListener('click', fn);
      foot.appendChild(b);
      return b;
    };
    if (o.showCancel !== false) addBtn(o.cancelText || '取消', 'btn btn-ghost', cancel);
    // For confirm/alert the OK yields `true`; for prompt it yields the input value.
    const ok = addBtn(o.okText || '确定', (o.danger ? 'btn btn-danger' : 'btn btn-primary'), () => finish(isPrompt ? input.value : true));
    overlay.classList.remove('hidden'); // << show the dialog
    ok.focus();
    if (isPrompt) input.focus();
    current = { resolve: null, isPrompt: isPrompt };
    return new Promise(resolve => { current.resolve = resolve; });
  }

  document.addEventListener('keydown', function (e) {
    if (!current) return;
    if (e.key === 'Escape') { cancel(); }
    else if (e.key === 'Enter') {
      const t = e.target;
      if (t && (t.tagName === 'BUTTON' || t.tagName === 'INPUT')) {
        const ok = overlay.querySelector('.dlg-foot .btn-primary, .dlg-foot .btn-danger');
        if (ok && ok !== t) { ok.click(); e.preventDefault(); }
      }
    }
  });
  overlay.querySelector('.dlg-x').addEventListener('click', cancel);

  DSH.confirm = (message, o) => { o = o || {}; return open(Object.assign({}, o, { message: message })); };
  DSH.prompt  = (message, def, o) => { o = o || {}; return open(Object.assign({}, o, { message: message, input: def == null ? '' : String(def) })); };
  DSH.alert   = (message, o) => { o = o || {}; return open(Object.assign({}, o, { message: message, showCancel: false })); };
})();
