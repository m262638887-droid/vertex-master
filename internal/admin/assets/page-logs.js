let logsRefreshTimer = null;

async function loadLogs() {
  try {
    const res = await fetch('/api/admin/log');
    const data = await res.json();
    if (res.ok && data.ok) {
      renderLogs(data.content || '');
    } else {
      toast('拉取日志失败', true);
    }
  } catch (e) {
    console.error(e);
  }
}

function renderLogs(content) {
  // Render UI
  const tbody = $('#logUITbody');
  if (tbody) {
    const lines = content.split('\n').filter(l => l.trim() !== '');
    let html = '';
    
    // Parse standard Go log format: "2026/06/29 01:21:21 [Level] Message"
    // Or our custom format like "[Config] ..."
    lines.forEach(line => {
      let level = 'info';
      let timeStr = '';
      let msg = line;
      
      // Basic heuristic parsing
      const timeMatch = line.match(/^(\d{4}\/\d{2}\/\d{2} \d{2}:\d{2}:\d{2})\s+(.*)/);
      if (timeMatch) {
        timeStr = timeMatch[1];
        msg = timeMatch[2];
      }
      
      if (msg.includes('[Config]') || msg.includes('[Server]')) level = 'info';
      if (msg.includes('警告') || msg.includes('warn') || msg.includes('WARN')) level = 'warn';
      if (msg.includes('失败') || msg.includes('error') || msg.includes('ERROR') || msg.includes('错误')) level = 'error';
      
      let levelClass = '';
      let levelText = 'INFO';
      if (level === 'info') { levelClass = 'log-level-info'; levelText = 'INFO'; }
      if (level === 'warn') { levelClass = 'log-level-warn'; levelText = 'WARN'; }
      if (level === 'error') { levelClass = 'log-level-error'; levelText = 'ERRO'; }
      
      html += `<tr>
        <td>${timeStr}</td>
        <td class="${levelClass}">${levelText}</td>
        <td>${escapeHtml(msg)}</td>
      </tr>`;
    });
    
    tbody.innerHTML = html;
    const uiEl = $('#logContentUI');
    if (uiEl) uiEl.scrollTop = uiEl.scrollHeight;
  }
}

function escapeHtml(str) {
  return str.replace(/[&<>"']/g, function(m) {
    switch (m) {
      case '&': return '&amp;';
      case '<': return '&lt;';
      case '>': return '&gt;';
      case '"': return '&quot;';
      case "'": return '&#39;';
    }
  });
}
