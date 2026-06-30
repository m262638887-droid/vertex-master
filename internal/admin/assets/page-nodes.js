document.getElementById('nodesBody').addEventListener('click', function(e) {
  var btn = e.target.closest('[data-action]');
  if (!btn) return;
  var uri = btn.dataset.uri;
  var action = btn.dataset.action;
  if (action === 'use-node') useNode(uri);
  else if (action === 'unuse-node') unuseNode(uri);
  else if (action === 'delete-node') delNode(uri);
  else if (action === 'test-node') testSingleNode(uri);
  else if (action === 'enable-node') enableNode(uri);
});

async function loadNodes() {
  try {
    const sd = await API.settings.get();
    if (typeof curSettings !== 'undefined') {
      curSettings = sd.settings || sd;
    }
    const gpEl = document.getElementById('globalProxy');
    if (gpEl && (sd.settings || sd).proxy_url !== undefined) {
      gpEl.value = (sd.settings || sd).proxy_url;
    }
  } catch (e) {}

  const d = await API.nodes.list();
  const nodes = d.nodes || [];

  const enabledCount = nodes.filter(n => !n.disabled).length;
  const disabledCount = nodes.filter(n => n.disabled).length;
  document.getElementById('nodesSummary').textContent = '\u5F53\u524D\u5171 ' + nodes.length + ' \u4E2A\u8282\u70B9\uFF08\u542F\u7528 ' + enabledCount + ' / \u7981\u7528 ' + disabledCount + '\uFF09';

  const tbody = document.getElementById('nodesBody');
  const frag = document.createDocumentFragment();

  if (nodes.length === 0) {
    var tr = document.createElement('tr');
    var td = document.createElement('td');
    td.colSpan = 5;
    td.style.cssText = 'color:var(--text-dim); text-align:center;';
    td.textContent = '\u6682\u65E0\u8282\u70B9';
    tr.appendChild(td);
    frag.appendChild(tr);
  } else {
    for (var i = 0; i < nodes.length; i++) {
      var n = nodes[i];
      var tr = document.createElement('tr');

      var cbTd = document.createElement('td');
      cbTd.style.cssText = 'text-align:center;vertical-align:middle;';
      var cb = document.createElement('input');
      cb.type = 'checkbox';
      cb.className = 'node-select-cb';
      cb.dataset.uri = n.raw_uri;
      cb.setAttribute('aria-label', '选择节点 ' + n.name);
      cbTd.appendChild(cb);
      tr.appendChild(cbTd);

      var nameTd = document.createElement('td');
      var nameDiv = document.createElement('div');
      nameDiv.style.cssText = 'font-weight:600;font-size:13.5px;color:var(--text);';
      nameDiv.textContent = n.name;
      var isLocked = n.raw_uri === curSettings.active_node_uri;
      if (isLocked) {
        var badge = document.createElement('span');
        badge.className = 'pill on';
        badge.style.cssText = 'font-size:10px;padding:2px 8px;margin-left:5px;';
        badge.textContent = '\u9501\u5B9A\u4F7F\u7528\u4E2D';
        nameDiv.appendChild(badge);
      } else if (n.disabled) {
        var badge = document.createElement('span');
        badge.className = 'pill off';
        badge.style.cssText = 'font-size:10px;padding:2px 8px;margin-left:5px;background:rgba(236,138,124,0.16);color:var(--red);';
        badge.textContent = '\u5DF2\u7981\u7528';
        nameDiv.appendChild(badge);
      } else {
        var badge = document.createElement('span');
        badge.className = 'pill off';
        badge.style.cssText = 'font-size:10px;padding:2px 8px;margin-left:5px;background:rgba(143,208,232,0.15);color:var(--blue);';
        badge.textContent = '\u5019\u9009';
        nameDiv.appendChild(badge);
      }
      nameTd.appendChild(nameDiv);

      var serverInfo = '';
      try {
        if (n.raw_uri.startsWith('vmess://')) {
          var b64Str = n.raw_uri.slice(8).split(/[?#]/)[0];
          var b = atob(b64Str.replace(/-/g, '+').replace(/_/g, '/'));
          var info = JSON.parse(b);
          serverInfo = (info.add || 'unknown') + ':' + (info.port || '');
        } else {
          var urlObj = new URL(n.raw_uri);
          serverInfo = urlObj.hostname + ':' + (urlObj.port || '443');
        }
      } catch(e) {
        serverInfo = '\u914D\u7F6E\u683C\u5F0F\u590D\u6742';
      }

      var addrDiv = document.createElement('div');
      addrDiv.style.cssText = 'font-size:11px;color:var(--text-dim);margin-top:4px;';
      addrDiv.appendChild(document.createTextNode('\u5730\u5740: '));
      var code = document.createElement('code');
      code.style.cssText = 'font-size:11px;background:rgba(0,0,0,0.25);color:var(--blue);padding:1px 4px;border-radius:4px;';
      code.textContent = serverInfo;
      addrDiv.appendChild(code);
      nameTd.appendChild(addrDiv);
      tr.appendChild(nameTd);

      var typeTd = document.createElement('td');
      var typeCode = document.createElement('code');
      typeCode.textContent = _tmap[n.type.toLowerCase()] || n.type.toUpperCase();
      typeTd.appendChild(typeCode);
      tr.appendChild(typeTd);

      var statusTd = document.createElement('td');
      var health = d.health[n.raw_uri];
      if (!health || (!health.last_success_at && !health.last_fail_at)) {
        var pill = document.createElement('span');
        pill.className = 'pill off';
        pill.style.cssText = 'background:rgba(195,182,164,0.15);color:var(--text-dim);';
        pill.textContent = '\u672A\u6D4B\u8BD5';
        statusTd.appendChild(pill);
      } else if (health.consecutive_failures === 0) {
        var ms = health.last_test_ms ? Math.round(health.last_test_ms) + 'ms' : '';
        var pill = document.createElement('span');
        pill.className = 'pill on';
        pill.style.cssText = 'background:rgba(132,214,160,0.16);color:var(--green);margin-right:5px;';
        pill.textContent = '\u6D4B\u8BD5\u901A\u8FC7 ' + ms;
        statusTd.appendChild(pill);
        var avail = document.createElement('span');
        avail.style.cssText = 'color:var(--green);font-weight:600;font-size:11px;';
        avail.textContent = '\u53EF\u7528';
        statusTd.appendChild(avail);
      } else {
        var pill = document.createElement('span');
        pill.className = 'pill off';
        pill.style.cssText = 'background:rgba(236,138,124,0.16);color:var(--red);margin-right:5px;';
        pill.textContent = '\u6D4B\u8BD5\u5931\u8D25';
        statusTd.appendChild(pill);
        
        if (health.last_test_error) {
          var errSpan = document.createElement('div');
          errSpan.className = 'node-err-msg';
          errSpan.textContent = health.last_test_error;
          statusTd.appendChild(errSpan);
        }
      }
      tr.appendChild(statusTd);

      var actionTd = document.createElement('td');
      actionTd.style.cssText = 'text-align:right;white-space:nowrap;';
      var testBtn = document.createElement('button');
      testBtn.className = 'btn ghost';
      testBtn.style.cssText = 'padding:4px 10px;font-size:12px;margin-right:4px;';
      testBtn.dataset.action = 'test-node';
      testBtn.dataset.uri = n.raw_uri;
      testBtn.textContent = '\u6D4B\u8BD5';
      actionTd.appendChild(testBtn);
      if (n.disabled) {
        var enableBtn = document.createElement('button');
        enableBtn.className = 'btn ghost';
        enableBtn.style.cssText = 'padding:4px 10px;font-size:12px;margin-right:4px;color:var(--green);';
        enableBtn.dataset.action = 'enable-node';
        enableBtn.dataset.uri = n.raw_uri;
        enableBtn.textContent = '\u542F\u7528';
        actionTd.appendChild(enableBtn);
      }
      if (isLocked) {
        var unuseBtn = document.createElement('button');
        unuseBtn.className = 'btn ghost';
        unuseBtn.style.cssText = 'padding:4px 10px;font-size:12px;margin-right:4px;color:var(--gold);';
        unuseBtn.dataset.action = 'unuse-node';
        unuseBtn.dataset.uri = n.raw_uri;
        unuseBtn.textContent = '取消锁定';
        actionTd.appendChild(unuseBtn);
      } else {
        var useBtn = document.createElement('button');
        useBtn.className = 'btn ghost';
        useBtn.style.cssText = 'padding:4px 10px;font-size:12px;margin-right:4px;';
        useBtn.dataset.action = 'use-node';
        useBtn.dataset.uri = n.raw_uri;
        useBtn.textContent = '\u9501\u5B9A\u4F7F\u7528';
        actionTd.appendChild(useBtn);
      }
      var delBtn = document.createElement('button');
      delBtn.className = 'btn danger';
      delBtn.style.cssText = 'padding:4px 10px;font-size:12px;';
      delBtn.dataset.action = 'delete-node';
      delBtn.dataset.uri = n.raw_uri;
      delBtn.textContent = '\u5220\u9664';
      actionTd.appendChild(delBtn);
      tr.appendChild(actionTd);
      frag.appendChild(tr);
    }
  }

  tbody.textContent = '';
  tbody.appendChild(frag);
  var mainCb = document.getElementById('selectAllNodesCheckbox');
  if (mainCb) mainCb.checked = false;
}

async function addAndFetchSub() {
  const u = $('#subUrl').value.trim();
  if (!u) return toast('请填订阅 URL');
  toast('正在拉取...');
  try {
    const res = await API.subscriptions.fetch(u);
    $('#subUrl').value = '';
    await loadNodes();
    toast('拉取成功，导入了 ' + (res.count || 0) + ' 个节点');
  } catch (e) {
    toast('拉取失败: ' + e.message);
  }
}

async function testAllNodes() {
  const d = await API.nodes.list();
  const nodes = d.nodes || [];
  if (!nodes.length) return toast('无可测试节点');

  const enabled = nodes.filter(function(n) { return !n.disabled; });
  const total = enabled.length;
  if (!total) return toast('没有已启用的节点可测试');

  const concurrency = Math.min(4, total);
  let next = 0, done = 0, ok = 0, failed = 0;

  const progressEl = document.getElementById('testProgress');
  const progressText = document.getElementById('testProgressText');
  const progressFill = document.getElementById('testProgressFill');
  const progressDetail = document.getElementById('testProgressDetail');

  progressEl.style.display = 'block';
  progressFill.style.width = '0%';
  progressText.textContent = '\u6D4B\u8BD5\u4E2D...';
  progressDetail.textContent = '';

  function formatRecentTest(node, ok, elapsed_ms, error) {
    if (ok) { return '\u6700\u8FD1\uFF1A' + node.name + ' \u00B7 \u6D4B\u8BD5\u901A\u8FC7 ' + Math.round(elapsed_ms) + 'ms'; }
    return '\u6700\u8FD1\uFF1A' + node.name + ' \u00B7 \u6D4B\u8BD5\u5931\u8D25' + (error ? ' ' + error : '');
  }
  async function worker() {
    while (next < total) {
      const node = enabled[next++];
      progressDetail.textContent = '正在测试: ' + node.name;
      let result;
      try {
        result = await API.nodes.test(node.raw_uri, { auto_disable: true });
      } catch (e) {
        result = { ok: false, elapsed_ms: 0, error: e.message };
      }
      done++;
      if (result.ok) ok++; else failed++;
      progressFill.style.width = Math.round(done / total * 100) + '%';
      progressText.textContent = '\u5DF2\u5B8C\u6210 ' + done + '/' + total + ' \u00B7 \u901A\u8FC7 ' + ok + ' \u00B7 \u5931\u8D25 ' + failed;
      progressDetail.textContent = formatRecentTest(node, result.ok, result.elapsed_ms, result.error);
    }
  }

  await Promise.all(Array.from({ length: concurrency }, worker));
  await loadNodes();
  toast('\u6279\u91CF\u6D4B\u8BD5\u5B8C\u6210\uFF1A\u901A\u8FC7 ' + ok + ' / \u5931\u8D25 ' + failed);
  progressEl.style.display = 'none';
}

async function dedupNodes() { await API.nodes.dedup(); loadNodes(); toast('去重完成'); }
async function deleteDisabledNodes() { await API.nodes.deleteDisabled(); loadNodes(); toast('清理完成'); }
async function sortNodesByLatency() { await API.nodes.sort(); await loadNodes(); toast('已按延迟重排节点'); }

async function testSingleNode(uri) {
  toast('正在测试节点...');
  try {
    const result = await API.nodes.test(uri, { auto_disable: true });
    const msg = result.ok
      ? '测试通过 ' + Math.round(result.elapsed_ms) + 'ms'
      : '测试失败 ' + (result.error || '');
    toast(msg);
    await loadNodes();
  } catch (e) {
    toast('测试出错: ' + e.message);
  }
}

async function enableNode(uri) {
  await API.nodes.enable(uri);
  await loadNodes();
  toast('已启用该节点');
}

async function useNode(uri) { await API.useNode(uri); loadSettings(); loadNodes(); toast('已锁定使用该节点，并关闭并发池'); }
async function unuseNode(uri) { await API.useNode(''); loadSettings(); loadNodes(); toast('已取消锁定，并恢复并发池'); }
async function delNode(uri) { if(!confirm('删除该节点？')) return; await API.nodes.delete(uri); loadNodes(); toast('已删除'); }

function getSelectedNodeURIs() {
  const cbs = document.querySelectorAll('.node-select-cb:checked');
  const uris = [];
  cbs.forEach(cb => uris.push(cb.getAttribute('data-uri')));
  return uris;
}

function toggleSelectAllNodes() {
  const cbs = document.querySelectorAll('.node-select-cb');
  let hasUnchecked = false;
  cbs.forEach(cb => { if (!cb.checked) hasUnchecked = true; });
  cbs.forEach(cb => { cb.checked = hasUnchecked; });
  const mainCb = document.getElementById('selectAllNodesCheckbox');
  if (mainCb) mainCb.checked = hasUnchecked;
}

function toggleSelectAllNodesCheckbox(mainCb) {
  const cbs = document.querySelectorAll('.node-select-cb');
  cbs.forEach(cb => { cb.checked = mainCb.checked; });
}

async function batchEnableSelectedNodes() {
  const uris = getSelectedNodeURIs();
  if (!uris.length) return toast('请先勾选需要批量操作的节点');
  toast('批量启用中...');
  try { await API.nodes.batchEnable(uris); await loadNodes(); toast('已成功启用 ' + uris.length + ' 个节点'); }
  catch(e) { toast('操作失败: ' + e.message); }
}

async function batchDisableSelectedNodes() {
  const uris = getSelectedNodeURIs();
  if (!uris.length) return toast('请先勾选需要批量操作的节点');
  toast('批量禁用中...');
  try { await API.nodes.batchDisable(uris); await loadNodes(); toast('已成功禁用 ' + uris.length + ' 个节点'); }
  catch(e) { toast('操作失败: ' + e.message); }
}

async function batchDeleteSelectedNodes() {
  const uris = getSelectedNodeURIs();
  if (!uris.length) return toast('请先勾选需要批量操作的节点');
  if (!confirm('确定要批量删除这 ' + uris.length + ' 个节点吗？')) return;
  toast('批量删除中...');
  try { await API.nodes.batchDelete(uris); await loadNodes(); toast('已成功删除 ' + uris.length + ' 个节点'); }
  catch(e) { toast('操作失败: ' + e.message); }
}

function importFileNodes(replace) {
  const fileInput = document.getElementById('nodeImportFile');
  if (!fileInput.files.length) return toast('请先选择一个节点配置文件');
  const file = fileInput.files[0];
  const reader = new FileReader();
  toast('正在读取配置文件并解析...');
  reader.onload = async function(e) {
    const text = e.target.result;
    try {
      const res = await API.nodes.import(text, replace);
      await loadNodes();
      fileInput.value = '';
      toast(replace ? '替换成功，导入了 ' + res.count + ' 个节点' : '导入成功，追加了 ' + res.count + ' 个节点');
    } catch (err) {
      toast('文件导入解析失败: ' + err.message);
    }
  };
  reader.readAsText(file);
}

function importJsonNodes(replace) {
  const fileInput = document.getElementById('nodeJsonImportFile');
  if (!fileInput.files.length) return toast('请先选择一个 nodes.json 配置文件');
  const file = fileInput.files[0];
  const reader = new FileReader();
  toast('正在读取配置文件并解析...');
  reader.onload = async function(e) {
    const text = e.target.result;
    try {
      const res = await API.nodes.importJson(text, replace);
      await loadNodes();
      fileInput.value = '';
      toast(replace ? '替换成功，导入了 ' + res.count + ' 个节点' : '导入成功，追加了 ' + res.count + ' 个节点');
    } catch (err) {
      toast('nodes.json 导入解析失败: ' + err.message);
    }
  };
  reader.readAsText(file);
}

async function saveGlobalProxy() { await API.settings.put({ proxy_url: $('#globalProxy').value }); loadSettings(); toast('全局代理已保存'); }