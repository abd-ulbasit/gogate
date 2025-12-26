/**
 * GoGate Load Test Suite
 * 
 * k6 load testing scripts for L4/L7 proxy performance testing
 * 
 * Usage:
 *   # Run basic load test
 *   k6 run k6/load-test.js
 * 
 *   # Run with custom options
 *   k6 run --vus 50 --duration 60s k6/load-test.js
 * 
 *   # Run specific scenario
 *   k6 run --env SCENARIO=spike k6/load-test.js
 * 
 *   # Output to JSON for analysis
 *   k6 run --out json=results.json k6/load-test.js
 */

import http from 'k6/http';
import { check, group, sleep, fail } from 'k6';
import { Rate, Trend, Counter } from 'k6/metrics';

// Custom metrics
const errorRate = new Rate('errors');
const latencyTrend = new Trend('request_latency', true);
const requestCounter = new Counter('total_requests');
const backendDistribution = new Counter('backend_hits');

// Configuration
const BASE_URL = __ENV.BASE_URL || 'http://localhost:8081';
const ADMIN_URL = __ENV.ADMIN_URL || 'http://localhost:9090';

// Scenarios
export const options = {
  scenarios: {
    // Default: Steady load
    steady_load: {
      executor: 'constant-vus',
      vus: 10,
      duration: '30s',
      gracefulStop: '5s',
    },
    
    // Ramp up and down
    ramp_up: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { duration: '10s', target: 20 },  // Ramp up
        { duration: '30s', target: 20 },  // Hold
        { duration: '10s', target: 0 },   // Ramp down
      ],
      gracefulRampDown: '5s',
      startTime: '35s',  // Start after steady_load
    },
    
    // Spike test
    spike: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { duration: '5s', target: 5 },    // Normal load
        { duration: '5s', target: 100 },  // Spike!
        { duration: '10s', target: 100 }, // Hold spike
        { duration: '5s', target: 5 },    // Cool down
        { duration: '5s', target: 0 },    // Stop
      ],
      startTime: '85s',  // Start after ramp_up
    },
  },

  thresholds: {
    http_req_duration: ['p(95)<500', 'p(99)<1000'],  // 95% under 500ms, 99% under 1s
    http_req_failed: ['rate<0.01'],                  // Less than 1% errors
    errors: ['rate<0.01'],
  },
};

// Main test function
export default function() {
  group('HTTP Proxy Tests', () => {
    testHttpProxy();
  });

  group('Admin API Tests', () => {
    testAdminEndpoints();
  });

  // Small sleep to prevent hammering
  sleep(Math.random() * 0.1);
}

function testHttpProxy() {
  const res = http.get(`${BASE_URL}/`, {
    headers: {
      'X-Request-ID': `k6-${__VU}-${__ITER}`,
    },
    tags: { name: 'http_proxy' },
  });

  const success = check(res, {
    'status is 200': (r) => r.status === 200,
    'response time < 500ms': (r) => r.timings.duration < 500,
    'has response body': (r) => r.body && r.body.length > 0,
  });

  errorRate.add(!success);
  latencyTrend.add(res.timings.duration);
  requestCounter.add(1);

  // Track backend distribution from response
  if (res.body) {
    if (res.body.includes('Backend 1')) {
      backendDistribution.add(1, { backend: 'backend1' });
    } else if (res.body.includes('Backend 2')) {
      backendDistribution.add(1, { backend: 'backend2' });
    } else if (res.body.includes('Backend 3')) {
      backendDistribution.add(1, { backend: 'backend3' });
    }
  }
}

function testAdminEndpoints() {
  // Health check
  const healthRes = http.get(`${ADMIN_URL}/health`, {
    tags: { name: 'admin_health' },
  });

  check(healthRes, {
    'health check returns 200': (r) => r.status === 200,
  });

  // Stats endpoint
  const statsRes = http.get(`${ADMIN_URL}/stats`, {
    tags: { name: 'admin_stats' },
  });

  check(statsRes, {
    'stats returns 200': (r) => r.status === 200,
    'stats has JSON': (r) => {
      try {
        JSON.parse(r.body);
        return true;
      } catch {
        return false;
      }
    },
  });

  // Backends endpoint
  const backendsRes = http.get(`${ADMIN_URL}/backends`, {
    tags: { name: 'admin_backends' },
  });

  check(backendsRes, {
    'backends returns 200': (r) => r.status === 200,
  });
}

// Setup function - runs once before test
export function setup() {
  console.log(`Testing GoGate at ${BASE_URL}`);
  console.log(`Admin API at ${ADMIN_URL}`);

  // Verify proxy is reachable
  const res = http.get(`${ADMIN_URL}/health`);
  if (res.status !== 200) {
    fail(`GoGate is not reachable at ${ADMIN_URL}`);
  }

  return { startTime: new Date().toISOString() };
}

// Teardown function - runs once after test
export function teardown(data) {
  console.log(`Test started at: ${data.startTime}`);
  console.log(`Test completed at: ${new Date().toISOString()}`);
}

// HTML summary report
export function handleSummary(data) {
  return {
    'stdout': textSummary(data, { indent: ' ', enableColors: true }),
    'k6/summary.json': JSON.stringify(data),
  };
}

function textSummary(data, options) {
  const lines = [];
  lines.push('\n=== GoGate Load Test Summary ===\n');

  // Request stats
  if (data.metrics.http_req_duration) {
    const duration = data.metrics.http_req_duration;
    lines.push('Request Duration:');
    lines.push(`  avg: ${duration.values.avg.toFixed(2)}ms`);
    lines.push(`  p50: ${duration.values['p(50)'].toFixed(2)}ms`);
    lines.push(`  p95: ${duration.values['p(95)'].toFixed(2)}ms`);
    lines.push(`  p99: ${duration.values['p(99)'].toFixed(2)}ms`);
    lines.push(`  max: ${duration.values.max.toFixed(2)}ms`);
    lines.push('');
  }

  // Request count
  if (data.metrics.http_reqs) {
    lines.push(`Total Requests: ${data.metrics.http_reqs.values.count}`);
    lines.push(`Requests/sec: ${data.metrics.http_reqs.values.rate.toFixed(2)}`);
    lines.push('');
  }

  // Error rate
  if (data.metrics.http_req_failed) {
    const failRate = data.metrics.http_req_failed.values.rate * 100;
    lines.push(`Error Rate: ${failRate.toFixed(2)}%`);
  }

  // Threshold results
  lines.push('\nThresholds:');
  for (const [name, threshold] of Object.entries(data.thresholds || {})) {
    const status = threshold.ok ? '✓' : '✗';
    lines.push(`  ${status} ${name}`);
  }

  return lines.join('\n');
}
