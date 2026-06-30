const SETTINGS_FIELDS = [
  // 🚀 Group: pool (并发与 Token 池管理)
  { k: 'parallel_pool_enabled', label: '并发请求池', type: 'bool', group: 'pool', desc: '同时请求多个健康节点，首包到达即采纳，降低延迟' },
  { k: 'parallel_pool_retry_enabled', label: '并发池单点重试', type: 'bool', group: 'pool', desc: '开启后并发池内即使节点撞库也依然强制等待并重试（不推荐，仅限节点极少时开启）' },
  { k: 'sticky_pool_enabled', label: '粘性节点池', type: 'bool', group: 'pool', desc: '竞速成功后缓存可靠节点，优先复用减少配额浪费。需启用并发请求池' },
  { k: 'parallel_pool_size', label: '并发数', type: 'number', max: 20, min: 1, group: 'pool', desc: '并发抢跑的节点数 (默认 15，最大 20)' },
  { k: 'parallel_pool_delay_dynamic', label: '动态对冲延迟', type: 'bool', group: 'pool', desc: '根据节点平均响应时间动态调整并发启动间隔，平衡延迟与流量消耗' },
  { k: 'parallel_pool_delay_ms', label: '固定对冲延迟时间 (毫秒)', type: 'number', group: 'pool', desc: '当禁用动态延迟时，以此固定间隔对冲触发后续备份通道 (默认 500ms)' },

  // 🛠 Group: core (核心控制与基础参数)
  { k: 'max_retries', label: '上游重试次数', type: 'number', group: 'core', desc: '上游请求失败时的重试次数；总尝试 = 此值 + 1' },
  { k: 'max_n', label: '最大候选数 (max_n)', type: 'number', group: 'core', desc: '限制客户端一次生成回答的条数上限，防滥用刷量 (默认 8)' },
  { k: 'max_spill_mb', label: '最大内存缓冲 (MB)', type: 'number', group: 'core', desc: '上传大文件时，超过此大小将写入磁盘，防爆内存 (默认 2048)' },
  { k: 'force_no_stream', label: '强制非流式', type: 'bool', group: 'core', desc: '把所有流式请求降级为非流式' },
  { k: 'debug_mode', label: 'Debug 日志', type: 'bool', group: 'core', desc: '开启更详细的错误与负载调试日志' },

  // 🛡 Group: security (安全增强与模型策略)
  { k: 'anti429_enabled', label: 'anti429 随机数注入', type: 'bool', group: 'security', desc: '往请求注入随机数字串，削弱上游 429 / 缓存限制' },
  { k: 'anti429_target', label: 'anti429 注入位置', type: 'select', opts: ['system', 'user'], group: 'security', desc: '随机数注入到 system 指令、还是首条 user 消息前' },
  { k: 'anti_tracking', label: '反追踪', type: 'bool', group: 'security', desc: '移除可能被用于追踪的请求特征' },
  { k: 'drop_max_tokens', label: '移除 maxOutputTokens', type: 'bool', group: 'security', desc: '移除输出 token 上限，让模型自由输出' },
];

