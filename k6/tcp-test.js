/**
 * GoGate TCP Proxy Load Test
 * 
 * Tests L4 TCP proxy performance using WebSocket (k6 doesn't support raw TCP)
 * For raw TCP testing, use the shell scripts or netcat
 * 
 * Usage:
 *   k6 run k6/tcp-test.js
 */

import { check } from 'k6';
import { Counter, Trend } from 'k6/metrics';
import http from 'k6/http';

// Since k6 doesn't support raw TCP, we test via HTTP to validate
// the infrastructure, and use shell scripts for actual TCP testing
const BASE_URL = __ENV.BASE_URL || 'http://localhost:8081';

export const options = {
  scenarios: {
    tcp_simulation: {
      executor: 'constant-vus',
      vus: 20,
      duration: '30s',
    },
  },
  thresholds: {
    http_req_duration: ['p(95)<200'],
    http_req_failed: ['rate<0.01'],
  },
};

export default function() {
  // Simulate TCP-like workload via HTTP with keepalive
  const params = {
    headers: {
      'Connection': 'keep-alive',
      'X-Request-ID': `tcp-test-${__VU}-${__ITER}`,
    },
  };

  // Multiple requests per iteration to simulate persistent connections
  for (let i = 0; i < 10; i++) {
    const res = http.get(`${BASE_URL}/`, params);
    
    check(res, {
      'status is 200': (r) => r.status === 200,
      'low latency': (r) => r.timings.duration < 100,
    });
  }
}

export function handleSummary(data) {
  console.log('\n=== TCP Proxy Test (via HTTP) ===');
  console.log('Note: For raw TCP testing, use:');
  console.log('  ./scripts/load-tcp.sh');
  console.log('');

  if (data.metrics.http_req_duration) {
    const d = data.metrics.http_req_duration.values;
    console.log(`Latency p50: ${d['p(50)'].toFixed(2)}ms`);
    console.log(`Latency p95: ${d['p(95)'].toFixed(2)}ms`);
  }

  return {
    'k6/tcp-summary.json': JSON.stringify(data),
  };
}
