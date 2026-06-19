import React, { useState, useEffect } from 'react';
import './App.css';

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

  const fetchServices = async (showLoading = true) => {
    try {
      if (showLoading) {
        setLoading(true);
      }
      
      try {
        const leaderRes = await fetch('/api/leader');
        if (leaderRes.ok) {
          const leaderData = await leaderRes.json();
          setIsLeader(leaderData.leader);
        }
      } catch (err) {
        console.error("Failed to fetch leader status:", err);
      }

      // Fetch federation status
      try {
        const fedRes = await fetch('/api/federation');
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

      const res = await fetch('/api/services');
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
      });
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
        </div>
      </header>


      <main className="main-content">
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
                    <h3 className="service-name">{svc.name}</h3>
                    {svc.status === 'deleted' && (
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
                  </div>
                </div>
              ))}
            </div>
          )}
        </section>
      </main>

      <footer className="app-footer">
        <p>ATC Dashboard • Running in local cluster mode</p>
      </footer>
    </div>
  );
}

export default App;
