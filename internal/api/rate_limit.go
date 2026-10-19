package api

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Cloud Run is configured for one API instance. These counters are intentionally
// process-local; scaling out requires a shared limiter before raising max instances.
const maxRateBuckets = 10000

type rateBucket struct {
	tokens float64
	last   time.Time
}

type rateRule struct {
	key   string
	rate  float64 // tokens per second
	burst float64
}

type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]rateBucket
	lastGC  time.Time
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{buckets: make(map[string]rateBucket)}
}

// allow evaluates all limits together: a rejected global request must not
// consume the caller's IP or session allowance.
func (l *rateLimiter) allow(now time.Time, rules ...rateRule) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastGC) >= time.Minute {
		for key, bucket := range l.buckets {
			if now.Sub(bucket.last) >= 2*time.Minute {
				delete(l.buckets, key)
			}
		}
		l.lastGC = now
	}
	wait := 0
	for _, rule := range rules {
		bucket, exists := l.buckets[rule.key]
		if !exists && len(l.buckets) >= maxRateBuckets {
			if wait < 60 {
				wait = 60
			}
			continue
		}
		if exists {
			elapsed := now.Sub(bucket.last).Seconds()
			if elapsed > 0 {
				bucket.tokens += elapsed * rule.rate
				if bucket.tokens > rule.burst {
					bucket.tokens = rule.burst
				}
			}
		} else {
			bucket.tokens = rule.burst
		}
		bucket.last = now
		l.buckets[rule.key] = bucket
		if bucket.tokens < 1 {
			seconds := int((1 - bucket.tokens) / rule.rate)
			if float64(seconds)*rule.rate < 1-bucket.tokens {
				seconds++
			}
			if seconds > wait {
				wait = seconds
			}
		}
	}
	if wait != 0 {
		return wait
	}
	for _, rule := range rules {
		bucket := l.buckets[rule.key]
		bucket.tokens--
		l.buckets[rule.key] = bucket
	}
	return 0
}

// A forwarding header is trusted only together with the configured edge
// secret, checked by middleware before this handler runs. Direct clients and
// missing/invalid forwarded values use the transport peer, never XFF.
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.EdgeSharedSecret != "" &&
		subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Trace-Edge-Secret")), []byte(s.cfg.EdgeSharedSecret)) == 1 {
		if values := r.Header.Values("X-Trace-Client-IP"); len(values) == 1 {
			if ip := net.ParseIP(values[0]); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		if ip := net.ParseIP(host); ip != nil {
			return ip.String()
		}
	}
	return "unknown"
}

func (s *Server) limitedPublic(kind string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := s.clientIP(r)
		wait := s.limiter.allow(s.now(),
			rateRule{key: kind + "/ip/" + ip, rate: 1, burst: 60},
			rateRule{key: kind + "/global", rate: 10, burst: 10},
			rateRule{key: "api/global", rate: 100, burst: 100})
		if wait > 0 {
			s.rateLimited(w, r, wait)
			return
		}
		next(w, r)
	}
}

func (s *Server) limitedSession(kind string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Normalize the same Bearer syntax as authentication; changes in
		// scheme case or surrounding whitespace must not reset the quota.
		ip := s.clientIP(r)
		token, err := bearerToken(r)
		if err != nil {
			token = "invalid/" + ip
		}
		// A forged token cannot drain a known participant's allowance.
		identity := hash(token)
		rate, burst := 0.5, 30.0
		if kind == "position" {
			rate, burst = 0.2, 5
		}
		budgetKind := kind
		if kind == "experience-delete" {
			budgetKind = "session"
		}
		rules := [4]rateRule{
			{key: budgetKind + "/" + identity, rate: rate, burst: burst},
			{key: "authenticated/ip/" + ip, rate: 20, burst: 120},
			{key: "api/global", rate: 100, burst: 100},
		}
		count := 3
		if liveToken := r.Header.Get("X-Trace-Live-Token"); kind == "experience-delete" && liveToken != "" {
			if liveIdentity := hash(liveToken); liveIdentity != identity {
				rules[count] = rateRule{key: "session/" + liveIdentity, rate: 0.5, burst: 30}
				count++
			}
		}
		if wait := s.limiter.allow(s.now(), rules[:count]...); wait > 0 {
			s.rateLimited(w, r, wait)
			return
		}
		next(w, r)
	}
}

func (s *Server) rateLimited(w http.ResponseWriter, r *http.Request, wait int) {
	w.Header().Set("Retry-After", strconv.Itoa(wait))
	s.writeError(w, requestID(r), &apiError{Code: "RATE_LIMITED", Message: "rate limit exceeded", Status: http.StatusTooManyRequests})
}
