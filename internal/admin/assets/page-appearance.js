let currentAppearanceSettings = {};

function applyBg(v) { document.documentElement.style.setProperty('--bg-img', v); applyThemeColorFromBg(v); }

async function initBg() {
  try {
    const res = await API.checkAuth();
    if (res) currentAppearanceSettings = res;
    
    // Apply font size
    if (res?.font_size) {
      document.documentElement.style.setProperty('--base-font-size', res.font_size);
      const fsEl = document.getElementById('currentFontSize');
      if (fsEl) fsEl.textContent = res.font_size;
    }
    if (res?.font_color_type) {
      const bAdapt = document.getElementById('btnColorAdaptive');
      const bSpec = document.getElementById('btnColorSpecified');
      if (bAdapt) bAdapt.classList.toggle('active', res.font_color_type === 'adaptive');
      if (bSpec) bSpec.classList.toggle('active', res.font_color_type === 'specified');
    }
    if (res?.font_color) {
      const fcBox = document.getElementById('fontColorBox');
      if (fcBox) fcBox.style.backgroundColor = res.font_color;
    }
    if (res?.background_image && !res.background_image.includes('url') && !res.background_image.includes('gradient')) {
      const bgBox = document.getElementById('bgColorBox');
      if (bgBox) bgBox.style.backgroundColor = res.background_image;
    }
    
    const bg = res?.background_image;
    if (bg) {
      applyBg(bg);
      return;
    }
  } catch (e) {}
  const s = localStorage.getItem('vproxy_bg');
  if (s) applyBg(s);
}
initBg();

let _saveTimeout = null;
async function saveAppearanceSettings(update) {
  Object.assign(currentAppearanceSettings, update);
  if (_saveTimeout) clearTimeout(_saveTimeout);
  _saveTimeout = setTimeout(async () => {
    try {
      await API.settings.put(currentAppearanceSettings);
    } catch (e) {
      console.error('Failed to save settings:', e);
    }
  }, 500);
}

function adjustFontSize(delta) {
  let size = parseInt(currentAppearanceSettings.font_size || '14');
  size += delta;
  if (size < 12) size = 12;
  if (size > 24) size = 24;
  const newSize = size + 'px';
  currentAppearanceSettings.font_size = newSize;
  const fsEl = $('#currentFontSize');
  if (fsEl) fsEl.textContent = newSize;
  document.documentElement.style.setProperty('--base-font-size', newSize);
  saveAppearanceSettings({ font_size: newSize });
}

function setFontColorType(type) {
  currentAppearanceSettings.font_color_type = type;
  if (type === 'adaptive') {
    if (currentAppearanceSettings.background_image) {
      applyThemeColorFromBg(currentAppearanceSettings.background_image);
    } else {
      const curBg = document.documentElement.style.getPropertyValue('--bg-img').trim();
      if (curBg) applyThemeColorFromBg(curBg);
    }
  } else {
    if (currentAppearanceSettings.font_color) {
      document.documentElement.style.setProperty('--text-custom', currentAppearanceSettings.font_color);
      document.documentElement.style.setProperty('--text-dim-custom', currentAppearanceSettings.font_color + 'b3');
    }
  }
  saveAppearanceSettings({ font_color_type: type });
}

function setFontColor(color) {
  currentAppearanceSettings.font_color = color;
  currentAppearanceSettings.font_color_type = 'specified';
  document.documentElement.style.setProperty('--text-custom', color);
  document.documentElement.style.setProperty('--text-dim-custom', color + 'b3');
  saveAppearanceSettings({ font_color: color, font_color_type: 'specified' });
}

async function setBgAndSync(v) {
  applyBg(v);
  localStorage.setItem('vproxy_bg', v); // Fallback
  try {
    await API.settings.put({ background_image: v });
    toast('背景已更换');
    loadAppearance(); // Sync current presets
  } catch (e) {
    toast('同步背景失败', true);
  }
}

function applyBgUrl() { 
  const u = $('#bgUrl').value.trim(); 
  if (!u) return; 
  setBgAndSync(`url('${u}')`); 
}

