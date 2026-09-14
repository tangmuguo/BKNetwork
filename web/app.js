(() => {
  'use strict';

  const $ = (selector, root = document) => root.querySelector(selector);
  const $$ = (selector, root = document) => Array.from(root.querySelectorAll(selector));

  const dom = {
    sidebar: $('#sidebar'),
    mobileMenuBtn: $('#mobileMenuBtn'),
    navItems: $$('.nav-item[href]'),
    topSettingsBtn: $('#topSettingsBtn'),
    settingsOpenBtn: $('#settingsOpenBtn'),
    settingsLayer: $('#settingsLayer'),
    settingsModal: $('.settings-modal'),
    settingsCloseBtn: $('#settingsCloseBtn'),
    settingsCancelBtn: $('#settingsCancelBtn'),
    settingsSaveBtn: $('#settingsSaveBtn'),
    settingAutoStart: $('#settingAutoStart'),
    settingsStatus: $('#settingsStatus'),
    backendChip: $('#backendChip'),
    backendText: $('#backendText'),
    readonlyChip: $('#readonlyChip'),
    readonlyBanner: $('#readonlyBanner'),
    appVersion: $('#appVersion'),
    footerVersion: $('#footerVersion'),
    platformName: $('#platformName'),
    lastUpdated: $('#lastUpdated'),
    livePulse: $('#livePulse'),
    connectionCard: $('#connectionCard'),
    phaseBadge: $('#phaseBadge'),
    phaseText: $('#phaseText'),
    connectionTitle: $('#connectionTitle'),
    connectionMessage: $('#connectionMessage'),
    progressBar: $('#progressBar'),
    progressLabel: $('#progressLabel'),
    connectionAction: $('#connectionAction'),
    connectionActionText: $('#connectionActionText'),
    selectedModeHint: $('#selectedModeHint'),
    modeTabs: $$('.mode-tab'),
    modePanels: $$('.mode-panel'),
    warpTabState: $('#warpTabState'),
    wireguardTabState: $('#wireguardTabState'),
    warpAvailability: $('#warpAvailability'),
    wireguardAvailability: $('#wireguardAvailability'),
    warpPanelNote: $('#warpPanelNote'),
    wireguardPanelNote: $('#wireguardPanelNote'),
    warpPanelState: $('#warpPanelState'),
    wireguardPanelState: $('#wireguardPanelState'),
    interfaceSelect: $('#interfaceSelect'),
    interfaceHelp: $('#interfaceHelp'),
    profileSelect: $('#profileSelect'),
    profileHelp: $('#profileHelp'),
    profileRefreshBtn: $('#profileRefreshBtn'),
    recoveryBanner: $('#recoveryBanner'),
    recoveryText: $('#recoveryText'),
    recoveryAction: $('#recoveryAction'),
    readinessSummary: $('#readinessSummary'),
    readinessCount: $('#readinessCount'),
    readinessList: $('#readinessList'),
    networkFacts: $('#networkFacts'),
    pathStrip: $('#pathStrip'),
    clearLogBtn: $('#clearLogBtn'),
    logList: $('#logList'),
    logCount: $('#logCount'),
    toast: $('#toast'),
    toastIcon: $('#toastIcon'),
    toastText: $('#toastText'),
  };

  const modeLabels = {
    warp: 'WARP',
    wireguard: 'WireGuard',
    direct: '直连',
  };

  const phaseLabels = {
    idle: '未连接',
    connecting: '连接中',
    disconnecting: '断开中',
    connected: '已连接',
    recovery: '待恢复',
    error: '连接失败',
  };

  const state = {
    status: null,
    homeNetwork: null,
    selectedMode: 'warp',
    selectedInterface: '',
    selectedProfile: '',
    modeWasSelected: false,
    busy: false,
    profileLoading: false,
    settingsLoading: false,
    settingsSaving: false,
    settingsLoaded: false,
    settings: { autoStart: false },
    previousFocus: null,
    toastTimer: null,
    pollTimer: null,
    ws: null,
    wsRetryTimer: null,
    wsConnectedOnce: false,
    wsStopped: false,
    logEntries: [],
    lastStatusMessage: '',
    lastStatusPhase: '',
    initialized: false,
  };

  class ApiError extends Error {
    constructor(message, detail = '', status = 0) {
      super(message);
      this.name = 'ApiError';
      this.detail = detail;
      this.status = status;
    }
  }

  function text(value, fallback = '—') {
    if (value === null || value === undefined || value === '') {
      return fallback;
    }
    return String(value);
  }

  function setText(element, value, fallback = '—') {
    if (element) {
      element.textContent = text(value, fallback);
    }
  }

  function phaseTone(phase) {
    if (phase === 'connected') return 'connected';
    if (phase === 'error' || phase === 'recovery') return 'error';
    if (phase === 'connecting' || phase === 'disconnecting') return 'pending';
    return 'pending';
  }

  function setDot(element, tone) {
    if (!element) return;
    element.className = `status-dot ${tone}`;
  }

  function formatTime(value = Date.now()) {
    const date = value instanceof Date ? value : new Date(value);
    if (Number.isNaN(date.getTime())) return '--:--:--';
    return date.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false });
  }

  function normalizeIPv6Address(value) {
    let address = text(value, '').trim().toLowerCase();
    if (!address) return '';
    const slash = address.indexOf('/');
    if (slash >= 0) address = address.slice(0, slash);
    if (address.startsWith('[') && address.endsWith(']')) address = address.slice(1, -1);
    const zone = address.indexOf('%');
    if (zone >= 0) address = address.slice(0, zone);
    return address;
  }

  function isGlobalIPv6(value) {
    const address = normalizeIPv6Address(value);
    if (!address || !address.includes(':') || address === '::' || address === '::1') return false;
    const firstHextet = Number.parseInt(address.split(':')[0] || '0', 16);
    if (!Number.isFinite(firstHextet)) return false;
    // Global unicast is 2000::/3. Exclude ULA (fc00::/7), link-local
    // (fe80::/10), and the unspecified/IPv4-mapped ranges represented by 0.
    if (firstHextet < 0x2000 || firstHextet > 0x3fff) return false;
    if ((firstHextet & 0xfe00) === 0xfc00) return false;
    if ((firstHextet & 0xffc0) === 0xfe80) return false;
    return true;
  }

  function interfaceStateUsable(iface) {
    const value = text(iface?.state, '').trim().toLowerCase();
    return value === 'up' || value === 'connected';
  }

  function globalIPv6Addresses(iface) {
    return Array.isArray(iface?.ipv6) ? iface.ipv6.filter(isGlobalIPv6) : [];
  }

  function interfaceReady(iface) {
    return Boolean(iface && iface.physical && interfaceStateUsable(iface) && globalIPv6Addresses(iface).length > 0);
  }

  function formatIPv6Addresses(addresses) {
    const values = Array.isArray(addresses) ? addresses.filter(Boolean).map(String) : [];
    if (!values.length) return { label: '未分配', title: '' };
    const shorten = (value) => value.length > 27 ? `${value.slice(0, 16)}…${value.slice(-8)}` : value;
    const title = values.join('\n');
    const visible = values.slice(0, 2).map(shorten).join(' · ');
    const more = values.length > 2 ? ` +${values.length - 2}` : '';
    return { label: `${visible}${more}`, title };
  }

  function statusLabel(value, fallback = '未连接') {
    if (typeof value === 'string' || typeof value === 'number') {
      const raw = String(value);
      const normalized = raw.toLowerCase();
      if (['disconnected', 'down', 'stopped', 'inactive'].includes(normalized)) return '未连接';
      if (['connected', 'up', 'running', 'active'].includes(normalized)) return '已连接';
      if (['connecting', 'starting', 'activating'].includes(normalized)) return '连接中';
      return raw;
    }
    if (value && typeof value === 'object') {
      if (typeof value.state === 'string' && value.state) return value.state;
      if (typeof value.status === 'string' && value.status) return value.status;
      if (value.connected === true) return '已连接';
      if (value.connected === false) return '未连接';
    }
    return fallback;
  }

  function modeFromStatus(network) {
    return network && (network.mode === 'warp' || network.mode === 'wireguard') ? network.mode : '';
  }

  function isConnected(network, mode = state.selectedMode) {
    return network && network.phase === 'connected' && network.mode === mode;
  }

  function isReadOnly() {
    return state.status?.network?.privileged === false;
  }

  function getNetwork() {
    return state.status?.network || {};
  }

  async function request(path, options = {}) {
    let response;
    try {
      response = await fetch(path, {
        credentials: 'same-origin',
        cache: 'no-store',
        ...options,
        headers: {
          Accept: 'application/json',
          ...(options.body ? { 'Content-Type': 'application/json' } : {}),
          ...(options.headers || {}),
        },
      });
    } catch (error) {
      throw new ApiError('无法连接本机服务', error instanceof Error ? error.message : 'network error');
    }

    let payload = null;
    try {
      payload = await response.json();
    } catch (_) {
      payload = null;
    }

    if (!response.ok || (payload && payload.ok === false)) {
      const message = payload?.error || `请求失败（${response.status}）`;
      const detail = payload?.detail || '';
      throw new ApiError(message, detail, response.status);
    }
    return payload || {};
  }

  function showToast(message, tone = 'info') {
    if (!dom.toast || !dom.toastText) return;
    window.clearTimeout(state.toastTimer);
    dom.toast.hidden = false;
    dom.toast.classList.toggle('is-error', tone === 'error');
    setText(dom.toastText, message, '操作完成');
    if (dom.toastIcon) {
      dom.toastIcon.setAttribute('href', tone === 'error' ? '#icon-alert' : '#icon-check');
    }
    state.toastTimer = window.setTimeout(() => {
      dom.toast.hidden = true;
    }, tone === 'error' ? 6200 : 3600);
  }

  function appendLog(message, type = '状态', tone = 'info', timestamp = Date.now()) {
    const cleanMessage = text(message, '').trim();
    if (!cleanMessage) return;

    state.logEntries.push({ message: cleanMessage, type: text(type, '状态'), tone, timestamp });
    if (state.logEntries.length > 80) state.logEntries.shift();

    if (!dom.logList) return;
    dom.logList.replaceChildren();
    for (const entry of state.logEntries) {
      const item = document.createElement('li');
      item.className = `log-entry${entry.tone === 'error' ? ' is-error' : ''}`;

      const time = document.createElement('span');
      time.className = 'log-time';
      time.textContent = formatTime(entry.timestamp);
      const logType = document.createElement('span');
      logType.className = 'log-type';
      logType.textContent = entry.type;
      const logMessage = document.createElement('span');
      logMessage.className = 'log-message';
      logMessage.textContent = entry.message;
      item.append(time, logType, logMessage);
      dom.logList.append(item);
    }
    dom.logList.scrollTop = dom.logList.scrollHeight;
    setText(dom.logCount, `${state.logEntries.length} 条记录`);
  }

  function clearLog() {
    state.logEntries = [];
    if (dom.logList) {
      const empty = document.createElement('li');
      empty.className = 'log-empty';
      empty.textContent = '等待连接事件…';
      dom.logList.replaceChildren(empty);
    }
    setText(dom.logCount, '0 条记录');
  }

  function setBusy(busy) {
    state.busy = busy;
    document.body.classList.toggle('is-busy', busy);
    const controls = [
      dom.connectionAction,
      dom.recoveryAction,
      dom.profileRefreshBtn,
      dom.interfaceSelect,
      dom.profileSelect,
      ...dom.modeTabs,
    ];
    controls.filter(Boolean).forEach((control) => {
      control.disabled = busy || control.dataset.readonly === 'true';
    });
    renderControls();
  }

  function setBackend(online, message) {
    if (dom.backendChip) {
      dom.backendChip.classList.toggle('is-online', online);
    }
    const dot = dom.backendChip ? $('.status-dot', dom.backendChip) : null;
    setDot(dot, online ? 'connected' : 'error');
    setText(dom.backendText, message, online ? '后端在线' : '后端离线');
  }

  function selectMode(mode, fromUser = false) {
    if (mode !== 'warp' && mode !== 'wireguard') return;
    state.selectedMode = mode;
    state.modeWasSelected = state.modeWasSelected || fromUser;
    renderModeTabs();
    renderControls();
  }

  function renderModeTabs() {
    const network = getNetwork();
    const activeMode = modeFromStatus(network);
    const warpInstalled = network.warp?.installed === true;
    const wireguardInstalled = network.wireguard?.installed === true;
    const wireguardProfiles = Array.isArray(state.homeNetwork?.profiles) ? state.homeNetwork.profiles.length : 0;

    dom.modeTabs.forEach((tab) => {
      const mode = tab.dataset.mode;
      const selected = mode === state.selectedMode;
      tab.classList.toggle('is-selected', selected);
      tab.setAttribute('aria-selected', selected ? 'true' : 'false');
    });

    if (dom.warpTabState) {
      dom.warpTabState.textContent = isConnected(network, 'warp') ? '已连接' : (warpInstalled ? '可用' : '未安装');
    }
    if (dom.wireguardTabState) {
      dom.wireguardTabState.textContent = isConnected(network, 'wireguard') ? '已连接' : (wireguardInstalled && wireguardProfiles > 0 ? '可用' : '待配置');
    }
    if (dom.selectedModeHint) {
      dom.selectedModeHint.textContent = activeMode && network.phase === 'connected' && activeMode !== state.selectedMode
        ? `当前使用 ${modeLabels[activeMode]} · 已选择 ${modeLabels[state.selectedMode]}`
        : `已选择 ${modeLabels[state.selectedMode]}`;
    }

    dom.modePanels.forEach((panel) => {
      const selected = panel.dataset.modePanel === state.selectedMode;
      panel.hidden = !selected;
      panel.classList.toggle('is-visible', selected);
    });
  }

  function normalizeInterface(iface) {
    if (!iface || typeof iface !== 'object') return null;
    const name = text(iface.name, '').trim();
    if (!name) return null;
    return {
      ...iface,
      name,
      physical: iface.physical === true,
      state: text(iface.state, 'unknown'),
      ipv4: Array.isArray(iface.ipv4) ? iface.ipv4 : [],
      ipv6: Array.isArray(iface.ipv6) ? iface.ipv6 : [],
    };
  }

  function physicalInterfaces(network) {
    return (Array.isArray(network.interfaces) ? network.interfaces : [])
      .map(normalizeInterface)
      .filter((iface) => iface && iface.physical);
  }

  function renderInterfaces() {
    if (!dom.interfaceSelect) return;
    const network = getNetwork();
    const interfaces = physicalInterfaces(network);
    const networkInterface = text(network.interface, '').trim();
    const recommended = text(network.recommendedInterface, '').trim();

    if (!state.selectedInterface || !interfaces.some((iface) => iface.name === state.selectedInterface)) {
      const preferred = [
        interfaces.find((iface) => iface.name === networkInterface && interfaceReady(iface)),
        interfaces.find((iface) => iface.name === recommended && interfaceReady(iface)),
        interfaces.find((iface) => interfaceReady(iface)),
        interfaces.find((iface) => iface.name === networkInterface && interfaceStateUsable(iface)),
        interfaces.find((iface) => interfaceStateUsable(iface)),
        interfaces[0],
      ].find(Boolean);
      state.selectedInterface = preferred?.name || '';
    }

    dom.interfaceSelect.replaceChildren();
    if (!interfaces.length) {
      const option = document.createElement('option');
      option.value = '';
      option.textContent = '没有检测到可用的物理网卡';
      dom.interfaceSelect.append(option);
      dom.interfaceSelect.disabled = true;
      setText(dom.interfaceHelp, '请检查本机网卡是否已连接。');
      return;
    }

    interfaces.forEach((iface) => {
      const option = document.createElement('option');
      option.value = iface.name;
      const stateLabel = iface.state && iface.state !== 'unknown' ? ` · ${iface.state}` : '';
      option.textContent = `${iface.name}${stateLabel}`;
      option.selected = iface.name === state.selectedInterface;
      dom.interfaceSelect.append(option);
    });
    dom.interfaceSelect.disabled = state.busy || isReadOnly();
    setText(dom.interfaceHelp, recommended ? `推荐网卡：${recommended}。WARP 与 WireGuard 共用此物理出口。` : 'WARP 与 WireGuard 共用选中的物理出口。');
  }

  function normalizeProfile(profile) {
    if (typeof profile === 'string') {
      const value = profile.trim();
      return value ? { value, label: value, detail: '' } : null;
    }
    if (!profile || typeof profile !== 'object') return null;
    const value = text(profile.name || profile.id || profile.tunnelName || profile.file || profile.path || profile.label, '').trim();
    if (!value) return null;
    const label = text(profile.displayName || profile.label || profile.name || value).trim();
    const detail = text(profile.detail || profile.endpoint || '', '').trim();
    return { ...profile, value, label, detail };
  }

  function getProfiles() {
    return (Array.isArray(state.homeNetwork?.profiles) ? state.homeNetwork.profiles : [])
      .map(normalizeProfile)
      .filter(Boolean);
  }

  function renderProfiles() {
    if (!dom.profileSelect) return;
    const profiles = getProfiles();
    const home = state.homeNetwork || {};
    const configuredTunnel = text(home.tunnelName, '').trim();
    const currentNetworkProfile = text(getNetwork().profile, '').trim();

    if (!state.selectedProfile || !profiles.some((profile) => profile.value === state.selectedProfile)) {
      const preferred = [currentNetworkProfile, configuredTunnel].find((value) => value && profiles.some((profile) => profile.value === value));
      state.selectedProfile = preferred || profiles[0]?.value || '';
    }

    dom.profileSelect.replaceChildren();
    if (!profiles.length) {
      const option = document.createElement('option');
      option.value = '';
      option.textContent = home.profilesError ? '无法读取 WireGuard 配置' : '没有可用的 WireGuard 配置';
      dom.profileSelect.append(option);
      dom.profileSelect.disabled = true;
      setText(dom.profileHelp, home.profilesError || '请先在本机导入 WireGuard 配置，再点击刷新。');
      return;
    }

    profiles.forEach((profile) => {
      const option = document.createElement('option');
      option.value = profile.value;
      option.textContent = profile.detail ? `${profile.label} · ${profile.detail}` : profile.label;
      option.selected = profile.value === state.selectedProfile;
      dom.profileSelect.append(option);
    });
    dom.profileSelect.disabled = state.busy || isReadOnly();
    setText(dom.profileHelp, home.profilesError || '从本机 WireGuard 配置目录读取名称；私钥不会显示在页面或日志中。');
  }

  function relevantDependencies(network, mode) {
    const dependencies = Array.isArray(network.dependencies) ? network.dependencies : [];
    const relevant = dependencies.filter((dependency) => {
      const requiredFor = Array.isArray(dependency?.requiredFor) ? dependency.requiredFor : [dependency?.requiredFor];
      return requiredFor.filter(Boolean).some((value) => String(value).toLowerCase() === mode);
    });
    return relevant.length ? relevant : dependencies;
  }

  function createReadinessItem(name, detail, available, statusText) {
    const item = document.createElement('div');
    item.className = `readiness-item ${available ? 'is-ready' : 'is-error'}`;

    const icon = document.createElement('span');
    icon.className = 'readiness-icon';
    const iconSvg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    iconSvg.setAttribute('class', 'icon');
    const use = document.createElementNS('http://www.w3.org/2000/svg', 'use');
    use.setAttribute('href', available ? '#icon-check' : '#icon-alert');
    iconSvg.append(use);
    icon.append(iconSvg);

    const copy = document.createElement('span');
    copy.className = 'readiness-copy';
    const title = document.createElement('strong');
    title.textContent = name;
    const note = document.createElement('small');
    note.textContent = detail || (available ? '已满足连接条件' : '暂不可用');
    copy.append(title, note);

    const status = document.createElement('span');
    status.className = 'readiness-status';
    status.textContent = statusText || (available ? '就绪' : '待处理');
    item.append(icon, copy, status);
    return item;
  }

  function readinessChecks(network) {
    const mode = state.selectedMode;
    const dependencies = relevantDependencies(network, mode);
    const checks = dependencies.map((dependency) => ({
      name: text(dependency?.name, '系统依赖'),
      detail: text(dependency?.detail, ''),
      available: dependency?.available === true,
      status: dependency?.available === true ? '已安装' : '缺少',
    }));

    if (mode === 'warp' && !checks.some((check) => /warp/i.test(check.name))) {
      checks.push({ name: 'WARP 客户端', detail: network.warp?.status || '', available: network.warp?.installed === true, status: network.warp?.installed === true ? '已安装' : '缺少' });
    }
    if (mode === 'wireguard' && !checks.some((check) => /wireguard/i.test(check.name))) {
      checks.push({ name: 'WireGuard', detail: '', available: network.wireguard?.installed === true, status: network.wireguard?.installed === true ? '已安装' : '缺少' });
    }

    const iface = physicalInterfaces(network).find((item) => item.name === state.selectedInterface);
    const globalAddresses = globalIPv6Addresses(iface);
    const interfaceDetail = iface
      ? `${iface.name} · ${interfaceStateUsable(iface) ? '已连接' : '未连接'}${globalAddresses.length ? ` · ${globalAddresses.length} 个公网 IPv6` : ' · 无公网 IPv6'}`
      : '没有选中的物理网卡';
    checks.push({
      name: '物理网卡',
      detail: interfaceDetail,
      available: Boolean(iface && interfaceStateUsable(iface)),
      status: iface ? (interfaceStateUsable(iface) ? '已连接' : '未连接') : '待选择',
    });
    checks.push({
      name: '公网 IPv6',
      detail: iface
        ? (globalAddresses.length ? globalAddresses.map(String).join(' · ') : '未分配公网 IPv6；当前只有链路本地地址')
        : '先选择物理网卡',
      available: globalAddresses.length > 0,
      status: globalAddresses.length ? '已分配' : '缺少',
    });
    checks.push({
      name: '管理员权限',
      detail: network.privileged === false ? '普通用户预览，只读' : (network.privileged === true ? '可执行本机网络操作' : '等待权限状态'),
      available: network.privileged === true,
      status: network.privileged === true ? '已授权' : '只读',
    });
    if (mode === 'wireguard') {
      const hasProfile = Boolean(state.selectedProfile && getProfiles().some((profile) => profile.value === state.selectedProfile));
      checks.push({ name: 'WireGuard 配置', detail: state.homeNetwork?.profilesError || (hasProfile ? state.selectedProfile : '未选择配置'), available: hasProfile, status: hasProfile ? '已选择' : '待配置' });
    }
    return checks;
  }

  function renderReadiness() {
    const network = getNetwork();
    const checks = readinessChecks(network);
    const ready = checks.filter((check) => check.available).length;
    const total = checks.length;

    if (dom.readinessList) {
      dom.readinessList.replaceChildren(...checks.map((check) => createReadinessItem(check.name, check.detail, check.available, check.status)));
    }
    setText(dom.readinessCount, `${ready}/${total} 就绪`);
    setText(dom.readinessSummary, ready === total && total > 0 ? `${modeLabels[state.selectedMode]} 可以连接` : `${total - ready} 项待处理`);
  }

  function renderFacts() {
    const network = getNetwork();
    const iface = physicalInterfaces(network).find((item) => item.name === state.selectedInterface);
    const ipv6 = formatIPv6Addresses(iface?.ipv6);
    const fields = [
      ['物理网卡', iface?.name || network.interface || '未选择', iface?.state || ''],
      ['IPv6 地址', iface ? ipv6.label : '未读取', ipv6.title],
      ['验证结果', network.verified === true ? '已验证' : (network.verified === false ? '未验证' : '等待验证')],
      ['协议栈', network.ipv6Only === true ? '仅 IPv6' : (network.ipv6Only === false ? '双栈' : '等待读取')],
    ];
    if (dom.networkFacts) {
      dom.networkFacts.replaceChildren(...fields.map(([label, value, titleText]) => {
        const fact = document.createElement('div');
        fact.className = 'fact';
        const labelNode = document.createElement('span');
        labelNode.textContent = label;
        const valueNode = document.createElement('strong');
        valueNode.textContent = text(value);
        if (titleText) valueNode.title = titleText;
        fact.append(labelNode, valueNode);
        return fact;
      }));
    }

    if (dom.pathStrip) {
      const nodes = [
        ['本机', state.selectedInterface || '本机', Boolean(state.selectedInterface)],
        [modeLabels[state.selectedMode], modeLabels[state.selectedMode], isConnected(network, state.selectedMode)],
        ['校园网 IPv6', '校园网 IPv6', network.verified === true],
      ];
      dom.pathStrip.replaceChildren();
      nodes.forEach((node, index) => {
        const nodeElement = document.createElement('span');
        nodeElement.className = `path-node ${node[2] ? (index === 2 ? 'is-connected' : 'is-active') : 'is-muted'}`;
        nodeElement.textContent = node[1];
        dom.pathStrip.append(nodeElement);
        if (index < nodes.length - 1) {
          const line = document.createElement('span');
          line.className = `path-line ${node[2] ? 'is-active' : ''}`;
          dom.pathStrip.append(line);
        }
      });
    }
  }

  function canStartMode(mode = state.selectedMode) {
    const network = getNetwork();
    if (isReadOnly() || !state.status || network.privileged !== true) return false;
    const iface = physicalInterfaces(network).find((item) => item.name === state.selectedInterface);
    if (!interfaceReady(iface)) return false;
    if (mode === 'warp' && network.warp?.installed !== true) return false;
    if (mode === 'wireguard') {
      if (network.wireguard?.installed !== true) return false;
      if (!state.selectedProfile || !getProfiles().some((profile) => profile.value === state.selectedProfile)) return false;
      if (state.homeNetwork?.profilesError) return false;
    }
    return relevantDependencies(network, mode).every((dependency) => dependency?.available === true);
  }

  function renderControls() {
    const network = getNetwork();
    const phase = network.phase || 'idle';
    const recovery = network.recoveryPending === true || phase === 'recovery';
    const active = phase === 'connected' && (network.mode === 'warp' || network.mode === 'wireguard');
    const selectedIsActive = active && network.mode === state.selectedMode;
    const switching = active && network.mode !== state.selectedMode;
    let label = '连接';
    let disabled = state.busy || isReadOnly();

    if (recovery) {
      label = '恢复直连';
      disabled = state.busy || isReadOnly();
    } else if (phase === 'connecting' || phase === 'disconnecting') {
      label = phase === 'connecting' ? '连接中…' : '断开中…';
      disabled = true;
    } else if (selectedIsActive) {
      label = '断开连接';
    } else if (switching) {
      label = `切换到 ${modeLabels[state.selectedMode]}`;
      disabled = disabled || !canStartMode(state.selectedMode);
    } else if (phase === 'error') {
      label = `重试 ${modeLabels[state.selectedMode]}`;
      disabled = disabled || !canStartMode(state.selectedMode);
    } else {
      label = `连接 ${modeLabels[state.selectedMode]}`;
      disabled = disabled || !canStartMode(state.selectedMode);
    }

    if (dom.connectionAction) {
      dom.connectionAction.disabled = disabled;
      setText(dom.connectionActionText, isReadOnly() ? '只读预览' : label);
      const use = $('use', dom.connectionAction);
      if (use) use.setAttribute('href', recovery ? '#icon-refresh' : '#icon-power');
    }
    if (dom.recoveryAction) dom.recoveryAction.disabled = state.busy || isReadOnly();
    if (dom.profileRefreshBtn) dom.profileRefreshBtn.disabled = state.busy || state.profileLoading;
    if (dom.settingsSaveBtn) dom.settingsSaveBtn.disabled = state.settingsSaving || state.settingsLoading || isReadOnly();
    if (dom.settingAutoStart) dom.settingAutoStart.disabled = state.settingsLoading || state.settingsSaving || isReadOnly();
    renderInterfaces();
    renderProfiles();
  }

  function renderStatus(payload) {
    state.status = payload;
    const network = getNetwork();
    const phase = phaseLabels[network.phase] ? network.phase : 'idle';
    const activeMode = modeFromStatus(network);

    if (!state.modeWasSelected && activeMode && network.phase === 'connected') {
      state.selectedMode = activeMode;
    }

    setBackend(true, '后端在线');
    setText(dom.appVersion, payload.version ? `v${String(payload.version).replace(/^v/i, '')}` : '版本未知');
    setText(dom.footerVersion, payload.version ? `版本 ${String(payload.version).replace(/^v/i, '')}` : '版本未知');
    const platform = text(network.platform, '').toLowerCase() === 'linux' ? 'Ubuntu 本机' : (network.platform ? `平台 ${network.platform}` : '本机 Linux');
    setText(dom.platformName, platform);
    setText(dom.lastUpdated, `上次同步 ${formatTime()}`);
    if (dom.livePulse) dom.livePulse.classList.toggle('is-offline', false);

    const tone = phaseTone(network.phase);
    const phaseLabel = phaseLabels[network.phase] || '未知状态';
    if (dom.connectionCard) dom.connectionCard.dataset.phase = network.phase || 'idle';
    setDot($('.status-dot', dom.phaseBadge), tone);
    setText(dom.phaseText, phaseLabel);
    setText(dom.progressLabel, phaseLabel);

    if (network.phase === 'connected' && activeMode) {
      setText(dom.connectionTitle, `${modeLabels[activeMode]} 已连接`);
    } else if (network.phase === 'connecting' && activeMode) {
      setText(dom.connectionTitle, `${modeLabels[activeMode]} 连接中`);
    } else if (network.phase === 'disconnecting') {
      setText(dom.connectionTitle, '正在恢复直连');
    } else if (network.phase === 'recovery' || network.recoveryPending) {
      setText(dom.connectionTitle, '网络需要恢复');
    } else if (network.phase === 'error') {
      setText(dom.connectionTitle, '连接未完成');
    } else {
      setText(dom.connectionTitle, '尚未连接');
    }
    setText(dom.connectionMessage, network.message || '选择一个通道开始连接。');

    const isRecovery = network.recoveryPending === true || network.phase === 'recovery';
    dom.recoveryBanner.hidden = !isRecovery;
    setText(dom.recoveryText, network.message || '上次操作留下了待恢复状态，请先执行恢复直连。');
    dom.readonlyChip.hidden = !isReadOnly();
    dom.readonlyBanner.hidden = !isReadOnly();

    const warpInstalled = network.warp?.installed === true;
    const wireguardInstalled = network.wireguard?.installed === true;
    updateAvailability(dom.warpAvailability, warpInstalled, warpInstalled ? '已安装' : '未安装');
    updateAvailability(dom.wireguardAvailability, wireguardInstalled, wireguardInstalled ? '已安装' : '未安装');
    setText(dom.warpPanelNote, network.clashAppProxy
      ? 'Clash 应用分流：ChatGPT 与 quota-float 保留专用代理，断开后恢复原系统代理。'
      : (warpInstalled ? '连接前会检查 WARP 客户端与管理员权限；Clash 应用分流需按使用说明配置一次。' : '未检测到 WARP 客户端，请先安装后再连接。'));
    setText(dom.wireguardPanelNote, state.homeNetwork?.profilesError || (wireguardInstalled ? '连接前会检查 WireGuard 服务与配置。' : '未检测到 WireGuard，请先安装后再连接。'));
    setPanelState(dom.warpPanelState, isConnected(network, 'warp'), statusLabel(network.warp?.status, warpInstalled ? '未连接' : '未安装'));
    setPanelState(dom.wireguardPanelState, isConnected(network, 'wireguard'), statusLabel(state.homeNetwork?.status, wireguardInstalled ? '未连接' : '未安装'));

    renderModeTabs();
    renderInterfaces();
    renderProfiles();
    renderReadiness();
    renderFacts();
    renderControls();

    const statusMessage = text(network.message, '').trim();
    if (statusMessage && (statusMessage !== state.lastStatusMessage || network.phase !== state.lastStatusPhase)) {
      appendLog(statusMessage, '状态', network.phase === 'error' || network.phase === 'recovery' ? 'error' : 'info');
      state.lastStatusMessage = statusMessage;
      state.lastStatusPhase = network.phase || '';
    }
  }

  function updateAvailability(element, available, label) {
    if (!element) return;
    element.classList.toggle('is-ready', available);
    element.classList.toggle('is-error', !available);
    element.textContent = label;
  }

  function setPanelState(element, connected, label) {
    if (!element) return;
    element.classList.toggle('is-connected', connected);
    element.textContent = connected ? '已连接' : text(label, '未连接');
  }

  function renderOffline(error) {
    setBackend(false, '后端离线');
    if (dom.livePulse) dom.livePulse.classList.add('is-offline');
    setText(dom.lastUpdated, '等待后端恢复');
    setText(dom.connectionTitle, '暂时无法读取状态');
    setText(dom.connectionMessage, error?.message || '请确认 BKNetwork 服务正在运行。');
    if (dom.connectionCard) dom.connectionCard.dataset.phase = 'error';
    setDot($('.status-dot', dom.phaseBadge), 'error');
    setText(dom.phaseText, '读取失败');
    setText(dom.progressLabel, '离线');
    if (dom.connectionAction) dom.connectionAction.disabled = true;
    if (dom.readinessList) {
      const empty = document.createElement('div');
      empty.className = 'empty-state';
      empty.textContent = '后端离线，暂时无法读取依赖。';
      dom.readinessList.replaceChildren(empty);
    }
  }

  async function refreshStatus({ quiet = false } = {}) {
    try {
      const payload = await request('/api/v1/status');
      renderStatus(payload);
      if (!state.initialized) {
        state.initialized = true;
        appendLog('已读取本机网络状态。', '系统');
      }
      return payload;
    } catch (error) {
      renderOffline(error);
      if (!quiet || state.lastStatusMessage !== '__offline__') {
        appendLog(error instanceof ApiError ? error.message : '本机状态读取失败。', '系统', 'error');
        state.lastStatusMessage = '__offline__';
      }
      return null;
    }
  }

  async function refreshHomeNetwork({ quiet = false } = {}) {
    state.profileLoading = true;
    renderControls();
    try {
      const payload = await request('/api/v1/home-network');
      state.homeNetwork = payload;
      renderProfiles();
      renderModeTabs();
      renderReadiness();
      renderControls();
      if (!quiet) appendLog('已刷新 WireGuard 配置列表。', 'WireGuard');
      return payload;
    } catch (error) {
      state.homeNetwork = { profiles: [], profilesError: error instanceof ApiError ? error.message : '配置读取失败', status: null };
      renderProfiles();
      renderReadiness();
      renderControls();
      if (!quiet) {
        appendLog(state.homeNetwork.profilesError, 'WireGuard', 'error');
        showToast(state.homeNetwork.profilesError, 'error');
      }
      return null;
    } finally {
      state.profileLoading = false;
      renderControls();
    }
  }

  async function postAction(path, payload, successMessage) {
    setBusy(true);
    try {
      const response = await request(path, {
        method: 'POST',
        body: JSON.stringify(payload),
      });
      if (successMessage) appendLog(successMessage, '操作');
      await refreshStatus({ quiet: true });
      return response;
    } catch (error) {
      const detail = error instanceof ApiError && error.detail ? `：${error.detail}` : '';
      const message = `${error instanceof Error ? error.message : '操作失败'}${detail}`;
      appendLog(message, '操作', 'error');
      showToast(message, 'error');
      await refreshStatus({ quiet: true });
      throw error;
    } finally {
      setBusy(false);
    }
  }

  async function disconnect() {
    return postAction('/api/v1/disconnect', {}, '已请求恢复直连。');
  }

  async function startSelectedMode() {
    if (!canStartMode(state.selectedMode)) {
      const message = isReadOnly() ? '只读预览无法执行连接操作。' : '当前通道的连接条件尚未满足。';
      showToast(message, 'error');
      appendLog(message, '操作', 'error');
      return null;
    }
    if (state.selectedMode === 'warp') {
      return postAction('/api/v1/warp-mode', { ifName: state.selectedInterface, enabled: true }, '已请求启动 WARP。');
    }
    return postAction('/api/v1/home-network', { ifName: state.selectedInterface, tunnelName: state.selectedProfile, action: 'start' }, '已请求启动 WireGuard。');
  }

  async function handleConnectionAction() {
    if (state.busy || isReadOnly()) return;
    const network = getNetwork();
    const recovery = network.recoveryPending === true || network.phase === 'recovery';
    const connected = network.phase === 'connected' && (network.mode === 'warp' || network.mode === 'wireguard');

    try {
      if (recovery || (connected && network.mode === state.selectedMode)) {
        await disconnect();
        showToast('已请求恢复直连。');
        return;
      }
      if (connected && network.mode !== state.selectedMode) {
        await disconnect();
        await startSelectedMode();
        showToast(`已请求切换到 ${modeLabels[state.selectedMode]}。`);
        return;
      }
      await startSelectedMode();
      showToast(`已请求连接 ${modeLabels[state.selectedMode]}。`);
    } catch (_) {
      // postAction already renders the server error and refreshes state.
    }
  }

  async function handleRecovery() {
    if (state.busy || isReadOnly()) return;
    try {
      await disconnect();
      showToast('已请求恢复直连。');
    } catch (_) {
      // postAction already reports the failure.
    }
  }

  function settingsSnapshot() {
    return { autoStart: dom.settingAutoStart?.checked === true };
  }

  function setSettingsStatus(message, tone = 'normal') {
    if (!dom.settingsStatus) return;
    dom.settingsStatus.textContent = message;
    dom.settingsStatus.dataset.tone = tone;
  }

  async function loadSettings() {
    if (state.settingsLoading) return;
    state.settingsLoading = true;
    setSettingsStatus('正在读取…');
    renderControls();
    try {
      const payload = await request('/api/v1/settings');
      const settings = payload.settings || {};
      state.settings = { autoStart: settings.autoStart === true };
      if (dom.settingAutoStart) dom.settingAutoStart.checked = state.settings.autoStart;
      state.settingsLoaded = true;
      setSettingsStatus(isReadOnly() ? '只读预览，无法修改' : '已加载');
    } catch (error) {
      setSettingsStatus(error instanceof Error ? error.message : '设置读取失败', 'error');
      appendLog(error instanceof Error ? error.message : '设置读取失败', '设置', 'error');
    } finally {
      state.settingsLoading = false;
      renderControls();
    }
  }

  async function saveSettings() {
    if (state.settingsSaving || state.settingsLoading || isReadOnly()) return;
    state.settingsSaving = true;
    setSettingsStatus('正在保存…');
    renderControls();
    let saved = false;
    try {
      const payload = await request('/api/v1/settings', {
        method: 'POST',
        body: JSON.stringify(settingsSnapshot()),
      });
      const settings = payload.settings || settingsSnapshot();
      state.settings = { autoStart: settings.autoStart === true };
      if (dom.settingAutoStart) dom.settingAutoStart.checked = state.settings.autoStart;
      setSettingsStatus('已保存');
      appendLog('启动设置已保存。', '设置');
      showToast('启动设置已保存。');
      saved = true;
    } catch (error) {
      const detail = error instanceof ApiError && error.detail ? `：${error.detail}` : '';
      const message = `${error instanceof Error ? error.message : '设置保存失败'}${detail}`;
      setSettingsStatus(message, 'error');
      appendLog(message, '设置', 'error');
      showToast(message, 'error');
    } finally {
      state.settingsSaving = false;
      renderControls();
      if (saved) closeSettings();
    }
  }

  function openSettings() {
    if (!dom.settingsLayer) return;
    state.previousFocus = document.activeElement;
    dom.settingsLayer.hidden = false;
    document.body.classList.add('modal-open');
    dom.settingsModal?.focus();
    loadSettings();
  }

  function closeSettings() {
    if (!dom.settingsLayer) return;
    dom.settingsLayer.hidden = true;
    document.body.classList.remove('modal-open');
    if (state.previousFocus instanceof HTMLElement) state.previousFocus.focus();
  }

  function connectWebSocket() {
    if (state.wsStopped || state.ws || window.location.protocol === 'file:') return;
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    let socket;
    try {
      socket = new WebSocket(`${protocol}//${window.location.host}/ws`);
    } catch (_) {
      return;
    }
    state.ws = socket;
    socket.addEventListener('open', () => {
      if (!state.wsConnectedOnce) appendLog('实时事件通道已连接。', '系统');
      state.wsConnectedOnce = true;
    });
    socket.addEventListener('message', (event) => {
      let payload;
      try {
        payload = JSON.parse(event.data);
      } catch (_) {
        return;
      }
      const eventMessage = payload?.message || payload?.data?.message;
      const eventType = payload?.type || '事件';
      if (eventMessage) appendLog(eventMessage, eventType, /error|fail/i.test(eventType) ? 'error' : 'info', payload?.timestamp || Date.now());
      refreshStatus({ quiet: true });
    });
    socket.addEventListener('close', () => {
      state.ws = null;
      if (!state.wsStopped && state.wsConnectedOnce) {
        window.clearTimeout(state.wsRetryTimer);
        state.wsRetryTimer = window.setTimeout(connectWebSocket, 5000);
      }
    });
    socket.addEventListener('error', () => {
      socket.close();
    });
  }

  function bindEvents() {
    dom.modeTabs.forEach((tab) => tab.addEventListener('click', () => selectMode(tab.dataset.mode, true)));
    dom.interfaceSelect?.addEventListener('change', () => {
      state.selectedInterface = dom.interfaceSelect.value;
      renderReadiness();
      renderFacts();
      renderControls();
    });
    dom.profileSelect?.addEventListener('change', () => {
      state.selectedProfile = dom.profileSelect.value;
      renderReadiness();
      renderControls();
    });
    dom.profileRefreshBtn?.addEventListener('click', () => refreshHomeNetwork());
    dom.connectionAction?.addEventListener('click', handleConnectionAction);
    dom.recoveryAction?.addEventListener('click', handleRecovery);
    dom.clearLogBtn?.addEventListener('click', clearLog);
    dom.settingsOpenBtn?.addEventListener('click', openSettings);
    dom.topSettingsBtn?.addEventListener('click', openSettings);
    dom.settingsCloseBtn?.addEventListener('click', closeSettings);
    dom.settingsCancelBtn?.addEventListener('click', closeSettings);
    dom.settingsSaveBtn?.addEventListener('click', saveSettings);
    dom.settingsLayer?.addEventListener('click', (event) => {
      if (event.target?.dataset?.closeSettings === 'true') closeSettings();
    });
    document.addEventListener('keydown', (event) => {
      if (event.key === 'Escape' && dom.settingsLayer && !dom.settingsLayer.hidden) closeSettings();
      if (event.key === 'Escape' && dom.sidebar?.classList.contains('is-open')) closeSidebar();
    });
    dom.mobileMenuBtn?.addEventListener('click', () => {
      const open = !dom.sidebar?.classList.contains('is-open');
      dom.sidebar?.classList.toggle('is-open', open);
      dom.mobileMenuBtn.setAttribute('aria-expanded', open ? 'true' : 'false');
    });
    dom.navItems.forEach((item) => item.addEventListener('click', () => {
      dom.navItems.forEach((nav) => nav.classList.toggle('is-active', nav === item));
      closeSidebar();
    }));
    window.addEventListener('beforeunload', () => {
      state.wsStopped = true;
      window.clearTimeout(state.wsRetryTimer);
      state.ws?.close();
    });
  }

  function closeSidebar() {
    dom.sidebar?.classList.remove('is-open');
    dom.mobileMenuBtn?.setAttribute('aria-expanded', 'false');
  }

  async function init() {
    bindEvents();
    renderModeTabs();
    renderReadiness();
    renderFacts();
    appendLog('正在读取本机网络状态…', '系统');
    await refreshStatus();
    await refreshHomeNetwork({ quiet: true });
    state.pollTimer = window.setInterval(() => refreshStatus({ quiet: true }), 5000);
    // Polling is the reliable baseline for a local preview and keeps the page quiet
    // when an optional WebSocket endpoint is unavailable.
  }

  init().catch((error) => {
    renderOffline(error);
    appendLog(error instanceof Error ? error.message : '页面初始化失败。', '系统', 'error');
  });
})();
