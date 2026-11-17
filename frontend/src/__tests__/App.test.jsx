import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { vi, describe, it, expect, beforeEach } from 'vitest';
import App from '../App';

const mockFederation = [
  { datacenter: 'dc1', status: 'alive' },
  { datacenter: 'dc2', status: 'failed' }
];

const mockServices = [
  {
    name: 'payment-service',
    tags: ['atc.enabled=true', 'primary'],
    resolver_type: 'failover',
    status: 'active',
    failover_strategy: 'multi-region-failover',
    failover_targets: [
      { service: 'payment-service', datacenter: 'dc2' },
      { service: 'fallback-service', datacenter: 'dc3' }
    ]
  },
  {
    name: 'auth-service',
    tags: ['atc.enabled=true', 'status:deleted'],
    resolver_type: 'redirect',
    status: 'deleted',
    redirect_strategy: 'geo-redirect',
    redirect_target: { service: 'auth-service', datacenter: 'dc2' }
  }
];

describe('ATC Dashboard App', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    
    window.fetch = vi.fn().mockImplementation((url, options) => {
      if (url.includes('/api/status')) {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ consul: { reachable: true, message: 'Connected to Consul.' } })
        });
      }
      if (url.includes('/api/federation')) {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve(mockFederation)
        });
      }
      if (url.includes('/api/services')) {
        if (options && options.method === 'DELETE') {
          return Promise.resolve({ ok: true });
        }
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve(mockServices)
        });
      }
      if (url.includes('/api/modules')) {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve(['forwarder', 'redirector'])
        });
      }
      if (url.includes('/api/strategies')) {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({ failover: {}, redirect: {} })
        });
      }
      if (url.includes('/api/leader')) {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            leader: true,
            auth_enabled: false,
            components: { forwarder: true, redirector: true }
          })
        });
      }
      return Promise.resolve({ ok: false });
    });
  });

  it('renders stats, headers and cards correctly', async () => {
    render(<App />);

    await waitFor(() => {
      expect(screen.getByText('payment-service')).toBeInTheDocument();
    });

    expect(screen.getByText('auth-service')).toBeInTheDocument();
    expect(screen.getByText('Service Resolvers Active')).toBeInTheDocument();
  });

  it('filters services based on search input', async () => {
    const { container } = render(<App />);

    await waitFor(() => {
      expect(screen.getByText('payment-service')).toBeInTheDocument();
    });

    const searchInput = container.querySelector('.search-input');
    expect(searchInput).toBeInTheDocument();
    
    fireEvent.change(searchInput, { target: { value: 'auth' } });

    expect(screen.getByText('auth-service')).toBeInTheDocument();
    expect(screen.queryByText('payment-service')).not.toBeInTheDocument();
  });

  it('displays the correct failover and redirect routing node paths', async () => {
    render(<App />);

    await waitFor(() => {
      expect(screen.getByText('payment-service')).toBeInTheDocument();
    });

    const paymentCard = screen.getByText('payment-service').closest('.service-card');
    const authCard = screen.getByText('auth-service').closest('.service-card');

    expect(paymentCard).toBeInTheDocument();
    expect(authCard).toBeInTheDocument();

    // Verify payment-service card visualizations
    const paymentNodes = paymentCard.querySelectorAll('.failover-node');
    expect(paymentNodes.length).toBe(3);
    expect(paymentNodes[0].textContent).toBe('Local');
    expect(paymentNodes[1].textContent).toContain('payment-service (dc2)');
    expect(paymentNodes[2].textContent).toContain('fallback-service (dc3)');

    // Verify auth-service card visualizations
    const authNodes = authCard.querySelectorAll('.failover-node');
    expect(authNodes.length).toBe(2);
    expect(authNodes[0].textContent).toBe('Local');
    expect(authNodes[1].textContent).toContain('auth-service (dc2)');
  });

  it('handles the purge trigger with confirmation modal rejection', async () => {
    render(<App />);

    await waitFor(() => {
      expect(screen.getByText('auth-service')).toBeInTheDocument();
    });

    const confirmMock = vi.spyOn(window, 'confirm').mockImplementation(() => false);
    const purgeButton = screen.getByRole('button', { name: /purge/i });

    fireEvent.click(purgeButton);

    expect(confirmMock).toHaveBeenCalled();
    expect(window.fetch).not.toHaveBeenCalledWith(expect.stringContaining('DELETE'), expect.any(Object));
  });

  it('handles the purge trigger with confirmation modal approval', async () => {
    render(<App />);

    await waitFor(() => {
      expect(screen.getByText('auth-service')).toBeInTheDocument();
    });

    const confirmMock = vi.spyOn(window, 'confirm').mockImplementation(() => true);
    const purgeButton = screen.getByRole('button', { name: /purge/i });

    fireEvent.click(purgeButton);

    expect(confirmMock).toHaveBeenCalled();
    await waitFor(() => {
      expect(window.fetch).toHaveBeenCalledWith(
        expect.stringContaining('/api/services?name=auth-service'),
        expect.objectContaining({ method: 'DELETE' })
      );
    });
  });

  it('renders active manual overrides with expiration info', async () => {
    const mockServicesWithOverride = [
      ...mockServices,
      {
        name: 'override-service',
        tags: ['atc.enabled=true'],
        resolver_type: 'failover',
        status: 'active',
        meta: {
          'created-by': 'atc-override',
          'atc-override-expires-at': new Date(Date.now() + 15 * 60 * 1000).toISOString()
        }
      }
    ];

    window.fetch = vi.fn().mockImplementation((url) => {
      if (url.includes('/api/services')) {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve(mockServicesWithOverride)
        });
      }
      return Promise.resolve({ ok: true, json: () => Promise.resolve([]) });
    });

    render(<App />);

    await waitFor(() => {
      expect(screen.getByText('override-service')).toBeInTheDocument();
    });

    expect(screen.getByText(/Bypassed \(Expires in 15m\)/)).toBeInTheDocument();
  });

  it('submits manual override with TTL duration and associates form labels with inputs', async () => {
    render(<App />);

    await waitFor(() => {
      expect(screen.getByText('payment-service')).toBeInTheDocument();
    });

    const overrideButton = screen.getAllByRole('button', { name: /Apply Manual Override/i })[0];
    fireEvent.click(overrideButton);

    expect(screen.getByText('Manual Override Controls')).toBeInTheDocument();

    // Verify all form labels are associated via htmlFor / id
    expect(screen.getByLabelText('Override Type')).toBeInTheDocument();
    expect(screen.getByLabelText('Target Datacenter')).toBeInTheDocument();
    expect(screen.getByLabelText('Target Namespace (Optional)')).toBeInTheDocument();
    expect(screen.getByLabelText('Duration (TTL)')).toBeInTheDocument();

    const applyBtn = screen.getByRole('button', { name: /Apply Override/i });
    fireEvent.click(applyBtn);

    await waitFor(() => {
      expect(window.fetch).toHaveBeenCalledWith(
        expect.stringContaining('/api/overrides'),
        expect.objectContaining({
          method: 'POST'
        })
      );
    });
  });

  it('debounces API token input changes before firing parallel API requests', async () => {
    const origImpl = window.fetch.getMockImplementation();
    window.fetch.mockImplementation((url, options) => {
      if (url.includes('/api/leader')) {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve({
            leader: true,
            auth_enabled: true,
            components: { forwarder: true, redirector: true }
          })
        });
      }
      return origImpl(url, options);
    });

    render(<App />);

    await waitFor(() => {
      expect(screen.getByPlaceholderText('Enter API Token...')).toBeInTheDocument();
    });

    const tokenInput = screen.getByPlaceholderText('Enter API Token...');
    window.fetch.mockClear();

    fireEvent.change(tokenInput, { target: { value: 'a' } });
    fireEvent.change(tokenInput, { target: { value: 'ab' } });
    fireEvent.change(tokenInput, { target: { value: 'abc' } });

    // Should not fire requests immediately on each keystroke
    expect(window.fetch).not.toHaveBeenCalled();

    // Flushing on blur triggers a single batch of 6 parallel requests (status + 5 API calls)
    fireEvent.blur(tokenInput);

    await waitFor(() => {
      expect(window.fetch).toHaveBeenCalledTimes(6);
    });
  });

  describe('API token storage', () => {
    beforeEach(() => {
      window.localStorage.clear();
      window.sessionStorage.clear();
    });

    it('keeps the token in session storage, not local storage', async () => {
      const origImpl = window.fetch.getMockImplementation();
      window.fetch.mockImplementation((url, options) => {
        if (url.includes('/api/leader')) {
          return Promise.resolve({ ok: true, json: () => Promise.resolve({ leader: true, auth_enabled: true, components: {} }) });
        }
        return origImpl(url, options);
      });

      render(<App />);
      const input = await screen.findByPlaceholderText('Enter API Token...');
      fireEvent.change(input, { target: { value: 'secret-token' } });
      fireEvent.blur(input);

      await waitFor(() => expect(window.sessionStorage.getItem('atc-api-token')).toBe('secret-token'));
      expect(window.localStorage.getItem('atc-api-token')).toBeNull();
    });

    it('removes a token left in local storage by older versions', async () => {
      window.localStorage.setItem('atc-api-token', 'old-token');
      render(<App />);
      await screen.findByText('payment-service');
      expect(window.localStorage.getItem('atc-api-token')).toBeNull();
    });
  });

  describe('resolvers not created by ATC', () => {
    const withForeignService = (origImpl, overrideResponses) => (url, options) => {
      if (url.includes('/api/services') && !(options && options.method)) {
        return Promise.resolve({
          ok: true,
          json: () => Promise.resolve([{ ...mockServices[0], managed_by: 'foreign' }])
        });
      }
      if (url.includes('/api/overrides')) return Promise.resolve(overrideResponses.shift());
      return origImpl(url, options);
    };

    it('flags a service whose resolver ATC leaves alone', async () => {
      const origImpl = window.fetch.getMockImplementation();
      window.fetch.mockImplementation(withForeignService(origImpl, []));

      render(<App />);

      const badge = await screen.findByText('Not managed by ATC');
      expect(badge.getAttribute('title')).toMatch(/atc\.adopt=true/);
    });

    it('asks before an override replaces it, then resends with force', async () => {
      const origImpl = window.fetch.getMockImplementation();
      window.fetch.mockImplementation(withForeignService(origImpl, [
        { ok: false, status: 409, json: () => Promise.resolve({ error: 'service already has a service-resolver that ATC did not create' }) },
        { ok: true, status: 200, text: () => Promise.resolve('') }
      ]));
      const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(true);

      render(<App />);
      fireEvent.click((await screen.findAllByRole('button', { name: /Apply Manual Override/i }))[0]);
      fireEvent.click(screen.getByRole('button', { name: /Apply Override/i }));

      await waitFor(() => expect(confirmSpy).toHaveBeenCalledWith(expect.stringMatching(/did not create/)));
      await waitFor(() => {
        const overridePosts = window.fetch.mock.calls.filter(([url]) => url.includes('/api/overrides'));
        expect(overridePosts).toHaveLength(2);
        expect(JSON.parse(overridePosts[0][1].body).force).toBe(false);
        expect(JSON.parse(overridePosts[1][1].body).force).toBe(true);
      });
    });

    it('does not replace it when the user declines', async () => {
      const origImpl = window.fetch.getMockImplementation();
      window.fetch.mockImplementation(withForeignService(origImpl, [
        { ok: false, status: 409, json: () => Promise.resolve({ error: 'not created by ATC' }) }
      ]));
      vi.spyOn(window, 'confirm').mockReturnValue(false);

      render(<App />);
      fireEvent.click((await screen.findAllByRole('button', { name: /Apply Manual Override/i }))[0]);
      fireEvent.click(screen.getByRole('button', { name: /Apply Override/i }));

      await waitFor(() => expect(window.confirm).toHaveBeenCalled());
      expect(window.fetch.mock.calls.filter(([url]) => url.includes('/api/overrides'))).toHaveLength(1);
    });
  });

  describe('outages', () => {
    const consulDownStatus = {
      ok: true,
      json: () => Promise.resolve({
        consul: {
          reachable: false,
          problem: 'unreachable',
          message: 'ATC cannot reach Consul.',
          last_contact: '2026-11-17T10:00:00Z',
          checked_at: '2026-11-17T10:07:00Z'
        }
      })
    };

    it('explains a Consul outage instead of showing an authentication or raw error', async () => {
      const origImpl = window.fetch.getMockImplementation();
      window.fetch.mockImplementation((url, options) => {
        if (url.includes('/api/status')) return Promise.resolve(consulDownStatus);
        if (url.includes('/api/services')) {
          // Delegated token cannot be verified while Consul is down.
          return Promise.resolve({
            ok: false,
            status: 503,
            statusText: 'Service Unavailable',
            json: () => Promise.resolve({ error: 'consul_unavailable' })
          });
        }
        return origImpl(url, options);
      });

      render(<App />);

      expect(await screen.findByText('Consul is unreachable')).toBeInTheDocument();
      expect(screen.getByText(/automatic failover and redirects are paused/i)).toBeInTheDocument();
      expect(screen.getByText(/Last successful contact: .*\(7 minutes ago\)/)).toBeInTheDocument();
      expect(screen.queryByText(/Authentication failed/i)).not.toBeInTheDocument();
      expect(screen.queryByText(/Failed to fetch services/i)).not.toBeInTheDocument();
    });

    it('distinguishes a Consul cluster without a leader', async () => {
      const origImpl = window.fetch.getMockImplementation();
      window.fetch.mockImplementation((url, options) => {
        if (url.includes('/api/status')) {
          return Promise.resolve({
            ok: true,
            json: () => Promise.resolve({
              consul: { reachable: false, problem: 'no_leader', message: 'Consul is reachable but has no cluster leader.' }
            })
          });
        }
        return origImpl(url, options);
      });

      render(<App />);

      expect(await screen.findByText('Consul has no cluster leader')).toBeInTheDocument();
      expect(screen.getByText(/has not reached Consul since it started/i)).toBeInTheDocument();
    });

    it('disables routing changes while Consul is down', async () => {
      const origImpl = window.fetch.getMockImplementation();
      window.fetch.mockImplementation((url, options) => {
        if (url.includes('/api/status')) return Promise.resolve(consulDownStatus);
        return origImpl(url, options);
      });

      render(<App />);

      await screen.findByText('Consul is unreachable');
      const overrideButtons = await screen.findAllByText(/Apply Manual Override/);
      overrideButtons.forEach((btn) => expect(btn.closest('button')).toBeDisabled());
      expect(screen.getByText(/Purge/).closest('button')).toBeDisabled();
    });

    it('tells the user when ATC itself cannot be reached', async () => {
      window.fetch.mockImplementation(() => Promise.reject(new TypeError('Failed to fetch')));

      render(<App />);

      expect(await screen.findByText('Lost connection to ATC')).toBeInTheDocument();
      expect(screen.queryByText('Failed to fetch')).not.toBeInTheDocument();
    });
  });
});