async function uploadBg(e) { 
  const f = e.target.files[0]; 
  if (!f) return;
  if (f.size > 10 * 1024 * 1024) {
    toast('文件不能超过10MB', true);
    return;
  }
  const fd = new FormData();
  fd.append('file', f);
  try {
    const res = await fetch('/api/admin/upload-bg', { method: 'POST', body: fd });
    const data = await res.json();
    if (res.ok && data.ok) {
      setBgAndSync(data.url);
      loadAppearance();
    } else {
      toast(data.error?.message || '上传失败', true);
    }
  } catch (err) {
    toast('上传失败', true);
  }
}

function resetBg() { 
  localStorage.removeItem('vproxy_bg'); 
  applyBg(DEFAULT_BG);
  API.settings.put({ background_image: DEFAULT_BG }).catch(()=>{});
  toast('已恢复默认'); 
}

async function loadAppearance() {
  try {
    const data = await API.settings.get();
    if (data) {
      currentAppearanceSettings = data.settings || data;
    }
  } catch (e) {}

  const presetsImg = [
    { name: '默认', val: "url('background.jpg')" }
  ];
  let presetsColor = [
    { name: '克莱因蓝', val: '#002fa7' },
    { name: '暗夜紫', val: '#2e1065' },
    { name: '酒红', val: '#4c0519' },
    { name: '深海绿', val: '#064e3b' },
    { name: '极暗蓝', val: '#0f172a' },
    { name: '琥珀黄', val: '#78350f' },
    { name: '苍穹蓝', val: '#1e3a8a' },
    { name: '焦糖', val: '#7c2d12' },
    { name: '松石绿', val: '#0f766e' },
    { name: '莫兰迪粉', val: '#831843' },
    { name: '纯白', val: '#ffffff' },
    { name: '纯黑', val: '#000000' },
    { name: '银灰', val: '#f3f4f6' },
    { name: '暗灰', val: '#3f3f46' },
  ];

  const curBg = document.documentElement.style.getPropertyValue('--bg-img').trim() || currentAppearanceSettings.background_image || '';
  
  const renderThumbs = (presets, containerId, canDelete = false) => {
    const el = $('#' + containerId);
    if (!el) return;
    el.innerHTML = presets.map(p => {
      const isImg = p.val.startsWith('url');
      const style = isImg ? `background-image:${p.val}` : `background:${p.val}`;
      const activeClass = (p.val === curBg) ? ' active' : '';
      const delBtn = canDelete ? `<div class="del-btn" onclick="deletePreset(&quot;${p.val}&quot;, event)" title="删除预设">×</div>` : '';
      return `<div class="thumb${activeClass}" style="${style}" onclick="setBgAndSync(&quot;${p.val}&quot;)" title="${p.name}">${delBtn}</div>`;
    }).join('');
  };

  if (currentAppearanceSettings.custom_bg_presets && Array.isArray(currentAppearanceSettings.custom_bg_presets)) {
    const customPresets = currentAppearanceSettings.custom_bg_presets.map((val, i) => ({ name: '自定义' + (i + 1), val }));
    renderThumbs(customPresets, 'presetsCustom', true);
    const customRow = $('#customPresetsRow');
    if (customRow) customRow.style.display = customPresets.length > 0 ? 'flex' : 'none';
  } else {
    const customRow = $('#customPresetsRow');
    if (customRow) customRow.style.display = 'none';
  }
  
  try {
    const res = await fetch('/api/admin/list-bgs');
    const data = await res.json();
    if (res.ok && data.ok && data.files) {
      data.files.forEach((f, i) => {
        presetsImg.push({ name: `自定义${i+1}`, val: `url('/assets/${f}')`, canDel: true });
      });
    }
  } catch (e) {}

  if (curBg && !presetsImg.find(p => p.val === curBg) && !presetsColor.find(p => p.val === curBg)) {
    presetsImg.unshift({ name: '当前', val: curBg });
  }
  
  const elImg = $('#presets');
  if (elImg) {
    elImg.innerHTML = presetsImg.map(p => {
      const isImg = p.val.startsWith('url');
      const style = isImg ? `background-image:${p.val}` : `background:${p.val}`;
      const activeClass = (p.val === curBg) ? ' active' : '';
      const delBtn = p.canDel ? `<div class="del-btn" onclick="deletePreset(&quot;${p.val}&quot;, event)" title="删除图片">×</div>` : '';
      return `<div class="thumb${activeClass}" style="${style}" onclick="setBgAndSync(&quot;${p.val}&quot;)" title="${p.name}">${delBtn}</div>`;
    }).join('');
  }
  renderThumbs(presetsColor, 'presetsColor');

  // 初始化字体大小显示
  const fsEl = $('#currentFontSize');
  if (fsEl) fsEl.textContent = currentAppearanceSettings.font_size || '14px';
  
  // 初始化字体颜色模式 UI
  if (currentAppearanceSettings.font_color_type === 'specified' && currentAppearanceSettings.font_color) {
    const box = $('#fontColorBox');
    if (box) box.style.backgroundColor = currentAppearanceSettings.font_color;
  }
  
  renderGradientStops(false);
  setActiveColorTarget('font');
  
  PAGE_CACHE['appearance'] = $('#page-appearance').innerHTML;
}

