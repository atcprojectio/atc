import React from 'react';
import { render, screen, fireEvent, waitFor, within } from '@testing-library/react';
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
    
    global.fetch = vi.fn().mockImplementation((url, options) => {
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
    expect(global.fetch).not.toHaveBeenCalledWith(expect.stringContaining('DELETE'), expect.any(Object));
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
      expect(global.fetch).toHaveBeenCalledWith(
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

    global.fetch = vi.fn().mockImplementation((url) => {
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

  it('submits manual override with TTL duration', async () => {
    const { container } = render(<App />);

    await waitFor(() => {
      expect(screen.getByText('payment-service')).toBeInTheDocument();
    });

    const overrideButton = screen.getAllByRole('button', { name: /Apply Manual Override/i })[0];
    fireEvent.click(overrideButton);

    expect(screen.getByText('Manual Override Controls')).toBeInTheDocument();

    const applyBtn = screen.getByRole('button', { name: /Apply Override/i });
    fireEvent.click(applyBtn);

    await waitFor(() => {
      expect(global.fetch).toHaveBeenCalledWith(
        expect.stringContaining('/api/overrides'),
        expect.objectContaining({
          method: 'POST'
        })
      );
    });
  });
});