let curSettings = {};
async function loadSettings() {
  const d = await API.settings.get(); curSettings = d.settings || d;
  
  const gpEl = $('#globalProxy');
  if (gpEl && curSettings.proxy_url !== undefined) {
    gpEl.value = curSettings.proxy_url;
  }
  
  const fld = (f) => {
    const v = curSettings[f.k];
    if (f.type === 'bool') return `<div class="field bool"><div class="min-w-0"><label for="set_${f.k}">${f.label}</label>${f.desc?`<div class="desc mt-4px">${f.desc}</div>`:''}</div><label class="toggle"><input type="checkbox" id="set_${f.k}" ${v?'checked':''}><span class="track"></span></label></div>`;
    let input;
    if (f.type === 'select') input = `<select id="set_${f.k}">${f.opts.map(o => `<option ${o===v?'selected':''}>${o}</option>`).join('')}</select>`;
    else input = `<input type="${f.type}" id="set_${f.k}" value="${v ?? ''}" ${f.max !== undefined ? `max="${f.max}" oninput="if(this.value!=='' && parseInt(this.value)>${f.max}) this.value='${f.max}'"` : ''} ${f.min !== undefined ? `min="${f.min}"` : ''}>`;
    return `<div class="field"><label for="set_${f.k}">${f.label}</label>${input}${f.desc?`<div class="desc">${f.desc}</div>`:''}</div>`;
  };

  // 【核心修改：定义视觉功能分组】
  const groups = {
    pool: { title: '🚀 并发与 Token 池管理', fields: [] },
    core: { title: '🛠 核心控制与基础参数', fields: [] },
    security: { title: '🛡 安全增强与模型策略', fields: [] }
  };

  SETTINGS_FIELDS.forEach(f => {
    if (groups[f.group]) {
      groups[f.group].fields.push(f);
    }
  });

  let sectionsHtml = '';
  for (const [key, g] of Object.entries(groups)) {
    const numFields = g.fields.filter(f => f.type !== 'bool');
    const boolFields = g.fields.filter(f => f.type === 'bool');

    sectionsHtml += `
      <div class="settings-section-title">${g.title}</div>
      ${numFields.length ? `<div class="grid grid-2">${numFields.map(fld).join('')}</div>` : ''}
      ${boolFields.length ? `<div class="grid grid-2" style="margin-top:10px;">${boolFields.map(fld).join('')}</div>` : ''}
    `;
  }

  $('#settingsForm').innerHTML =
    sectionsHtml +
    '<button class="btn mt-14px" onclick="saveSettings()">保存设置</button>';

  const stickyEl = $('#set_sticky_pool_enabled');
  const parallelRetryEl = $('#set_parallel_pool_retry_enabled');
  const parallelEl = $('#set_parallel_pool_enabled');
  if (stickyEl && parallelEl && parallelRetryEl) {
    const updateStickyDisabled = () => {
      const disabled = !parallelEl.checked;
      
      stickyEl.disabled = disabled;
      if (disabled) stickyEl.checked = false;
      const container = stickyEl.closest('.field');
      if (container) {
        container.style.opacity = disabled ? '0.5' : '1';
        container.style.pointerEvents = disabled ? 'none' : '';
        const desc = container.querySelector('.desc');
        if (desc) {
          desc.textContent = disabled ? '需先启用并发请求池' : '竞速成功后缓存可靠节点，优先复用减少配额浪费。需启用并发请求池';
        }
      }
      
      parallelRetryEl.disabled = disabled;
      if (disabled) parallelRetryEl.checked = false;
      const retryContainer = parallelRetryEl.closest('.field');
      if (retryContainer) {
        retryContainer.style.opacity = disabled ? '0.5' : '1';
        retryContainer.style.pointerEvents = disabled ? 'none' : '';
        const retryDesc = retryContainer.querySelector('.desc');
        if (retryDesc) {
          retryDesc.textContent = disabled ? '需先启用并发请求池' : '开启后并发池内即使节点撞库也依然强制等待并重试（不推荐，仅限节点极少时开启）';
        }
      }
    };
    updateStickyDisabled();
    parallelEl.addEventListener('change', updateStickyDisabled);
  }

  PAGE_CACHE['settings'] = $('#page-settings').innerHTML;
}

async function saveSettings() {
  const out = {};
  for (const f of SETTINGS_FIELDS) { 
    const el = $('#set_' + f.k); 
    if (!el) continue; 
    if (f.type === 'bool') out[f.k] = el.checked; 
    else if (f.type === 'number') out[f.k] = parseInt(el.value || '0', 10); 
    else out[f.k] = el.value; 
  }
  // Keep sending whatever telemetry_enabled is in curSettings to prevent config loss/errors
  if (curSettings.telemetry_enabled !== undefined) {
    out['telemetry_enabled'] = curSettings.telemetry_enabled;
  }
  if (!out['parallel_pool_enabled']) {
    out['sticky_pool_enabled'] = false;
    out['parallel_pool_retry_enabled'] = false;
  }
  await API.settings.put(out); toast('设置已保存');
}