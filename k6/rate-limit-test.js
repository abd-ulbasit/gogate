/**
 * GoGate Rate Limiter Stress Test
 * 
 * Tests the token bucket rate limiter under heavy load
 * 
 * Usage:
 *   k6 run k6/rate-limit-test.js
 */

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Rate, Counter } from 'k6/metrics';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8081';
const rateLimitedRequests = new Counter('rate_limited_requests');
const successfulRequests = new Counter('successful_requests');

export const options = {
  scenarios: {
    // Burst test - exceed rate limit
    burst: {
      executor: 'constant-arrival-rate',
      rate: 200,              // 200 requests per second
      timeUnit: '1s',
      duration: '30s',
      preAllocatedVUs: 50,
      maxVUs: 100,
    },
  },
  thresholds: {
    // We expect some 429s, but not too many
    'successful_requests': ['count>1000'],
  },
};

export default function() {
  const res = http.get(`${BASE_URL}/`);

  if (res.status === 429) {
    rateLimitedRequests.add(1);
    check(res, {
      'rate limit response has Retry-After': (r) => 
        r.headers['Retry-After'] !== undefined,
    });
  } else if (res.status === 200) {
    successfulRequests.add(1);
  }

  // Tiny sleep to let token bucket refill
  sleep(0.01);
}

export function handleSummary(data) {
  const rateLimited = data.metrics.rate_limited_requests?.values.count || 0;
  const successful = data.metrics.successful_requests?.values.count || 0;
  const total = rateLimited + successful;

  console.log('\n=== Rate Limiter Test Results ===');
  console.log(`Total Requests: ${total}`);
  console.log(`Successful (200): ${successful} (${((successful/total)*100).toFixed(1)}%)`);
  console.log(`Rate Limited (429): ${rateLimited} (${((rateLimited/total)*100).toFixed(1)}%)`);
  console.log('');

  return {
    'k6/rate-limit-summary.json': JSON.stringify(data),
  };
}
