package app

// Login throttling.
//
// POST /auth/login is public and unauthenticated. With local accounts that was survivable: a failed
// guess cost an attacker a bcrypt compare and cost us nothing. With directory auth it is not - every
// failed login is a real bad-password bind against a real Active Directory account, and AD lockout
// policies trip at 3–5 failures. Without a counter in front of it, anyone who can reach the portal
// can script a name list and lock out every account in the domain. That is a denial of service
// against the company, delivered through a control plane that has no business being able to do it.
//
// So: count failures, refuse to forward a bind once the count is up, and keep our threshold well
// under AD's own. The platform must never be the thing that locks an account.
//
// Two independent scopes, because they defend different things:
//
//	user - the load-bearing one. Protects a given account from being locked, and holds even when
//	       the attacker rotates source addresses.
//	ip   - catches one source spraying MANY usernames, which the per-user counter never sees.
//
// Either one tripping refuses the attempt. The per-IP key comes from a client-controlled header
// when the portal sits behind nginx (see api.clientIP), so it is a refinement, not a guarantee -
// which is exactly why the per-username counter is the one that has to hold.
//
// Counters live in Postgres, not in memory, because the api tier scales horizontally: an in-process
// map would hand an attacker one full allowance per replica and reset on every deploy (CLAUDE.md:
// "nothing is pinned to a replica").
//
// Production would do better than this: an exponential backoff rather than a fixed window, a
// CAPTCHA or a second factor on the way out of it, and an alert on the spray pattern rather than a
// silent 429.

import (
	"time"

	"github.com/Daniel-Vaz/KaaS-demo/internal/store"
)

// throttleConfig is the tunable half.
//
// The two scopes need DIFFERENT thresholds, and this is not a detail - one number for both is wrong
// in whichever direction you pick it:
//
//   - maxUser is per account, and wants to be small: 3 sits below every default AD lockout
//     threshold, so a user who fatfingers their password meets our 429 (which forgets them in five
//     minutes) instead of AD's lockout (which needs an administrator).
//   - maxIP is per source address, and CANNOT be 3. A whole office behind one NAT egress shares it,
//     so three unrelated typos across three colleagues would lock the building out of the portal.
//     It exists to catch one host spraying many usernames - a shape that looks nothing like normal
//     traffic - so it is set an order of magnitude higher and still catches that.
type throttleConfig struct {
	maxUser int
	maxIP   int
	window  time.Duration
}

// maxFor returns the threshold for one scope.
func (c throttleConfig) maxFor(scope string) int {
	if scope == store.ThrottleScopeIP {
		return c.maxIP
	}
	return c.maxUser
}

type throttler struct {
	store store.Store
	cfg   throttleConfig
	log   interface{ Warn(string, ...any) }
}

func (a *App) throttle() *throttler {
	return &throttler{
		store: a.Store,
		cfg: throttleConfig{
			maxUser: envInt("KAAS_LDAP_MAX_FAILURES", 3),
			maxIP:   envInt("KAAS_LDAP_MAX_IP_FAILURES", 30),
			window:  envDuration("KAAS_LDAP_THROTTLE_WINDOW", 5*time.Minute),
		},
		log: a.Log,
	}
}

// check reports ErrTooManyAttempts if either counter has tripped. Call it BEFORE the directory
// round trip - the whole point is to not send the bind at all.
//
// A store failure here is NOT fatal: it fails open, deliberately. This runs in front of every
// login, and a throttle that can't read its own table must not become the reason nobody can
// authenticate. The exposure is bounded - a broken database is already an outage - and it is the
// lesser of the two failures.
func (t *throttler) check(username, clientIP string) error {
	for _, s := range t.scopes(username, clientIP) {
		count, start, err := t.store.LoginFailures(s.scope, s.key)
		if err != nil {
			t.log.Warn("login throttle unreadable, allowing the attempt", "scope", s.scope, "err", err)
			continue
		}
		if count < t.cfg.maxFor(s.scope) {
			continue
		}
		// An expired window is a stale count, not a trip. RecordLoginFailure resets the window
		// lazily on the next failure, so a counter that tripped and then went quiet has to be read
		// as expired here or it would never let the user back in.
		if time.Since(start) > t.cfg.window {
			continue
		}
		t.log.Warn("login throttled", "scope", s.scope, "key", s.key,
			"failures", count, "max", t.cfg.maxFor(s.scope), "window", t.cfg.window)
		return ErrTooManyAttempts
	}
	return nil
}

// recordFailure counts a genuine credential rejection. Only ever called for a wrong password - an
// unreachable directory must not count against the user, and neither must an unknown username
// (which never reaches a bind; see the ldap client's findUser).
func (t *throttler) recordFailure(username, clientIP string) {
	for _, s := range t.scopes(username, clientIP) {
		if err := t.store.RecordLoginFailure(s.scope, s.key, t.cfg.window); err != nil {
			t.log.Warn("could not record login failure", "scope", s.scope, "err", err)
		}
	}
}

// reset clears the counters after a successful login, so a user who eventually remembers their
// password isn't left sitting out the rest of the window.
func (t *throttler) reset(username, clientIP string) {
	for _, s := range t.scopes(username, clientIP) {
		if err := t.store.ResetLoginFailures(s.scope, s.key); err != nil {
			t.log.Warn("could not reset login failures", "scope", s.scope, "err", err)
		}
	}
}

type throttleScope struct{ scope, key string }

// scopes is the (scope, key) pairs for one attempt. The IP scope is dropped when the address is
// unknown, rather than counted under an empty key - that would pool every unknown-address attempt
// into one shared counter and let one attacker throttle everybody else.
func (t *throttler) scopes(username, clientIP string) []throttleScope {
	out := []throttleScope{{store.ThrottleScopeUser, username}}
	if clientIP != "" {
		out = append(out, throttleScope{store.ThrottleScopeIP, clientIP})
	}
	return out
}

// pruneThrottleCounters drops expired rows. Unbounded otherwise: the key space is "every username
// anyone has ever guessed at", which on a public endpoint is attacker-controlled.
//
// Runs under the leader lease rather than per-replica - it is a periodic sweep, and CLAUDE.md is
// explicit that those are leader-elected (see internal/reconcile/leader.go). The in-memory store
// needs none of this; it dies with the process.
func (a *App) pruneThrottleCounters() error {
	pruner, ok := a.Store.(interface {
		PruneLoginFailures(time.Duration) (int64, error)
	})
	if !ok {
		return nil
	}
	window := envDuration("KAAS_LDAP_THROTTLE_WINDOW", 5*time.Minute)
	n, err := pruner.PruneLoginFailures(window)
	if err != nil {
		return err
	}
	if n > 0 {
		a.Log.Debug("pruned expired login-throttle counters", "rows", n)
	}
	return nil
}
