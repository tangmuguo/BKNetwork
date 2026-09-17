const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const appSource = fs.readFileSync(path.join(__dirname, '..', 'web', 'app.js'), 'utf8');
const startupMarker = '\nsetAdvancedMode(false);';
const startupOffset = appSource.indexOf(startupMarker);
assert(startupOffset > 0, 'app.js startup marker not found');
const appWithoutStartup = appSource.slice(0, startupOffset);

class MockElement {
  constructor(tagName = 'div') {
    this.tagName = tagName.toUpperCase();
    this.textContent = '';
    this.className = '';
    this.innerHTML = '';
    this.value = '';
    this.checked = false;
    this.disabled = false;
    this.dataset = {};
    this.style = {};
    this.children = [];
    this.attributes = {};
    this.classList = {
      add: (...names) => names.forEach(name => this.classList._names.add(name)),
      remove: (...names) => names.forEach(name => this.classList._names.delete(name)),
      toggle: (name, force) => {
        const next = force === undefined ? !this.classList._names.has(name) : !!force;
        if (next) this.classList._names.add(name);
        else this.classList._names.delete(name);
        return next;
      },
      contains: name => this.classList._names.has(name),
      _names: new Set(),
    };
  }

  addEventListener() {}

  setAttribute(name, value) {
    this.attributes[name] = String(value);
  }

  appendChild(child) {
    this.children.push(child);
    return child;
  }

  remove() {}

  contains() {
    return false;
  }
}