function applyThemeColorFromBg(bgValue) {
  const match = bgValue.match(/url\(['"]?(.*?)['"]?\)/);
  const src = match ? match[1] : null;
  const canvas = document.createElement("canvas");
  const ctx = canvas.getContext("2d", { willReadFrequently: true });
  canvas.width = 64; canvas.height = 64;
  if (src) {
    const img = new Image();
    if (src.startsWith('http') && !src.startsWith(location.origin)) {
      img.crossOrigin = "Anonymous";
    }
    img.onload = () => { ctx.drawImage(img, 0, 0, 64, 64); extractAndSetColors(ctx); };
    img.src = src;
  } else if (bgValue.includes('gradient')) {
    // extract hex/rgb from gradient string to average them
    const matches = bgValue.match(/#[0-9a-fA-F]{3,6}/g);
    let avgR = 0, avgG = 0, avgB = 0, count = 0;
    if (matches) {
      matches.forEach(m => {
        if (m.length === 4) m = '#' + m[1]+m[1]+m[2]+m[2]+m[3]+m[3];
        avgR += parseInt(m.slice(1,3), 16);
        avgG += parseInt(m.slice(3,5), 16);
        avgB += parseInt(m.slice(5,7), 16);
        count++;
      });
      if (count > 0) {
        ctx.fillStyle = `rgb(${Math.round(avgR/count)}, ${Math.round(avgG/count)}, ${Math.round(avgB/count)})`;
      }
    }
    ctx.fillRect(0, 0, 64, 64);
    extractAndSetColors(ctx);
  } else {
    ctx.fillStyle = bgValue;
    ctx.fillRect(0, 0, 64, 64);
    extractAndSetColors(ctx);
  }
}

function extractAndSetColors(ctx) {
  const data = ctx.getImageData(0, 0, 64, 64).data;
  let sumR = 0, sumG = 0, sumB = 0, totalWeight = 0;
  
  for (let i = 0; i < data.length; i += 16) {
    let r = data[i], g = data[i+1], b = data[i+2];
    let [h, s, l] = rgbToHsl(r, g, b);
    let weight = s * (1 - Math.abs(2 * l - 1));
    weight = Math.max(weight, 0.02);
    sumR += r * weight; sumG += g * weight; sumB += b * weight;
    totalWeight += weight;
  }
  
  let r = 0, g = 0, b = 0;
  if (totalWeight > 0) {
    r = sumR / totalWeight; g = sumG / totalWeight; b = sumB / totalWeight;
  }
  
  let [h, s, l] = rgbToHsl(r, g, b);
  if (s < 0.25) s = 0.5; 
  if (l < 0.4) l = 0.6; 
  if (l > 0.7) l = 0.55; 
  
  const c1 = hslToHex(h, s, l);
  const c2 = hslToHex(h, s, Math.max(l - 0.18, 0.25));
  
  const toRgbString = (hex) => {
    let r = parseInt(hex.slice(1,3), 16);
    let g = parseInt(hex.slice(3,5), 16);
    let b = parseInt(hex.slice(5,7), 16);
    return `${r}, ${g}, ${b}`;
  };
  
  const rgb1 = toRgbString(c1);
  const rgb2 = toRgbString(c2);
  
  document.documentElement.style.setProperty("--gold", c1);
  document.documentElement.style.setProperty("--gold-rgb", rgb1);
  document.documentElement.style.setProperty("--gold-deep", c2);
  document.documentElement.style.setProperty("--gold-soft", `rgba(${rgb1}, 0.15)`);
  
  if (currentAppearanceSettings.font_color_type !== 'specified') {
    const luminance = 0.299 * r + 0.587 * g + 0.114 * b;
    if (luminance > 140) {
      document.documentElement.style.setProperty('--text-custom', '#2c1d08');
      document.documentElement.style.setProperty('--text-dim-custom', 'rgba(44, 29, 8, 0.7)');
    } else {
      document.documentElement.style.setProperty('--text-custom', '#f6f1e9');
      document.documentElement.style.setProperty('--text-dim-custom', 'rgba(246, 241, 233, 0.7)');
    }
  } else if (currentAppearanceSettings.font_color) {
    document.documentElement.style.setProperty('--text-custom', currentAppearanceSettings.font_color);
    document.documentElement.style.setProperty('--text-dim-custom', currentAppearanceSettings.font_color + 'b3');
  }

  document.documentElement.style.setProperty("--gold-shadow1", `rgba(${rgb1}, 0.3)`);
  document.documentElement.style.setProperty("--gold-shadow2", `rgba(${rgb1}, 0.42)`);
  
  // Create a complementary blueish color for the second veil based on the dominant hue
  let blueH = (h + 0.5) % 1;
  const cBlue = hslToHex(blueH, s, l);
  const rgbBlue = toRgbString(cBlue);
  document.documentElement.style.setProperty("--blue-soft", `rgba(${rgbBlue}, 0.1)`);
  
  // Background panels adaptive colors (dark theme)
  const glassHex = hslToHex(h, Math.min(s, 0.2), 0.11);
  const veilHex = hslToHex(h, Math.min(s, 0.25), 0.06);
  const veilDarkHex = hslToHex(h, Math.min(s, 0.25), 0.04);
  const strokeHex = hslToHex(h, Math.min(s, 0.4), 0.95);
  
  const glassRgb = toRgbString(glassHex);
  const veilRgb = toRgbString(veilHex);
  const veilDarkRgb = toRgbString(veilDarkHex);
  const strokeRgb = toRgbString(strokeHex);
  
  document.documentElement.style.setProperty("--glass", `rgba(${glassRgb}, 0.38)`);
  document.documentElement.style.setProperty("--glass-solid", `rgba(${glassRgb}, 0.68)`);
  document.documentElement.style.setProperty("--veil-light", `rgba(${veilRgb}, 0.42)`);
  document.documentElement.style.setProperty("--veil-dark", `rgba(${veilDarkRgb}, 0.62)`);
  document.documentElement.style.setProperty("--stroke", `rgba(${strokeRgb}, 0.14)`);
}

function rgbToHsl(r, g, b) {
  r /= 255; g /= 255; b /= 255;
  const max = Math.max(r, g, b), min = Math.min(r, g, b);
  let h, s, l = (max + min) / 2;
  if (max === min) { h = s = 0; }
  else {
    const d = max - min;
    s = l > 0.5 ? d / (2 - max - min) : d / (max + min);
    switch (max) {
      case r: h = (g - b) / d + (g < b ? 6 : 0); break;
      case g: h = (b - r) / d + 2; break;
      case b: h = (r - g) / d + 4; break;
    }
    h /= 6;
  }
  return [h, s, l];
}

function hslToHex(h, s, l) {
  let r, g, b;
  if (s === 0) { r = g = b = l; }
  else {
    const hue2rgb = (p, q, t) => {
      if(t < 0) t += 1; if(t > 1) t -= 1;
      if(t < 1/6) return p + (q - p) * 6 * t;
      if(t < 1/2) return q;
      if(t < 2/3) return p + (q - p) * (2/3 - t) * 6;
      return p;
    };
    const q = l < 0.5 ? l * (1 + s) : l + s - l * s;
    const p = 2 * l - q;
    r = hue2rgb(p, q, h + 1/3); g = hue2rgb(p, q, h); b = hue2rgb(p, q, h - 1/3);
  }
  const toHex = x => {
    const hex = Math.round(x * 255).toString(16);
    return hex.length === 1 ? "0" + hex : hex;
  };
  return `#${toHex(r)}${toHex(g)}${toHex(b)}`;
}



// ==========================================
// Advanced Color Palette Logic
// ==========================================

let activeColorTarget = null; // 'font', 'gradient-0', etc.
let activeColorIndex = -1; // for gradients
let currentPaletteHex = '#002fa7';

function hslToRgb(h, s, l) {
  let r, g, b;
  if(s === 0) {
    r = g = b = l; // achromatic
  } else {
    const hue2rgb = (p, q, t) => {
      if(t < 0) t += 1;
      if(t > 1) t -= 1;
      if(t < 1/6) return p + (q - p) * 6 * t;
      if(t < 1/2) return q;
      if(t < 2/3) return p + (q - p) * (2/3 - t) * 6;
      return p;
    };
    const q = l < 0.5 ? l * (1 + s) : l + s - l * s;
    const p = 2 * l - q;
    r = hue2rgb(p, q, h + 1/3);
    g = hue2rgb(p, q, h);
    b = hue2rgb(p, q, h - 1/3);
  }
  return [Math.round(r * 255), Math.round(g * 255), Math.round(b * 255)];
}

function hexToRgb(hex) {
  hex = hex.replace(/^#/, '');
  if (hex.length === 3) hex = hex.split('').map(c => c+c).join('');
  const num = parseInt(hex, 16);
  if (isNaN(num)) return [0,0,0];
  return [num >> 16, (num >> 8) & 255, num & 255];
}

function rgbToHexStr(r, g, b) {
  return '#' + [r, g, b].map(x => {
    const hex = x.toString(16);
    return hex.length === 1 ? '0' + hex : hex;
  }).join('').toUpperCase();
}

function switchPaletteMode(mode) {
  ['RGB', 'HSL', 'HEX'].forEach(m => {
    const tab = $('#tab-' + m);
    const panel = $('#palette-' + m);
    if (tab && panel) {
      if (m === mode) {
        tab.classList.add('active');
        panel.classList.remove('hidden');
      } else {
        tab.classList.remove('active');
        panel.classList.add('hidden');
      }
    }
  });
}

function setActiveColorTarget(target, index = -1, hexColor = null, event = null) {
  if (activeColorTarget === 'bg' && target !== 'bg' && currentPaletteHex) {
    setBgAndSync(currentPaletteHex);
  }

  activeColorTarget = target;
  activeColorIndex = index;
  
  if (!hexColor) {
    if (target === 'font') {
      hexColor = currentAppearanceSettings.font_color || '#FFFFFF';
    } else if (target === 'gradient') {
      hexColor = gradientColors[index] || '#FFFFFF';
    } else {
      hexColor = '#002fa7';
    }
  }
  
  currentPaletteHex = hexColor;
  const palette = document.querySelector('.color-palette-container');
  if (palette && event) {
    palette.classList.add('show');
    if (event.target) {
      const rect = event.target.getBoundingClientRect();
      // Position it below the clicked target
      palette.style.top = (rect.bottom + window.scrollY + 8) + 'px';
      let left = rect.left + window.scrollX;
      // Prevent it from going off the right edge
      if (left + 320 > window.innerWidth) {
         left = window.innerWidth - 340;
      }
      palette.style.left = left + 'px';
      palette.style.bottom = 'auto';
      palette.style.right = 'auto';
    }
  }

  
  // Highlight active targets
  if (target === 'font') {
    $('#fontColorBox').classList.add('active');
    // Clear gradient actives
    document.querySelectorAll('.gradient-stop-box').forEach(el => el.classList.remove('active'));
  } else if (target === 'gradient') {
    $('#fontColorBox').classList.remove('active');
    document.querySelectorAll('.gradient-stop-box').forEach((el, i) => {
      el.classList.toggle('active', i === index);
    });
  }
  
  syncPaletteUI(hexColor);
}

function applyPaletteToTarget(hex) {
  currentPaletteHex = hex;
  $('#palettePreview').style.backgroundColor = hex;
  
  if (activeColorTarget === 'font') {
    $('#fontColorBox').style.backgroundColor = hex;
    setFontColor(hex);
  } else if (activeColorTarget === 'gradient') {
    gradientColors[activeColorIndex] = hex;
    renderGradientStops(false); // Don't steal focus
  } else if (activeColorTarget === 'bg') {
    const bgBox = $('#bgColorBox');
    if (bgBox) bgBox.style.backgroundColor = hex;
    applyBg(hex); // Only apply visually during drag, do not sync yet
  }
}

function syncPaletteUI(hex) {
  $('#palettePreview').style.backgroundColor = hex;
  $('#hex-input').value = hex.toUpperCase();
  
  const [r, g, b] = hexToRgb(hex);
  $('#rgb-r').value = r; $('#rgb-r-num').value = r;
  $('#rgb-g').value = g; $('#rgb-g-num').value = g;
  $('#rgb-b').value = b; $('#rgb-b-num').value = b;
  
  const [h, s, l] = rgbToHsl(r, g, b);
  const hDeg = Math.round(h * 360);
  const sPct = Math.round(s * 100);
  const lPct = Math.round(l * 100);
  
  $('#hsl-h').value = hDeg; $('#hsl-h-num').value = hDeg;
  $('#hsl-s').value = sPct; $('#hsl-s-num').value = sPct;
  $('#hsl-l').value = lPct; $('#hsl-l-num').value = lPct;
  
  updateHSLTracks(hDeg, sPct, lPct);
}

function updateHSLTracks(h, s, l) {
  const satTrack = $('#hsl-s');
  const litTrack = $('#hsl-l');
  if (satTrack) {
    const c0 = hslToHex(h/360, 0, l/100);
    const c100 = hslToHex(h/360, 1, l/100);
    satTrack.style.background = `linear-gradient(to right, ${c0}, ${c100})`;
  }
  if (litTrack) {
    const c50 = hslToHex(h/360, s/100, 0.5);
    const ch = hslToHex(h/360, s/100, l/100); // the hue color at 50% lit
    litTrack.style.background = `linear-gradient(to right, #000, ${c50}, #fff)`;
  }
}

function updateFromRGB() {
  let r = parseInt($('#rgb-r').value) || 0;
  let g = parseInt($('#rgb-g').value) || 0;
  let b = parseInt($('#rgb-b').value) || 0;
  $('#rgb-r-num').value = r; $('#rgb-g-num').value = g; $('#rgb-b-num').value = b;
  
  const hex = rgbToHexStr(r, g, b);
  syncPaletteUI(hex);
  applyPaletteToTarget(hex);
}

function updateFromHSL() {
  let h = parseInt($('#hsl-h').value) || 0;
  let s = parseInt($('#hsl-s').value) || 0;
  let l = parseInt($('#hsl-l').value) || 0;
  $('#hsl-h-num').value = h; $('#hsl-s-num').value = s; $('#hsl-l-num').value = l;
  
  const [r, g, b] = hslToRgb(h/360, s/100, l/100);
  const hex = rgbToHexStr(r, g, b);
  syncPaletteUI(hex);
  applyPaletteToTarget(hex);
}

function updateFromHEX() {
  let hex = $('#hex-input').value.trim();
  if (/^#[0-9A-Fa-f]{6}$/.test(hex)) {
    syncPaletteUI(hex);
    applyPaletteToTarget(hex);
  }
}

function updateFromNative(hex) {
  const hexInput = document.getElementById('hex-input');
  if (hexInput) {
    hexInput.value = hex;
    updateFromHEX();
  }
}

async function activateEyeDropper() {
  if (!window.EyeDropper) {
    toast('您的浏览器不支持吸管工具', true);
    return;
  }
  try {
    const dropper = new EyeDropper();
    const result = await dropper.open();
    syncPaletteUI(result.sRGBHex);
    applyPaletteToTarget(result.sRGBHex);
  } catch (e) {
    // User canceled
  }
}

function renderGradientStops(stealFocus = true) {
  const container = $('#gradientStops');
  if (!container) return;
  container.innerHTML = gradientColors.map((c, i) => `
    <div class="gradient-stop-box ${activeColorTarget === 'gradient' && activeColorIndex === i ? 'active' : ''}" 
         style="background-color: ${c}" 
         onclick="setActiveColorTarget('gradient', ${i}, '${c}', event)"
         title="点击编辑色标">
      ${gradientColors.length > 2 ? `<button class="del-btn" onclick="removeGradientStop(event, ${i})">×</button>` : ''}
    </div>
  `).join('');
  
  if (stealFocus && gradientColors.length > 0) {
    setActiveColorTarget('gradient', gradientColors.length - 1, gradientColors[gradientColors.length - 1]);
  }
}

function removeGradientStop(e, index) {
  e.stopPropagation();
  if (gradientColors.length <= 2) return;
  gradientColors.splice(index, 1);
  if (activeColorTarget === 'gradient' && activeColorIndex === index) {
    activeColorTarget = null; // deselect
  } else if (activeColorIndex > index) {
    activeColorIndex--;
  }
  renderGradientStops(false);
}

let gradientColors = ['#002fa7', '#2e1065'];
function addGradientStop() {
  gradientColors.push('#ffffff');
  renderGradientStops(true);
}
function applyGradient() {
  if (gradientColors.length < 2) return;
  const gradStr = `linear-gradient(135deg, ${gradientColors.join(', ')})`;
  setBgAndSync(gradStr);
}

async function saveCustomPreset(type) {
  let val = '';
  if (type === 'color') {
    val = currentPaletteHex || '#002fa7';
  } else if (type === 'gradient') {
    if (gradientColors.length < 2) return;
    val = `linear-gradient(135deg, ${gradientColors.join(', ')})`;
  }
  if (!val) return;
  
  let customPresets = currentAppearanceSettings.custom_bg_presets || [];
  if (!customPresets.includes(val)) {
    customPresets.unshift(val); // Add to beginning
  }
  
  try {
    await API.settings.put({ custom_bg_presets: customPresets });
    toast('已保存为自定义预设');
    loadAppearance(); // Refresh presets display
  } catch (e) {
    toast('保存预设失败', true);
  }
}


document.addEventListener('click', (e) => {
  const palette = document.querySelector('.color-palette-container');
  if (!palette) return;
  if (!palette.classList.contains('show')) return;
  
  // if click is inside palette or on a color target, do nothing
  if (palette.contains(e.target) || e.target.closest('.color-preview-box') || e.target.closest('.gradient-stop-box')) {
    return;
  }
  
  palette.classList.remove('show');
  
  if (activeColorTarget === 'bg' && currentPaletteHex) {
    setBgAndSync(currentPaletteHex); // Sync bg color when palette is closed
  }
  
  // clear active highlights
  document.querySelectorAll('.color-preview-box, .gradient-stop-box').forEach(el => el.classList.remove('active'));
  activeColorTarget = null;
});

// Make palette draggable
document.addEventListener('DOMContentLoaded', () => {
  const headerDrag = document.getElementById('paletteHeaderDrag');
  let isDragging = false, dragOffsetX = 0, dragOffsetY = 0;
  if (headerDrag) {
    headerDrag.addEventListener('mousedown', (e) => {
      isDragging = true;
      const palette = document.querySelector('.color-palette-container');
      if (!palette) return;
      dragOffsetX = e.clientX - palette.offsetLeft;
      dragOffsetY = e.clientY - palette.offsetTop;
    });
    document.addEventListener('mousemove', (e) => {
      if (!isDragging) return;
      const palette = document.querySelector('.color-palette-container');
      if (!palette) return;
      palette.style.left = (e.clientX - dragOffsetX) + 'px';
      palette.style.top = (e.clientY - dragOffsetY) + 'px';
      palette.style.bottom = 'auto';
      palette.style.right = 'auto';
    });
    document.addEventListener('mouseup', () => isDragging = false);
  }
});

window.deletePreset = async function(val, event) {
  if (event) event.stopPropagation();
  if (!confirm('确定要删除这个预设吗？')) return;

  if (val.startsWith('url')) {
    const match = val.match(/\/assets\/(background.*?\.jpg)/);
    if (match && match[1]) {
      try {
        const res = await API.raw('/api/admin/delete-bg', { method: 'POST', body: JSON.stringify({ filename: match[1] }) });
        if (res.ok) {
          toast('图片已删除');
          loadAppearance();
        }
      } catch (e) {
        toast('删除失败: ' + e.message);
      }
    }
  } else {
    if (currentAppearanceSettings.custom_bg_presets) {
      currentAppearanceSettings.custom_bg_presets = currentAppearanceSettings.custom_bg_presets.filter(p => p !== val);
      try {
        await API.settings.put({ custom_bg_presets: currentAppearanceSettings.custom_bg_presets });
        toast('预设已删除');
        loadAppearance();
      } catch (e) {
        toast('删除失败: ' + e.message);
      }
    }
  }
};
