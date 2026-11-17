import React, { useState, useEffect, useRef } from 'react';
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
  // Consul connectivity as reported by GET /api/status (null until the first answer).
  const [consulStatus, setConsulStatus] = useState(null);
  // False when the browser cannot reach the ATC server at all.
  const [atcReachable, setAtcReachable] = useState(true);
  const [leaderComponents, setLeaderComponents] = useState({ forwarder: false, redirector: false });
  const [leaderInfo, setLeaderInfo] = useState(null);
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
  const tokenDebounceRef = useRef(null);

  const getHeaders = (t) => {
    const h = {};
    if (t) {
      h['Authorization'] = `Bearer ${t}`;
    }
    return h;
  };

  useEffect(() => {
    return () => {
      if (tokenDebounceRef.current) {
        clearTimeout(tokenDebounceRef.current);
      }
    };
  }, []);

  const handleTokenChange = (val) => {
    setToken(val);
    setLocalStorageItem('atc-api-token', val);
    setError(null);
    if (tokenDebounceRef.current) {
      clearTimeout(tokenDebounceRef.current);
    }
    tokenDebounceRef.current = setTimeout(() => {
      tokenDebounceRef.current = null;
      fetchServices(true, val);
    }, 400);
  };

  const flushTokenSync = (val = token) => {
    if (tokenDebounceRef.current) {
      clearTimeout(tokenDebounceRef.current);
      tokenDebounceRef.current = null;
      fetchServices(true, val);
    }
  };

  const handleClearToken = () => {
    if (tokenDebounceRef.current) {
      clearTimeout(tokenDebounceRef.current);
      tokenDebounceRef.current = null;
    }
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

      const [
        statusOutcome,
        leaderOutcome,
        fedOutcome,
        stratOutcome,
        modulesOutcome,
        servicesOutcome,
      ] = await Promise.allSettled([
        fetch('/api/status', { headers }),
        fetch('/api/leader', { headers }),
        fetch('/api/federation', { headers }),
        fetch('/api/strategies', { headers }),
        fetch('/api/modules', { headers }),
        fetch('/api/services', { headers }),
      ]);

      // The status endpoint needs no token; if even it cannot be fetched, ATC itself is down
      // or unreachable from this browser.
      let consulDown = false;
      if (statusOutcome.status === 'rejected') {
        setAtcReachable(false);
        return;
      }
      setAtcReachable(true);
      if (statusOutcome.value && statusOutcome.value.ok) {
        try {
          const statusData = await statusOutcome.value.json();
          const consul = statusData && statusData.consul ? statusData.consul : null;
          setConsulStatus(consul);
          consulDown = !!consul && consul.reachable === false;
        } catch (err) {
          console.error('Failed to parse ATC status:', err);
        }
      }

      const outcomes = [
        leaderOutcome,
        fedOutcome,
        stratOutcome,
        modulesOutcome,
        servicesOutcome,
      ];

      for (const outcome of outcomes) {
        if (outcome.status === 'fulfilled' && outcome.value && (outcome.value.status === 401 || outcome.value.status === 403)) {
          setAuthEnabled(true);
          setError('Authentication failed. Please check your API token.');
          setAutoRefresh(false);
          return;
        }
      }

      if (leaderOutcome.status === 'fulfilled' && leaderOutcome.value.ok) {
        try {
          const leaderData = await leaderOutcome.value.json();
          setAuthEnabled(!!leaderData.auth_enabled);
          setLeaderComponents(leaderData.components || { forwarder: false, redirector: false });
          setLeaderInfo(leaderData);
        } catch (err) {
          console.error("Failed to parse leader status:", err);
        }
      } else if (leaderOutcome.status === 'rejected') {
        console.error("Failed to fetch leader status:", leaderOutcome.reason);
      }

      if (fedOutcome.status === 'fulfilled' && fedOutcome.value.ok) {
        try {
          const fedData = await fedOutcome.value.json();
          const fedMap = {};
          if (Array.isArray(fedData)) {
            fedData.forEach(item => {
              fedMap[item.datacenter] = item.status;
            });
          }
          setFederation(fedMap);
        } catch (err) {
          console.error("Failed to parse federation status:", err);
        }
      } else if (fedOutcome.status === 'rejected') {
        console.error("Failed to fetch federation status:", fedOutcome.reason);
      }

      if (stratOutcome.status === 'fulfilled' && stratOutcome.value.ok) {
        try {
          const stratData = await stratOutcome.value.json();
          setStrategies(stratData || { failover: {}, redirect: {} });
        } catch (err) {
          console.error("Failed to parse predefined strategies:", err);
        }
      } else if (stratOutcome.status === 'rejected') {
        console.error("Failed to fetch predefined strategies:", stratOutcome.reason);
      }

      if (modulesOutcome.status === 'fulfilled' && modulesOutcome.value.ok) {
        try {
          const modulesData = await modulesOutcome.value.json();
          setModules(modulesData || []);
        } catch (err) {
          console.error("Failed to parse active modules:", err);
        }
      } else if (modulesOutcome.status === 'rejected') {
        console.error("Failed to fetch active modules:", modulesOutcome.reason);
      }

      if (servicesOutcome.status === 'rejected') {
        throw servicesOutcome.reason;
      }

      const res = servicesOutcome.value;
      if (!res.ok) {
        if (consulDown) {
          // The Consul outage banner explains this; keep showing the last known services.
          setError(null);
          return;
        }
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
    // eslint-disable-next-line react-hooks/exhaustive-deps
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
    // eslint-disable-next-line react-hooks/exhaustive-deps
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


  const handleApplyOverride = async (serviceName, force = false) => {
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
          force,
        }),
      });
      if (res.status === 401 || res.status === 403) {
        throw new Error('Authentication failed. Check your API token.');
      }
      if (res.status === 409 && !force) {
        // The service has a resolver ATC did not create; replacing it is the operator's call.
        let message = 'This service already has a service-resolver that ATC did not create.';
        try {
          const body = await res.json();
          if (body && body.error) message = body.error;
        } catch {
          // keep the default message
        }
        if (window.confirm(`${message}\n\nReplace the existing service-resolver anyway?`)) {
          await handleApplyOverride(serviceName, true);
        }
        return;
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

  // Changing routing needs Consul (and ATC); block the buttons rather than let them fail.
  const writesBlocked = !atcReachable || (!!consulStatus && consulStatus.reachable === false);

  // "How long ago" is measured against the server's latest check, so it needs no clock read
  // during render and is not skewed by the browser's clock.
  const formatTimestamp = (iso, asOfIso) => {
    const then = new Date(iso);
    if (Number.isNaN(then.getTime())) return iso;
    const asOf = new Date(asOfIso);
    if (Number.isNaN(asOf.getTime())) return then.toLocaleTimeString();
    const minutes = Math.max(0, Math.round((asOf.getTime() - then.getTime()) / 60000));
    const ago = minutes < 1 ? 'less than a minute ago' : minutes === 1 ? '1 minute ago' : `${minutes} minutes ago`;
    return `${then.toLocaleTimeString()} (${ago})`;
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
          {modules.length > 0 ? (
            <div className="modules-badges-list">
              {modules.map(mod => {
                const isModLeader = !!leaderComponents[mod];
                const modDetails = leaderInfo?.modules?.[mod];
                const leaderNode = modDetails?.leader_node || '<none>';
                const sessionId = modDetails?.session_id || '<none>';
                const lockKey = modDetails?.lock_key || '';
                
                const tooltipText = `Module: ${mod}
Status: ${isModLeader ? 'Active Leader' : 'Standby'}
Leader Node: ${leaderNode}
Consul Session: ${sessionId}
Lock Key: ${lockKey}`;

                return (
                  <span
                    key={mod}
                    className={`module-badge ${isModLeader ? 'active' : 'standby'}`}
                    title={tooltipText}
                  >
                    📦 {mod.toUpperCase()}: {isModLeader ? '● LEADER' : '○ STANDBY'}
                  </span>
                );
              })}
            </div>
          ) : (
            <span className="leader-badge standby" title="No reconciliation modules are currently running on this node">
              ○ NO ACTIVE RECONCILIATION MODULES
            </span>
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
              onBlur={(e) => flushTokenSync(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') {
                  flushTokenSync(e.target.value);
                }
              }}
              className="auth-input"
              aria-label="API Token"
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

            {/* Outage banners: plain-language explanations instead of raw errors */}
            {!atcReachable && (
              <div className="outage-banner glass-panel" role="alert">
                <span className="outage-icon" aria-hidden="true">🔌</span>
                <div className="outage-message">
                  <h4>Lost connection to ATC</h4>
                  <p>This page cannot reach the ATC server. It may be restarting, or your network connection may have dropped.</p>
                  {lastUpdated && <p className="outage-detail">The information below is from {lastUpdated} and may be out of date.</p>}
                  {autoRefresh && <p className="outage-detail">Checking again automatically every 10 seconds.</p>}
                </div>
                <button onClick={handleManualRefresh} className="btn btn-retry">
                  Retry now
                </button>
              </div>
            )}
            {atcReachable && consulStatus && consulStatus.reachable === false && (
              <div className="outage-banner glass-panel" role="alert">
                <span className="outage-icon" aria-hidden="true">⚠️</span>
                <div className="outage-message">
                  <h4>{consulStatus.problem === 'no_leader' ? 'Consul has no cluster leader' : 'Consul is unreachable'}</h4>
                  <p>{consulStatus.message} While this lasts, ATC cannot read or change routing: automatic failover and redirects are paused and manual overrides cannot be applied. Routing rules already stored in Consul stay as they are.</p>
                  <p className="outage-detail">
                    {consulStatus.last_contact
                      ? `Last successful contact: ${formatTimestamp(consulStatus.last_contact, consulStatus.checked_at)}.`
                      : 'ATC has not reached Consul since it started.'}
                    {lastUpdated ? ` Services below show the last known state (synced ${lastUpdated}).` : ''}
                  </p>
                  {autoRefresh && <p className="outage-detail">Checking again automatically every 10 seconds.</p>}
                </div>
                <button onClick={handleManualRefresh} className="btn btn-retry">
                  Retry now
                </button>
              </div>
            )}

            {/* Error State */}
            {error && atcReachable && !(consulStatus && consulStatus.reachable === false) && (
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
                        {svc.managed_by === 'foreign' && (
                          <span
                            className="ownership-badge foreign"
                            title="This service already has a service-resolver that ATC did not create. ATC leaves it untouched, so automatic failover and redirects are not applied. Tag the service atc.adopt=true to let ATC manage it (ATC then replaces the existing entry)."
                          >
                            Not managed by ATC
                          </span>
                        )}
                        {svc.managed_by === 'adopting' && (
                          <span
                            className="ownership-badge adopting"
                            title="Tagged atc.adopt=true: ATC is replacing the existing service-resolver with its own on the next reconciliation."
                          >
                            Adopting
                          </span>
                        )}
                        {svc.meta && svc.meta['created-by'] === 'atc-override' && (
                          <>
                            <span className="override-badge active" title={`Expires: ${svc.meta['atc-override-expires-at'] || 'Never'}`}>
                              Bypassed ({formatExpiry(svc.meta['atc-override-expires-at'])})
                            </span>
                            <button
                              onClick={() => handlePurge(svc.name)}
                              disabled={writesBlocked}
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
                              disabled={writesBlocked}
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



                        {/* Manual Override controls */}
                        <div className="override-controls-section">
                          {activeOverrideForm === svc.name ? (
                            <div className="override-form glass-panel">
                              <h4>Manual Override Controls</h4>
                              <div className="form-group">
                                <label htmlFor="override-type-select">Override Type</label>
                                <select 
                                  id="override-type-select"
                                  value={overrideType} 
                                  onChange={(e) => setOverrideType(e.target.value)}
                                  className="form-select"
                                >
                                  <option value="failover">Failover</option>
                                  <option value="redirect">Redirect</option>
                                </select>
                              </div>
                              <div className="form-group">
                                <label htmlFor="override-target-dc-input">Target Datacenter</label>
                                {Object.keys(federation).length > 0 ? (
                                  <select
                                    id="override-target-dc-input"
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
                                    id="override-target-dc-input"
                                    type="text"
                                    placeholder="e.g. dc2"
                                    value={overrideTargetDc}
                                    onChange={(e) => setOverrideTargetDc(e.target.value)}
                                    className="form-input"
                                  />
                                )}
                              </div>
                              <div className="form-group">
                                <label htmlFor="override-namespace-input">Target Namespace (Optional)</label>
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
                                <label htmlFor="override-duration-select">Duration (TTL)</label>
                                <select
                                  id="override-duration-select"
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
                                  disabled={writesBlocked}
                                  title={writesBlocked ? 'Unavailable while ATC cannot reach Consul' : undefined}
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
                                disabled={writesBlocked}
                                title={writesBlocked ? 'Unavailable while ATC cannot reach Consul' : undefined}
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
