// retry of a keyed mutating call the server answered 503 (no capacity, no
// host, restate down). mirrors sdks/go/retry.go.

use std::time::Duration;

/// How many times a keyed call answered 503 is sent again by default.
pub(crate) const DEFAULT_RETRIES: u32 = 3;

// backoff of a 503 without Retry-After: 1s doubling, capped at 30s.
const RETRY_BASE: Duration = Duration::from_secs(1);
const RETRY_CAP: Duration = Duration::from_secs(30);
// a server's Retry-After is capped at a minute.
const MAX_RETRY_AFTER: Duration = Duration::from_secs(60);

// retry_delay is how long to wait before retry number attempt (from 0) of a
// 503 whose Retry-After header is retry_after: the header's seconds when it
// has them, else the backoff, plus up to half again as jitter. unit is the
// jitter's random fraction in [0, 1). jitter only ever adds, so the wait
// never undercuts the server's Retry-After.
pub(crate) fn retry_delay(retry_after: Option<&str>, attempt: u32, unit: f64) -> Duration {
    let mut d = RETRY_BASE
        .saturating_mul(1 << attempt.min(10))
        .min(RETRY_CAP);
    if let Some(secs) = retry_after.and_then(|v| v.trim().parse::<u64>().ok()) {
        d = Duration::from_secs(secs).min(MAX_RETRY_AFTER);
    }
    d + d.mul_f64(unit.clamp(0.0, 1.0) / 2.0)
}

// jitter is a random fraction in [0, 1), from the same source as the
// idempotency keys.
pub(crate) fn jitter() -> f64 {
    (uuid::Uuid::new_v4().as_u128() >> 75) as f64 / (1u64 << 53) as f64
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn honors_retry_after_plus_at_most_half() {
        assert_eq!(retry_delay(Some("2"), 0, 0.0), Duration::from_secs(2));
        let d = retry_delay(Some("2"), 3, 0.999);
        assert!(
            d >= Duration::from_secs(2) && d <= Duration::from_secs(3),
            "{d:?}"
        );
        assert_eq!(retry_delay(Some("3600"), 0, 0.0), Duration::from_secs(60));
    }

    #[test]
    fn backs_off_without_retry_after() {
        assert_eq!(retry_delay(None, 0, 0.0), Duration::from_secs(1));
        assert_eq!(retry_delay(None, 2, 0.0), Duration::from_secs(4));
        assert_eq!(retry_delay(None, 20, 0.0), Duration::from_secs(30));
    }

    #[test]
    fn jitter_is_a_fraction() {
        for _ in 0..100 {
            let j = jitter();
            assert!((0.0..1.0).contains(&j), "{j}");
        }
    }
}