function createHarness() {
  const elements = new Map();
  const getElement = id => {
    if (!elements.has(id)) elements.set(id, new MockElement());
    return elements.get(id);
  };
  const document = {
    activeElement: null,
    body: new MockElement('body'),
    head: new MockElement('head'),
    getElementById: getElement,
    querySelector(selector) {
      if (selector === '.lead') {
        const lead = getElement('__lead');
        lead.textContent = 'v2.0.2';
        return lead;
      }
      return getElement(`query:${selector}`);
    },
    createElement: tagName => new MockElement(tagName),
    createDocumentFragment: () => new MockElement('fragment'),
  };
  const localStorage = new Map();
  const window = {
    localStorage: {
      getItem: key => localStorage.get(key) || null,
      setItem: (key, value) => localStorage.set(key, String(value)),
    },
    setTimeout,
    clearTimeout,
    setInterval,
    clearInterval,
    addEventListener() {},
  };
  window.window = window;

  let fetchImpl = async () => {
    throw new Error('unexpected fetch');
  };
  const sandbox = {
    console,
    document,
    window,
    location: { protocol: 'http:', host: 'localhost:13335' },
    WebSocket: class MockWebSocket {},
    fetch: (...args) => fetchImpl(...args),
    AbortController,
    setTimeout,
    clearTimeout,
    setInterval,
    clearInterval,
  };
  sandbox.WebSocket.CONNECTING = 0;
  sandbox.WebSocket.OPEN = 1;
  vm.createContext(sandbox);
  vm.runInContext(appWithoutStartup, sandbox, { filename: 'web/app.js' });

  return {
    app: sandbox,
    elements,
    warpSnapshot() {
      return JSON.parse(vm.runInContext('JSON.stringify(latestNetwork?.warp || null)', sandbox));
    },
    setFetch(next) {
      fetchImpl = next;
    },
  };
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function response(data, ok = true) {
  return { ok, json: async () => data };
}

function flush() {
  return new Promise(resolve => setImmediate(resolve));
}

function network(online, warp = { connected: false, status: 'Disconnected' }) {
  return {
    online,
    adapters: [{
      name: 'WiFi',
      status: 'Up',
      description: 'test',
      macAddress: '00:00:00:00:00:00',
      ipv4Enabled: true,
      ipv6Enabled: true,
      dns: [],
      ipv4: [],
      ipv6: [],
    }],
    availableAdapters: ['WiFi'],
    freeFlowMode: { active: false, mode: '' },
    warp: { ...warp },
    warpSettings: {},
    homeNetwork: {},
  };
}

async function testIpv6AddressTimeoutUsesTwoSeconds() {
  const harness = createHarness();
  let timeoutMs;
  let timeoutCallback;
  let signal;
  let cleared;
  harness.app.window.setTimeout = (callback, delay) => {
    timeoutCallback = callback;
    timeoutMs = delay;
    return 123;
  };
  harness.app.clearTimeout = timer => { cleared = timer; };
  harness.setFetch((url, options) => {
    assert.equal(url, 'https://api-ipv6.ip.sb/ip');
    signal = options.signal;
    return new Promise((resolve, reject) => {
      signal.addEventListener('abort', () => reject(new Error('aborted')), { once: true });
    });
  });

  const pending = harness.app.fetchIpv6AddressOnce();
  assert.equal(timeoutMs, 2000);
  assert.equal(signal.aborted, false);
  timeoutCallback();
  await assert.rejects(pending, /aborted/);
  assert.equal(signal.aborted, true, 'deadline must abort the actual fetch signal');
  assert.equal(cleared, 123, 'the deadline timer must be cleared after failure');
}

const invalidWarpStatuses = [
  { connected: false, status: '', error: 'warp-cli timeout' },
  { connected: false, status: 'Disconnected', error: 'command failed with output' },
  { connected: true, status: 'Connected', error: 'query failed' },
  { connected: false, status: '' },
  { connected: false, status: '  ', reason: 'query unavailable' },
  { connected: false, reason: 'missing status' },
  { status: 'Disconnected' },
  { connected: 'false', status: 'Disconnected' },
  {},
  null,
];

function connectedWarp() {
  return {
    connected: true,
    status: 'Connected',
    checkedAt: '2026-09-17T00:00:00Z',
    underlay: { ok: true },
  };
}

function assertConnectedWarp(harness, expected) {
  assert.equal(harness.elements.get('warpToggle').checked, true);
  assert.match(harness.elements.get('warpState').textContent, /已开启/);
  assert.deepEqual(harness.warpSnapshot(), expected, 'the full valid WARP snapshot must survive');
}

async function testFailedSnapshotKeepsPreviousWarpState() {
  const harness = createHarness();
  const previousWarp = connectedWarp();
  harness.app.renderStatus({ network: network(true, previousWarp) });
  assertConnectedWarp(harness, previousWarp);

  for (const invalid of invalidWarpStatuses) {
    harness.setFetch(async () => response({ network: network(false, invalid) }));
    await harness.app.refreshStatus(false);
    assertConnectedWarp(harness, previousWarp);
  }
  const failures = [
    async () => response({ network: network(false) }, false),
    async () => { throw new Error('offline'); },
    async () => ({ ok: true, json: async () => { throw new Error('invalid JSON'); } }),
    async () => response({}),
  ];
  for (const failure of failures) {
    harness.setFetch(failure);
    await harness.app.refreshStatus(false);
    assertConnectedWarp(harness, previousWarp);
  }

  harness.setFetch(async () => response({ network: network(true) }));
  await harness.app.refreshStatus(false);
  assert.equal(harness.elements.get('warpToggle').checked, false, 'a confirmed disconnect must replace the retained connection');
  assert.equal(harness.warpSnapshot().status, 'Disconnected');
  harness.app.renderStatus({ network: network(true, invalidWarpStatuses[0]) });
  assert.equal(harness.elements.get('warpToggle').checked, false);
  assert.equal(harness.warpSnapshot().status, 'Disconnected', 'failed reads must preserve a valid disconnected state too');
}

function captureWarpPoll(harness) {
  const timers = [];
  harness.app.setTimeout = (callback, delay) => {
    timers.push({ callback, delay });
    return timers.length;
  };
  // Failures below are deliberate; keep their expected diagnostics out of TAP.
  harness.app.console = { ...console, error() {} };
  harness.app.startWarpStatusPoll();
  assert.equal(timers[0].delay, 1000);
  return async () => {
    const timer = timers.shift();
    assert.ok(timer, 'the poll must remain scheduled after failures');
    await timer.callback();
    assert.equal(timers[0].delay, 3000, 'the polling cadence must stay unchanged');
  };
}

async function testWarpStatusPollKeepsStateOnInvalidResponse() {
  const harness = createHarness();
  const previousWarp = connectedWarp();
  harness.app.renderStatus({ network: network(true, previousWarp) });
  const poll = captureWarpPoll(harness);
  const responses = invalidWarpStatuses.map(status => async () => response(status));
  responses.push(
    async () => response({ connected: false, status: 'Disconnected' }, false),
    async () => { throw new Error('offline'); },
    async () => ({ ok: true, json: async () => { throw new Error('invalid JSON'); } }),
  );
  for (const fetchResponse of responses) {
    harness.setFetch(async url => {
      assert.equal(url, '/api/v1/warp-status');
      return fetchResponse();
    });
    await poll();
    assertConnectedWarp(harness, previousWarp);
  }

  harness.setFetch(async url => {
    if (url === '/api/v1/warp-status') {
      return response({ connected: false, status: 'Disconnected', reason: 'Manual' });
    }
    assert.equal(url, '/api/v1/status');
    throw new Error('full refresh unavailable');
  });
  await poll();
  assert.equal(harness.elements.get('warpToggle').checked, false, 'a valid poll must update the UI even if its follow-up refresh fails');
  assert.equal(harness.warpSnapshot().status, 'Disconnected');
  assert.equal(harness.warpSnapshot().reason, 'Manual');
}

async function testSuccessfulPollBeforeSnapshotIsRetained() {
  const harness = createHarness();
  const poll = captureWarpPoll(harness);
  harness.setFetch(async url => {
    if (url === '/api/v1/warp-status') {
      return response(connectedWarp());
    }
    assert.equal(url, '/api/v1/status');
    throw new Error('initial snapshot unavailable');
  });
  await poll();
  assertConnectedWarp(harness, connectedWarp());
  harness.app.renderStatus({ network: network(false, invalidWarpStatuses[0]) });
  assertConnectedWarp(harness, connectedWarp());
}

async function testForcedStatusRefreshWaitsForFreshData() {
  const harness = createHarness();
  const requests = [];
  harness.setFetch((url) => {
    assert.equal(url, '/api/v1/status');
    const request = deferred();
    requests.push(request);
    return request.promise;
  });

  const first = harness.app.refreshStatus(false);
  await Promise.resolve();
  assert.equal(requests.length, 1);
  const overlap = harness.app.refreshStatus(false);
  assert.strictEqual(overlap, first, 'ordinary overlapping refreshes should share one request');
  const forced = harness.app.refreshStatus(true);
  const forcedAgain = harness.app.refreshStatus(true);
  assert.strictEqual(forcedAgain, forced, 'overlapping force refreshes should share one follow-up');
  assert.notStrictEqual(forced, first);
  assert.equal(requests.length, 1, 'force should wait for the active request');

  requests[0].resolve(response({ admin: true, network: network(false) }));
  await flush();
  assert.equal(requests.length, 2, 'force should schedule one follow-up request');
  requests[1].resolve(response({ admin: true, network: network(true) }));
  await forced;
  assert.equal(harness.elements.get('networkDot').className, 'dot ok');
}

async function testFailedStatusRequestCanRecover() {
  const harness = createHarness();
  const requests = [];
  harness.setFetch(() => {
    const request = deferred();
    requests.push(request);
    return request.promise;
  });

  const failed = harness.app.refreshStatus(false);
  await Promise.resolve();
  requests[0].reject(new Error('offline'));
  assert.equal(await failed, null);

  const retry = harness.app.refreshStatus(false);
  await flush();
  assert.equal(requests.length, 2, 'a failed request must not poison later refreshes');
  requests[1].resolve(response({ admin: true, network: network(true) }));
  assert.deepEqual(await retry, { admin: true, network: network(true) });
}

async function testForcedHomeRefreshWaitsForMutationResult() {
  const harness = createHarness();
  const requests = [];
  harness.setFetch((url) => {
    assert.equal(url, '/api/v1/home-network');
    const request = deferred();
    requests.push(request);
    return request.promise;
  });

  const first = harness.app.loadHomeNetwork(false);
  await Promise.resolve();
  const forced = harness.app.loadHomeNetwork(true);
  assert.equal(requests.length, 1);
  requests[0].resolve(response({ profiles: [], status: { installed: true, running: false } }));
  await flush();
  assert.equal(requests.length, 2);
  requests[1].resolve(response({ profiles: ['home'], tunnelName: 'home', status: { installed: true, running: true, connected: true } }));
  await first;
  const latest = await forced;
  assert.equal(latest.tunnelName, 'home');
}

(async () => {
  await testIpv6AddressTimeoutUsesTwoSeconds();
  await testFailedSnapshotKeepsPreviousWarpState();
  await testWarpStatusPollKeepsStateOnInvalidResponse();
  await testSuccessfulPollBeforeSnapshotIsRetained();
  await testForcedStatusRefreshWaitsForFreshData();
  await testFailedStatusRequestCanRecover();
  await testForcedHomeRefreshWaitsForMutationResult();
  console.log('app.js request overlap, WARP state retention and IPv6 timeout tests passed');
})().catch(error => {
  console.error(error);
  process.exitCode = 1;
});
