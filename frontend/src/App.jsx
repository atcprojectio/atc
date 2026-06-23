import React, { useState, useEffect } from 'react';
import './App.css';

const getLocalStorageItem = (key) => {
  if (typeof window !== 'undefined' && window.localStorage) {
    return window.localStorage.getItem(key) || '';
  }
  return '';
};

const setLocalStorageItem = (key, val) => {
  if (typeof window !== 'undefined' && window.localStorage) {
    window.localStorage.setItem(key, val);
  }
};

const removeLocalStorageItem = (key) => {
  if (typeof window !== 'undefined' && window.localStorage) {
    window.localStorage.removeItem(key);
  }
};

function App() {
  const [services, setServices] = useState([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(null);
  const [searchTerm, setSearchTerm] = useState('');
  const [autoRefresh, setAutoRefresh] = useState(true);
  const [countdown, setCountdown] = useState(10);
  const [lastUpdated, setLastUpdated] = useState(null);
  const [isLeader, setIsLeader] = useState(true);
  const [federation, setFederation] = useState({});
  const [token, setToken] = useState(getLocalStorageItem('atc-api-token'));
  const [showToken, setShowToken] = useState(false);
  const [activeTab, setActiveTab] = useState('services');
  const [strategies, setStrategies] = useState({ failover: {}, redirect: {} });
  const [modules, setModules] = useState([]);
  const [authEnabled, setAuthEnabled] = useState(!!getLocalStorageItem('atc-api-token'));
  const [activeOverrideForm, setActiveOverrideForm] = useState(null);
  const [overrideTargetDc, setOverrideTargetDc] = useState('');
  const [overrideNamespace, setOverrideNamespace] = useState('');
  const [overrideType, setOverrideType] = useState('failover');
  const [overrideDuration, setOverrideDuration] = useState('');

  const getHeaders = (t) => {
    const h = {};
    if (t) {
      h['Authorization'] = `Bearer ${t}`;
    }
    return h;
  };

  const handleTokenChange = (val) => {
    setToken(val);
    setLocalStorageItem('atc-api-token', val);
    setError(null);
    fetchServices(true, val);
  };

  const handleClearToken = () => {
    setToken('');
    removeLocalStorageItem('atc-api-token');
    setError(null);
    fetchServices(true, '');
  };

  const fetchServices = async (showLoading = true, currentToken = token) => {
    try {
      if (showLoading) {
        setLoading(true);
      }
      
      const headers = getHeaders(currentToken);
 
      try {
        const leaderRes = await fetch('/api/leader', { headers });
        if (leaderRes.status === 401 || leaderRes.status === 403) {
          setAuthEnabled(true);
          setError('Authentication failed. Please check your API token.');
          setAutoRefresh(false);
          setLoading(false);
          return;
        }
        if (leaderRes.ok) {
          const leaderData = await leaderRes.json();
          setIsLeader(leaderData.leader);
          setAuthEnabled(!!leaderData.auth_enabled);
        }
      } catch (err) {
        console.error("Failed to fetch leader status:", err);
      }
 
      // Fetch federation status
      try {
        const fedRes = await fetch('/api/federation', { headers });
        if (fedRes.status === 401 || fedRes.status === 403) {
          setAuthEnabled(true);
          setError('Authentication failed. Please check your API token.');
          setAutoRefresh(false);
          setLoading(false);
          return;
        }
        if (fedRes.ok) {
          const fedData = await fedRes.json();
          const fedMap = {};
          fedData.forEach(item => {
            fedMap[item.datacenter] = item.status;
          });
          setFederation(fedMap);
        }
      } catch (err) {
        console.error("Failed to fetch federation status:", err);
      }
 
      // Fetch predefined strategies
      try {
        const stratRes = await fetch('/api/strategies', { headers });
        if (stratRes.status === 401 || stratRes.status === 403) {
          setAuthEnabled(true);
          setError('Authentication failed. Please check your API token.');
          setAutoRefresh(false);
          setLoading(false);
          return;
        }
        if (stratRes.ok) {
          const stratData = await stratRes.json();
          setStrategies(stratData || { failover: {}, redirect: {} });
        }
      } catch (err) {
        console.error("Failed to fetch predefined strategies:", err);
      }
 
      // Fetch enabled modules
      try {
        const modulesRes = await fetch('/api/modules', { headers });
        if (modulesRes.status === 401 || modulesRes.status === 403) {
          setAuthEnabled(true);
          setError('Authentication failed. Please check your API token.');
          setAutoRefresh(false);
          setLoading(false);
          return;
        }
        if (modulesRes.ok) {
          const modulesData = await modulesRes.json();
          setModules(modulesData || []);
        }
      } catch (err) {
        console.error("Failed to fetch active modules:", err);
      }
 
      const res = await fetch('/api/services', { headers });
      if (res.status === 401 || res.status === 403) {
        setAuthEnabled(true);
        setError('Authentication failed. Please check your API token.');
        setAutoRefresh(false);
        setLoading(false);
        return;
      }
      if (!res.ok) {
        throw new Error(`Failed to fetch services: ${res.statusText}`);
      }
      const data = await res.json();
      setServices(data || []);
      setError(null);
      setLastUpdated(new Date().toLocaleTimeString());
    } catch (err) {
      setError(err.message);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    Promise.resolve().then(() => {
      fetchServices(false);
    });
  }, []);

  useEffect(() => {
    if (!autoRefresh) return;
    Promise.resolve().then(() => {
      setCountdown(10);
    });

    const timer = setInterval(() => {
      setCountdown((prev) => {
        if (prev <= 1) {
          fetchServices();
          return 10;
        }
        return prev - 1;
      });
    }, 1000);

    return () => clearInterval(timer);
  }, [autoRefresh]);

  const formatExpiry = (expiresAtStr) => {
    if (!expiresAtStr) return 'Never';
    if (expiresAtStr === 'never') return 'Never';
    const exp = new Date(expiresAtStr);
    if (isNaN(exp.getTime())) return 'Never';
    const now = new Date();
    const diffMs = exp - now;
    if (diffMs <= 0) return 'Expired';
    
    const diffMins = Math.ceil(diffMs / 60000);
    if (diffMins < 60) return `Expires in ${diffMins}m`;
    const diffHours = Math.floor(diffMins / 60);
    const remMins = diffMins % 60;
    if (diffHours < 24) return `Expires in ${diffHours}h ${remMins}m`;
    const diffDays = Math.floor(diffHours / 24);
    return `Expires in ${diffDays}d`;
  };


  const handleApplyOverride = async (serviceName) => {
    if (!overrideTargetDc) {
      alert('Please specify a target datacenter.');
      return;
    }
    try {
      setLoading(true);
      const res = await fetch('/api/overrides', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          ...getHeaders(token),
        },
        body: JSON.stringify({
          service: serviceName,
          type: overrideType,
          target_dc: overrideTargetDc,
          namespace: overrideNamespace,
          duration: overrideDuration,
        }),
      });
      if (res.status === 401 || res.status === 403) {
        throw new Error('Authentication failed. Check your API token.');
      }
      if (!res.ok) {
        const text = await res.text();
        throw new Error(text || `Failed to apply override: ${res.statusText}`);
      }
      setActiveOverrideForm(null);
      setOverrideTargetDc('');
      setOverrideNamespace('');
      setOverrideDuration('');
      fetchServices();
    } catch (err) {
      alert(err.message);
    } finally {
      setLoading(false);
    }
  };

  const handleManualRefresh = () => {
    fetchServices();
    if (autoRefresh) {
      setCountdown(10);
    }
  };

  const handlePurge = async (name) => {
    if (!window.confirm(`Are you sure you want to purge the Consul config entry for "${name}"?`)) {
      return;
    }
    try {
      const res = await fetch(`/api/services?name=${encodeURIComponent(name)}`, {
        method: 'DELETE',
        headers: getHeaders(token),
      });
      if (res.status === 401 || res.status === 403) {
        throw new Error('Authentication failed. Check your API token.');
      }
      if (!res.ok) {
        const text = await res.text();
        throw new Error(text || `Failed to purge resolver: ${res.statusText}`);
      }
      fetchServices();
    } catch (err) {
      alert(err.message);
    }
  };

  const renderDatacenterNode = (service, dc, isRedirect = false) => {
    const status = federation[dc];
    let badgeClass = 'fed-unknown';
    let statusText = 'Unfederated';
    let indicatorText = '⚠️';

    if (status === 'alive') {
      badgeClass = 'fed-alive';
      statusText = 'WAN Federated';
      indicatorText = '●';
    } else if (status === 'failed') {
      badgeClass = 'fed-failed';
      statusText = 'WAN Failed';
      indicatorText = '▲';
    }

    return (
      <div className={`failover-node target ${isRedirect ? 'redirect-node' : ''} ${badgeClass}`} title={`${dc}: ${statusText}`}>
        <span className="fed-indicator">{indicatorText}</span> {service} ({dc})
      </div>
    );
  };

  const filteredServices = services.filter((svc) =>
    svc.name.toLowerCase().includes(searchTerm.toLowerCase()) ||
    svc.tags.some((tag) => tag.toLowerCase().includes(searchTerm.toLowerCase()))
  );

  return (
    <div className="app-container">
      {/* Top Background Glows */}
      <div className="glow glow-1"></div>
      <div className="glow glow-2"></div>

      <header className="app-header">
        <div className="logo-container">
          <div className="logo-pulse"></div>
          <h1 className="logo-title">ATC</h1>
          <span className={`leader-badge ${isLeader ? 'active' : 'standby'}`}>
            {isLeader ? '● ACTIVE LEADER' : '○ STANDBY'}
          </span>
          {modules.length > 0 && (
            <div className="modules-badges-list">
              {modules.map(mod => (
                <span key={mod} className="module-badge" title={`Module ${mod} is active`}>
                  📦 {mod}
                </span>
              ))}
            </div>
          )}
        </div>


        {authEnabled && (
          <div className="auth-box glass-panel">
            <span className="auth-icon">🔑</span>
            <input
              type={showToken ? "text" : "password"}
              placeholder="Enter API Token..."
              value={token}
              onChange={(e) => handleTokenChange(e.target.value)}
              className="auth-input"
            />
            <button 
              type="button" 
              onClick={() => setShowToken(!showToken)} 
              className="btn-show-token"
              title={showToken ? "Hide token" : "Show token"}
            >
              {showToken ? '👁️' : '🔒'}
            </button>
            {token && (
              <button onClick={handleClearToken} className="btn-clear-token" title="Clear token">
                ✕
              </button>
            )}
          </div>
        )}
      </header>

      <main className="main-content">
        <div className="tabs-container">
          <button
            onClick={() => setActiveTab('services')}
            className={`tab-btn ${activeTab === 'services' ? 'active' : ''}`}
          >
            📋 Services ({filteredServices.length})
          </button>
          <button
            onClick={() => setActiveTab('strategies')}
            className={`tab-btn ${activeTab === 'strategies' ? 'active' : ''}`}
          >
            ⚡ Predefined Strategies
          </button>
        </div>

        {activeTab === 'services' ? (
          <>
            {/* Controls Panel */}
            <section className="glass-panel controls-panel">
              <div className="search-box">
                <span className="search-icon">🔍</span>
                <input
                  type="text"
                  placeholder="Search services or tags..."
                  value={searchTerm}
                  onChange={(e) => setSearchTerm(e.target.value)}
                  className="search-input"
                />
              </div>

              <div className="refresh-controls">
                {autoRefresh && (
                  <span className="countdown-badge">
                    Auto-sync in <strong>{countdown}s</strong>
                  </span>
                )}
                <button
                  onClick={() => setAutoRefresh(!autoRefresh)}
                  className={`btn btn-toggle ${autoRefresh ? 'active' : ''}`}
                >
                  {autoRefresh ? '⏸ Pause Auto-Sync' : '▶ Enable Auto-Sync'}
                </button>
                <button
                  onClick={handleManualRefresh}
                  className={`btn btn-refresh ${loading ? 'spinning' : ''}`}
                  disabled={loading}
                >
                  🔄 Refresh
                </button>
              </div>
            </section>

            {/* Stats Panel */}
            <section className="stats-grid">
              <div className="stat-card glass-panel">
                <span className="stat-icon">📦</span>
                <div className="stat-info">
                  <h3>Services Monitored</h3>
                  <p className="stat-value">{services.length}</p>
                </div>
              </div>
              <div className="stat-card glass-panel">
                <span className="stat-icon">🔄</span>
                <div className="stat-info">
                  <h3>Service Resolvers Active</h3>
                  <p className="stat-value">
                    {services.filter((s) => s.resolver_type && s.resolver_type !== 'none').length}
                  </p>
                </div>
              </div>
              <div className="stat-card glass-panel">
                <span className="stat-icon">📡</span>
                <div className="stat-info">
                  <h3>Last Synced</h3>
                  <p className="stat-value text-sm">{lastUpdated || 'Never'}</p>
                </div>
              </div>
            </section>

            {/* Error State */}
            {error && (
              <div className="error-banner glass-panel">
                <span className="error-icon">⚠️</span>
                <div className="error-message">
                  <h4>Consul Connection Error</h4>
                  <p>{error}</p>
                </div>
                <button onClick={handleManualRefresh} className="btn btn-retry">
                  Retry
                </button>
              </div>
            )}

            {/* Services List */}
            <section className="services-section">
              <div className="section-header">
                <h2>Tracked Services ({filteredServices.length})</h2>
                {loading && <span className="loader-inline">Syncing catalog...</span>}
              </div>

              {filteredServices.length === 0 ? (
                <div className="no-services glass-panel">
                  <span className="empty-icon">📭</span>
                  <h3>No Services Found</h3>
                  <p>
                    {searchTerm
                      ? 'No services match your search query.'
                      : 'No active services in Consul with tag "atc.enabled=true" found.'}
                  </p>
                </div>
              ) : (
                <div className="services-grid">
                  {filteredServices.map((svc) => (
                    <div key={svc.name} className={`service-card glass-panel ${svc.status === 'deleted' ? 'offline-card' : ''}`}>
                      <div className="card-glow"></div>
                      <div className="service-header">
                        <span className={`status-indicator ${svc.status === 'deleted' ? 'deleted' : 'active'}`}></span>
                        <div className="service-title-wrapper" style={{ display: 'flex', alignItems: 'center', gap: '8px', flexWrap: 'wrap' }}>
                          <h3 className="service-name" style={{ margin: 0 }}>{svc.name}</h3>
                          {svc.namespace && svc.namespace !== 'default' && (
                            <span className="namespace-badge" title={`Consul Namespace: ${svc.namespace}`}>
                              📁 {svc.namespace}
                            </span>
                          )}
                        </div>
                        {svc.meta && svc.meta['created-by'] === 'atc-override' && (
                          <>
                            <span className="override-badge active" title={`Expires: ${svc.meta['atc-override-expires-at'] || 'Never'}`}>
                              Bypassed ({formatExpiry(svc.meta['atc-override-expires-at'])})
                            </span>
                            <button
                              onClick={() => handlePurge(svc.name)}
                              className="btn-purge btn-remove-override"
                              title="Remove manual override and resume automatic routing"
                            >
                              ✕ Clear
                            </button>
                          </>
                        )}
                        {svc.status === 'deleted' && (!svc.meta || svc.meta['created-by'] !== 'atc-override') && (
                          <>
                            <span className="deleted-badge">Offline (Redirecting)</span>
                            <button
                              onClick={() => handlePurge(svc.name)}
                              className="btn-purge"
                              title="Purge resolver config entry from Consul"
                            >
                              🗑️ Purge
                            </button>
                          </>
                        )}
                      </div>

                      <div className="service-body">
                        <div className="detail-item">
                          <span className="detail-label">Resolver Type</span>
                          <span className={`resolver-badge ${svc.resolver_type || 'none'}`}>
                            {svc.resolver_type ? svc.resolver_type.toUpperCase() : 'UNKNOWN'}
                          </span>
                        </div>

                        {svc.resolver_type === 'failover' && svc.failover_strategy && (
                          <div className="detail-item">
                            <span className="detail-label">Failover Strategy</span>
                            <span className="strategy-badge failover-strategy">
                              {svc.failover_strategy}
                            </span>
                          </div>
                        )}

                        {svc.resolver_type === 'redirect' && svc.redirect_strategy && (
                          <div className="detail-item">
                            <span className="detail-label">Redirect Strategy</span>
                            <span className="strategy-badge redirect-strategy">
                              {svc.redirect_strategy}
                            </span>
                          </div>
                        )}

                        <div className="failover-visualization">
                          {svc.resolver_type === 'redirect' ? (
                            <>
                              <div className="failover-node current">Local</div>
                              <div className="failover-arrow">➔</div>
                              {svc.redirect_target ? (
                                renderDatacenterNode(svc.redirect_target.service, svc.redirect_target.datacenter, true)
                              ) : (
                                <div className="failover-node target redirect-node">Remote DC</div>
                              )}
                            </>
                          ) : svc.resolver_type === 'failover' ? (
                            <>
                              <div className="failover-node current">Local</div>
                              {svc.failover_targets && svc.failover_targets.length > 0 ? (
                                svc.failover_targets.map((target, idx) => (
                                  <React.Fragment key={idx}>
                                    <div className="failover-arrow">➔</div>
                                    {renderDatacenterNode(target.service, target.datacenter)}
                                  </React.Fragment>
                                ))
                              ) : (
                                <>
                                  <div className="failover-arrow">➔</div>
                                  <div className="failover-node target">Remote DC</div>
                                </>
                              )}
                            </>
                          ) : (
                            <div className="failover-node current">Local (Standalone)</div>
                          )}
                        </div>

                        <div className="detail-item tags-container">
                          <span className="detail-label">Consul Tags</span>
                          <div className="tags-list">
                            {svc.tags.map((tag) => (
                              <span
                                key={tag}
                                className={`tag-badge ${
                                  tag === 'atc.enabled=true' ? 'atc-tag' : ''
                                }`}
                              >
                                {tag}
                              </span>
                            ))}
                          </div>
                        </div>

                        {/* Manual Override controls */}
                        <div className="override-controls-section">
                          {activeOverrideForm === svc.name ? (
                            <div className="override-form glass-panel">
                              <h4>Manual Override Controls</h4>
                              <div className="form-group">
                                <label>Override Type</label>
                                <select 
                                  value={overrideType} 
                                  onChange={(e) => setOverrideType(e.target.value)}
                                  className="form-select"
                                >
                                  <option value="failover">Failover</option>
                                  <option value="redirect">Redirect</option>
                                </select>
                              </div>
                              <div className="form-group">
                                <label>Target Datacenter</label>
                                {Object.keys(federation).length > 0 ? (
                                  <select
                                    value={overrideTargetDc}
                                    onChange={(e) => setOverrideTargetDc(e.target.value)}
                                    className="form-select"
                                  >
                                    <option value="">-- Select Datacenter --</option>
                                    {Object.keys(federation).map(dc => (
                                      <option key={dc} value={dc}>{dc}</option>
                                    ))}
                                  </select>
                                ) : (
                                  <input
                                    type="text"
                                    placeholder="e.g. dc2"
                                    value={overrideTargetDc}
                                    onChange={(e) => setOverrideTargetDc(e.target.value)}
                                    className="form-input"
                                  />
                                )}
                              </div>
                              <div className="form-group">
                                <label>Target Namespace (Optional)</label>
                                <input
                                  type="text"
                                  placeholder="e.g. default"
                                  value={overrideNamespace}
                                  onChange={(e) => setOverrideNamespace(e.target.value)}
                                  className="form-input"
                                  id="override-namespace-input"
                                />
                              </div>
                              <div className="form-group">
                                <label>Duration (TTL)</label>
                                <select
                                  value={overrideDuration}
                                  onChange={(e) => setOverrideDuration(e.target.value)}
                                  className="form-select"
                                >
                                  <option value="">Permanent</option>
                                  <option value="5m">5 Minutes</option>
                                  <option value="15m">15 Minutes</option>
                                  <option value="1h">1 Hour</option>
                                  <option value="24h">24 Hours</option>
                                </select>
                              </div>
                              <div className="form-buttons">
                                <button 
                                  onClick={() => handleApplyOverride(svc.name)} 
                                  className="btn btn-apply-override"
                                >
                                  Apply Override
                                </button>
                                <button 
                                  onClick={() => setActiveOverrideForm(null)} 
                                  className="btn btn-cancel-override"
                                >
                                  Cancel
                                </button>
                              </div>
                            </div>
                          ) : (
                            (!svc.meta || svc.meta['created-by'] !== 'atc-override') && (
                              <button
                                onClick={() => {
                                  setActiveOverrideForm(svc.name);
                                  setOverrideType(svc.resolver_type === 'redirect' ? 'redirect' : 'failover');
                                  setOverrideNamespace(svc.namespace || '');
                                  setOverrideTargetDc(
                                    svc.resolver_type === 'redirect' && svc.redirect_target 
                                      ? svc.redirect_target.datacenter 
                                      : (svc.failover_targets && svc.failover_targets.length > 0 ? svc.failover_targets[0].datacenter : '')
                                  );
                                }}
                                className="btn btn-override-trigger"
                              >
                                ⚡ Apply Manual Override
                              </button>
                            )
                          )}
                        </div>
                      </div>
                    </div>
                  ))}
                </div>
              )}
            </section>
          </>
        ) : (
          <section className="strategies-section">
            <div className="section-header">
              <h2>Predefined Strategies Templates</h2>
            </div>
            
            <div className="strategies-grid">
              {/* Failover strategies */}
              {strategies.failover && Object.keys(strategies.failover).length > 0 ? (
                Object.entries(strategies.failover).map(([name, config]) => (
                  <div key={name} className="service-card glass-panel">
                    <div className="card-glow"></div>
                    <div className="strategy-title-row">
                      <span className="resolver-badge failover">FAILOVER</span>
                      <h3 className="strategy-title">{name}</h3>
                    </div>
                    <div className="service-body">
                      <div className="strategy-info-item">
                        <span className="strategy-info-label">Connect Timeout</span>
                        <span className="strategy-info-value">{config.connect_timeout || 'default'}</span>
                      </div>
                      <div className="detail-item" style={{ marginTop: '15px' }}>
                        <span className="detail-label">Failover Targets</span>
                        <ul className="strategy-target-list">
                          {config.targets && config.targets.map((target, idx) => (
                            <li key={idx} className="strategy-target-card">
                              <div className="strategy-target-header">
                                <span>{target.service}</span>
                                <span>DC: {target.datacenter}</span>
                              </div>
                              {(target.namespace || target.service_subset) && (
                                <div className="strategy-target-details">
                                  {target.namespace && <span>NS: {target.namespace}</span>}
                                  {target.service_subset && <span>Subset: {target.service_subset}</span>}
                                </div>
                              )}
                            </li>
                          ))}
                        </ul>
                      </div>
                    </div>
                  </div>
                ))
              ) : null}

              {/* Redirect strategies */}
              {strategies.redirect && Object.keys(strategies.redirect).length > 0 ? (
                Object.entries(strategies.redirect).map(([name, config]) => (
                  <div key={name} className="service-card glass-panel">
                    <div className="card-glow"></div>
                    <div className="strategy-title-row">
                      <span className="resolver-badge redirect">REDIRECT</span>
                      <h3 className="strategy-title">{name}</h3>
                    </div>
                    <div className="service-body">
                      <div className="strategy-info-item">
                        <span className="strategy-info-label">Target Service</span>
                        <span className="strategy-info-value">{config.service}</span>
                      </div>
                      <div className="strategy-info-item">
                        <span className="strategy-info-label">Target Datacenter</span>
                        <span className="strategy-info-value">{config.datacenter}</span>
                      </div>
                      {config.namespace && (
                        <div className="strategy-info-item">
                          <span className="strategy-info-label">Namespace</span>
                          <span className="strategy-info-value">{config.namespace}</span>
                        </div>
                      )}
                      {config.service_subset && (
                        <div className="strategy-info-item">
                          <span className="strategy-info-label">Service Subset</span>
                          <span className="strategy-info-value">{config.service_subset}</span>
                        </div>
                      )}
                    </div>
                  </div>
                ))
              ) : null}

              {(!strategies.failover || Object.keys(strategies.failover).length === 0) &&
               (!strategies.redirect || Object.keys(strategies.redirect).length === 0) && (
                <div className="no-services glass-panel" style={{ gridColumn: '1 / -1' }}>
                  <span className="empty-icon">⚡</span>
                  <h3>No Predefined Strategies</h3>
                  <p>Configure predefined strategies under `strategies.failover` or `strategies.redirect` in the daemon configuration.</p>
                </div>
              )}
            </div>
          </section>
        )}
      </main>

      <footer className="app-footer">
        <p>ATC Dashboard • Running in local cluster mode</p>
      </footer>
    </div>
  );
}

export default App;
