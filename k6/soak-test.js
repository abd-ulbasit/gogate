/**
 * GoGate Soak Test
 * 
 * Long-running test to detect memory leaks, connection leaks, etc.
 * 
 * Usage:
 *   k6 run k6/soak-test.js
 *   
 *   # For longer durations:
 *   k6 run --duration 1h k6/soak-test.js
 */

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Trend, Counter, Gauge } from 'k6/metrics';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8081';
const ADMIN_URL = __ENV.ADMIN_URL || 'http://localhost:9090';

// Track metrics over time
const memoryUsage = new Gauge('proxy_memory_mb');
const activeConnections = new Gauge('proxy_active_connections');
const errorCount = new Counter('errors');

export const options = {
  scenarios: {
    soak: {
      executor: 'constant-vus',
      vus: 10,
      duration: '5m',  // Adjust for real soak test (e.g., 1h, 4h, 24h)
    },
  },
  thresholds: {
    http_req_duration: ['p(99)<1000'],
    http_req_failed: ['rate<0.001'],  // Very strict for soak test
    errors: ['count<10'],
  },
};

export default function() {
  // Normal traffic
  const res = http.get(`${BASE_URL}/`);
  
  const success = check(res, {
    'status is 200': (r) => r.status === 200,
  });

  if (!success) {
    errorCount.add(1);
  }

  // Periodically check proxy health metrics
  if (__ITER % 100 === 0) {
    checkProxyHealth();
  }

  sleep(0.1);
}

function checkProxyHealth() {
  const statsRes = http.get(`${ADMIN_URL}/stats`);
  
  if (statsRes.status === 200) {
    try {
      const stats = JSON.parse(statsRes.body);
      
      // Track memory if available
      if (stats.memory_mb) {
        memoryUsage.add(stats.memory_mb);
      }
      
      // Track connections
      if (stats.active_connections !== undefined) {
        activeConnections.add(stats.active_connections);
      }
    } catch (e) {
      // Ignore parse errors
    }
  }
}

export function handleSummary(data) {
  console.log('\n=== Soak Test Results ===');
  
  const duration = data.state.testRunDurationMs / 1000 / 60;
  console.log(`Duration: ${duration.toFixed(1)} minutes`);
  
  if (data.metrics.http_reqs) {
    console.log(`Total Requests: ${data.metrics.http_reqs.values.count}`);
    console.log(`Avg RPS: ${data.metrics.http_reqs.values.rate.toFixed(2)}`);
  }

  if (data.metrics.http_req_duration) {
    const d = data.metrics.http_req_duration.values;
    console.log(`\nLatency:`);
    console.log(`  avg: ${d.avg.toFixed(2)}ms`);
    console.log(`  p99: ${d['p(99)'].toFixed(2)}ms`);
    console.log(`  max: ${d.max.toFixed(2)}ms`);
  }

  const errors = data.metrics.errors?.values.count || 0;
  console.log(`\nErrors: ${errors}`);

  // Check for degradation over time
  console.log('\n--- Health Check ---');
  if (data.metrics.proxy_memory_mb) {
    const mem = data.metrics.proxy_memory_mb.values;
    console.log(`Memory: min=${mem.min?.toFixed(1)}MB, max=${mem.max?.toFixed(1)}MB`);
    if (mem.max > mem.min * 2) {
      console.log('⚠️  Warning: Memory grew significantly during test');
    } else {
      console.log('✓ Memory stable');
    }
  }

  if (data.metrics.proxy_active_connections) {
    const conn = data.metrics.proxy_active_connections.values;
    console.log(`Connections: min=${conn.min}, max=${conn.max}`);
  }

  return {
    'k6/soak-summary.json': JSON.stringify(data),
  };
}
