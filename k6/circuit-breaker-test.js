/**
 * GoGate Circuit Breaker Test
 * 
 * Tests circuit breaker behavior under backend failures
 * 
 * Usage:
 *   # First, simulate a backend failure (stop backend2)
 *   docker stop backend2
 *   
 *   # Run the test
 *   k6 run k6/circuit-breaker-test.js
 *   
 *   # Restore backend
 *   docker start backend2
 */

import http from 'k6/http';
import { check, sleep, group } from 'k6';
import { Counter, Trend } from 'k6/metrics';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8081';
const ADMIN_URL = __ENV.ADMIN_URL || 'http://localhost:9090';

const circuitOpenErrors = new Counter('circuit_open_errors');
const backendErrors = new Counter('backend_errors');
const successfulResponses = new Counter('successful_responses');
const responseTrend = new Trend('response_time');

export const options = {
  scenarios: {
    steady: {
      executor: 'constant-vus',
      vus: 5,
      duration: '60s',
    },
  },
};

export default function() {
  group('Circuit Breaker Behavior', () => {
    const res = http.get(`${BASE_URL}/`);
    responseTrend.add(res.timings.duration);

    if (res.status === 200) {
      successfulResponses.add(1);
    } else if (res.status === 503) {
      // Circuit breaker open or backend unavailable
      const body = res.body.toLowerCase();
      if (body.includes('circuit') || body.includes('breaker')) {
        circuitOpenErrors.add(1);
      } else {
        backendErrors.add(1);
      }
    } else if (res.status === 502) {
      backendErrors.add(1);
    }

    check(res, {
      'response received': (r) => r.status !== 0,
      'not a timeout': (r) => r.timings.duration < 5000,
    });
  });

  // Every 10 iterations, check circuit breaker state via admin
  if (__ITER % 10 === 0) {
    group('Admin State Check', () => {
      const statsRes = http.get(`${ADMIN_URL}/stats`);
      if (statsRes.status === 200) {
        try {
          const stats = JSON.parse(statsRes.body);
          // Log circuit breaker state if available
          if (stats.circuitBreaker) {
            console.log(`Circuit Breaker State: ${JSON.stringify(stats.circuitBreaker)}`);
          }
        } catch (e) {
          // Ignore parse errors
        }
      }
    });
  }

  sleep(0.1);
}

export function handleSummary(data) {
  const open = data.metrics.circuit_open_errors?.values.count || 0;
  const backend = data.metrics.backend_errors?.values.count || 0;
  const success = data.metrics.successful_responses?.values.count || 0;
  const total = open + backend + success;

  console.log('\n=== Circuit Breaker Test Results ===');
  console.log(`Total Requests: ${total}`);
  console.log(`Successful: ${success} (${((success/total)*100).toFixed(1)}%)`);
  console.log(`Circuit Open (503): ${open} (${((open/total)*100).toFixed(1)}%)`);
  console.log(`Backend Errors: ${backend} (${((backend/total)*100).toFixed(1)}%)`);
  console.log('');

  if (open > 0) {
    console.log('✓ Circuit breaker engaged during failures');
  } else if (backend === 0 && success > 0) {
    console.log('✓ All backends healthy, no circuit breaker needed');
  }

  return {
    'k6/circuit-breaker-summary.json': JSON.stringify(data),
  };
}
