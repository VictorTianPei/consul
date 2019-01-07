package cachetype

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hashicorp/consul/lib"
	"github.com/y0ssar1an/q"

	"github.com/hashicorp/consul/agent/cache"
	"github.com/hashicorp/consul/agent/connect"
	"github.com/hashicorp/consul/agent/structs"
)

// Recommended name for registration.
const ConnectCALeafName = "connect-ca-leaf"

// caChangeInitialJitter is the jitter we apply after noticing the CA changed
// before requesting a new cert. Since we don't know how many services are in
// the cluster we can't be too smart about setting this so it's a tradeoff
// between not making root rotations take unnecessarily long on small clusters
// and not hammering the servers to hard on large ones. Note that server's will
// soon have CSR rate limiting that will limit the impact on big clusters, but a
// small spread in the initial requests still seems like a good idea and limits
// how many clients will hit the rate limit.
const caChangeInitialJitter = 20 * time.Second

// ConnectCALeaf supports fetching and generating Connect leaf
// certificates.
type ConnectCALeaf struct {
	caIndex uint64 // Current index for CA roots

	// rootWatchMu protects access to the rootWatchSubscribers map and
	// rootWatchCancel
	rootWatchMu sync.Mutex
	// rootWatchSubscribers is a set of chans, one for each currently in-flight
	// Fetch. These chans have root updates delivered from the root watcher.
	rootWatchSubscribers map[chan struct{}]struct{}
	// rootWatchCancel is a func to call to stop the background root watch if any.
	// You must hold inflightMu to read (e.g. call) or write the value.
	rootWatchCancel func()

	// testSetCAChangeInitialJitter allows overriding the caChangeInitialJitter in
	// tests.
	testSetCAChangeInitialJitter time.Duration

	RPC        RPC          // RPC client for remote requests
	Cache      *cache.Cache // Cache that has CA root certs via ConnectCARoot
	Datacenter string       // This agent's datacenter
}

// fetchState is some additional metadata we store with each cert in the cache
// to track things like expiry and coordinate paces root rotations.
type fetchState struct {
	// authorityKeyID is the key ID of the CA root that signed the current cert.
	// This is just to save parsing the whole cert everytime we have to check if
	// the root changed.
	authorityKeyID string

	// forceExpireAfter is used to coordinate renewing certs after a CA rotation
	// in a staggered way so that we don't overwhelm the servers.
	forceExpireAfter time.Time
}

func (c *ConnectCALeaf) fetchStart(rootUpdateCh chan struct{}) {
	c.rootWatchMu.Lock()
	defer c.rootWatchMu.Unlock()
	// Lazy allocation
	if c.rootWatchSubscribers == nil {
		c.rootWatchSubscribers = make(map[chan struct{}]struct{})
	}
	// Make sure a root watcher is running. We don't only do this on first request
	// to be more tolerant of errors that could cause the root watcher to fail and
	// exit.
	c.ensureRootWatcher()
	c.rootWatchSubscribers[rootUpdateCh] = struct{}{}
}

func (c *ConnectCALeaf) fetchDone(rootUpdateCh chan struct{}) {
	c.rootWatchMu.Lock()
	defer c.rootWatchMu.Unlock()
	delete(c.rootWatchSubscribers, rootUpdateCh)
	if len(c.rootWatchSubscribers) == 0 && c.rootWatchCancel != nil {
		// This is was the last request. Stop the root watcher
		c.rootWatchCancel()
	}
}

// ensureRootWatcher must be called while holding c.inflightMu it starts a
// background root watcher if there isn't already one running.
func (c *ConnectCALeaf) ensureRootWatcher() {
	if c.rootWatchCancel == nil {
		ctx, cancel := context.WithCancel(context.Background())
		c.rootWatchCancel = cancel
		go c.rootWatcher(ctx)
	}
}

func (c *ConnectCALeaf) rootWatcher(ctx context.Context) {
	ch := make(chan cache.UpdateEvent, 1)
	err := c.Cache.Notify(ctx, ConnectCARootName, &structs.DCSpecificRequest{
		Datacenter: c.Datacenter,
	}, "roots", ch)

	if err != nil {
		// TODO(banks): Hmm we should probably plumb a logger through here at least.
		// No good place to put this otherwise. At least next Fetch will retry. We
		// could plumb it back through to all current Fetches and fail them with the
		// error I guess?
		return
	}

	var oldRoots *structs.IndexedCARoots
	// Wait for updates to roots or all requests to stop
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-ch:
			// Root response changed in some way. Note this might be the initial
			// fetch.
			if e.Err != nil {
				// TODO(banks): should we pass this on to clients? Feels like if it's a
				// temporary issue and we recover we will have shown an error to leaf
				// watchers for nothing. On the other hand all clients watching leafs
				// will also be watching roots too so probably already saw the same
				// error from their own roots watch.
				continue
			}

			roots, ok := e.Result.(*structs.IndexedCARoots)
			if !ok {
				// Shouldn't happen. Error handling as above.
				continue
			}

			q.Q(roots)

			// Check that the active root is actually different from the last CA
			// config there are many reasons the config might have changed without
			// actually updating the CA root that is signing certs in the cluster.
			// The Fetch calls will also validate this since the first call here we
			// don't know if it changed or not, but there is no point waking up all
			// Fetch calls to check this if we know none of them will need to act on
			// this update.
			if oldRoots != nil && oldRoots.ActiveRootID == roots.ActiveRootID {
				continue
			}

			// Distribute the update to all inflight requests - they will decide
			// whether or not they need to act on it.
			c.rootWatchMu.Lock()
			for ch := range c.rootWatchSubscribers {
				select {
				case ch <- struct{}{}:
				default:
					// Don't block - chans are 1-buffered so act as an edge trigger and
					// reload CA state directly from cache so they never "miss" updates.
				}
			}
			c.rootWatchMu.Unlock()
			oldRoots = roots
		}
	}
}

// calculateSoftExpiry encapsulates our logic for when to renew a cert based on
// it's age. It returns a pair of times min, max which makes it easier to test
// the logic without non-determinisic jitter to account for. The caller should choose a time randomly in between these.
//
// We want to balance a few factors here:
//   - renew too early and it increases the aggregate CSR rate in the cluster
//   - renew too late and it risks disruption to the service if a transient
//     error prevents the renewal
//   - we want a broad amount of jitter so if there is an outage, we don't end
//     up with all services in sync and causing a thundering herd every
//     renewal period. Broader is better for smoothing requests but pushes
//     both earlier and later tradeoffs above.
//
// Somewhat arbitrarily the current strategy looks like this:
//
//          0                              60%             90%
//   Issued [------------------------------|===============|!!!!!] Expires
// 72h TTL: 0                             ~43h            ~65h
//  1h TTL: 0                              36m             54m
//
// Where |===| is the soft renewal period where we jitter for the first attempt
// and |!!!| is the danger zone where we just try immediately.
//
// In the happy path (no outages) the average renewal occurs half way through
// the soft renewal region or at 75% of the cert lifetime which is ~54 hours for
// a 72 hour cert, or 45 mins for a 1 hour cert.
//
// If we are already in the softRenewal period, we randomly pick a time between
// now and the start of the danger zone.
//
// We pass in now to make testing easier.
func calculateSoftExpiry(now time.Time, cert *structs.IssuedCert) (min time.Time, max time.Time) {

	certLifetime := cert.ValidBefore.Sub(cert.ValidAfter)
	if certLifetime < 10*time.Minute {
		// Shouldn't happen as we limit to 1 hour shortest elsewhere but just be
		// defensive against strange times or bugs.
		return now, now
	}

	// Find the 60% mark in diagram above
	softRenewTime := cert.ValidAfter.Add(time.Duration(float64(certLifetime) * 0.6))
	hardRenewTime := cert.ValidAfter.Add(time.Duration(float64(certLifetime) * 0.9))

	if now.After(hardRenewTime) {
		// In the hard renew period, or already expired. Renew now!
		return now, now
	}

	if now.After(softRenewTime) {
		// Already in the soft renew period, make now the lower bound for jitter
		softRenewTime = now
	}
	return softRenewTime, hardRenewTime
}

func (c *ConnectCALeaf) Fetch(opts cache.FetchOptions, req cache.Request) (cache.FetchResult, error) {
	var result cache.FetchResult

	// Get the correct type
	reqReal, ok := req.(*ConnectCALeafRequest)
	if !ok {
		return result, fmt.Errorf(
			"Internal cache failure: request wrong type: %T", req)
	}

	// Do we already have a cert in the cache?
	var existing *structs.IssuedCert
	var state *fetchState
	if opts.LastResult != nil {
		existing, ok = opts.LastResult.Value.(*structs.IssuedCert)
		if !ok {
			return result, fmt.Errorf(
				"Internal cache failure: last value wrong type: %T", req)
		}
		state, ok = opts.LastResult.State.(*fetchState)
		if !ok {
			return result, fmt.Errorf(
				"Internal cache failure: last state wrong type: %T", req)
		}
	} else {
		state = &fetchState{}
	}

	// Handle brand new request first as it's simplest. Note that either both or
	// neither of these should be nil but we check both just to be defensive and
	// confident we won't have a nil pointer for the remainder of this function.
	if existing == nil || state == nil {
		return c.generateNewLeaf(reqReal, state)
	}

	// Make a chan we can be notified of changes to CA roots on. It must be
	// buffered so we don't miss broadcasts from rootsWatch. It is an edge trigger
	// so a single element is sufficient regardless of whether we consume the
	// updates fast enough since as soon as we see an element in it, we will
	// reload latest CA from cache.
	rootUpdateCh := make(chan struct{}, 1)

	// We are now inflight. Add our state to the map and defer removing it. This
	// may trigger background root watcher too.
	c.fetchStart(rootUpdateCh)
	defer c.fetchDone(rootUpdateCh)

	// We have a certificate in cache already. Check it's still valid.
	now := time.Now()
	minExpire, maxExpire := calculateSoftExpiry(now, existing)
	expiresAt := minExpire.Add(lib.RandomStagger(maxExpire.Sub(minExpire)))

	// Check if we have be force-expired by the root watcher because there is a
	// new CA root.
	if !state.forceExpireAfter.IsZero() && state.forceExpireAfter.Before(expiresAt) {
		expiresAt = state.forceExpireAfter
	}
	q.Q(expiresAt.String(), now.String())

	if expiresAt == now || expiresAt.Before(now) {
		// Already expired, just make a new one right away
		return c.generateNewLeaf(reqReal, state)
	}

	// Setup result to mirror the current value for if we timeout. This allows us
	// to update the state even if we don't generate a new cert.
	result.Value = existing
	result.Index = existing.ModifyIndex
	result.State = state

	// Current cert is valid so just wait until it expires or we time out.
	for {
		select {
		case <-time.After(opts.Timeout):
			// We timed out the request with same cert.
			return result, nil
			// Note that we don't use now in the case below because this might be hit
			// on a loop several minutes into the blocking request so recalculating
			// the delay based on when the request started would be wrong!
		case <-time.After(expiresAt.Sub(time.Now())):
			// Cert expired or was force-expired by a root change.
			return c.generateNewLeaf(reqReal, state)
		case <-rootUpdateCh:
			// A roots cache change occurred, reload them from cache.
			roots, err := c.rootsFromCache()
			if err != nil {
				return result, err
			}

			// Handle _possibly_ changed roots. We still need to verify the new active
			// root is not the same as the one our current cert was signed by since we
			// can be notified spuriously if we are the first request since the
			// rootsWatcher didn't know about the CA we were signed by.
			if activeRootHasKey(roots, state.authorityKeyID) {
				// Current active CA is the same one that signed our current cert so
				// keep waiting for a change.
				continue
			}
			// CA root changed. We add some jitter here to avoid a thundering herd.
			// The servers will be rate limited but we can still smooth this out over
			// a small time to limit number of requests that get made at once that
			// will likely hit the rate limit. We can't be too smart though since we
			// don't know how many outstanding certs there are in the whole cluster so
			// we just have to pick something better than no jitter at all and rely on
			// rate limiting for the rest. For now spread the initial requests over 30
			// seconds. Which means small clusters should still rotate in around 30
			// seconds but large ones will not be so badly hammered initially.
			jitter := caChangeInitialJitter
			if c.testSetCAChangeInitialJitter > 0 {
				jitter = c.testSetCAChangeInitialJitter
			}
			delay := lib.RandomStagger(jitter)
			// Force the cert to be expired after the jitter - the delay above might
			// be longer than we have left on out timeout so we might return to the
			// caller before we expire. We set forceExpireAfter in the cache state so
			// the next request will notice we still need to renew and do it at the
			// right time.
			state.forceExpireAfter = time.Now().Add(delay)
			// If that time is within the current timeout though, we want to renew the
			// cert right now. This ensures that when we loop back around, we'll wait
			// at most delay until generating a new cert, or will timeout and do it
			// next time based on the state.
			if state.forceExpireAfter.Before(expiresAt) {
				expiresAt = state.forceExpireAfter
			}
			continue
		}
	}
}

func activeRootHasKey(roots *structs.IndexedCARoots, currentSigningKeyID string) bool {
	for _, ca := range roots.Roots {
		if ca.Active {
			if ca.SigningKeyID == currentSigningKeyID {
				return true
			}
			// Found the active CA but it has changed
			return false
		}
	}
	// Shouldn't be possible since at least one root should be active.
	return false
}

func (c *ConnectCALeaf) rootsFromCache() (*structs.IndexedCARoots, error) {
	rawRoots, _, err := c.Cache.Get(ConnectCARootName, &structs.DCSpecificRequest{
		Datacenter: c.Datacenter,
	})
	if err != nil {
		return nil, err
	}
	roots, ok := rawRoots.(*structs.IndexedCARoots)
	if !ok {
		return nil, errors.New("invalid RootCA response type")
	}
	return roots, nil
}

// generateNewLeaf does the actual work of creating a new private key,
// generating a CSR and getting it signed by the servers.
func (c *ConnectCALeaf) generateNewLeaf(req *ConnectCALeafRequest, state *fetchState) (cache.FetchResult, error) {
	var result cache.FetchResult

	// Need to lookup RootCAs response to discover trust domain. First just lookup
	// with no blocking info - this should be a cache hit most of the time.
	roots, err := c.rootsFromCache()
	if err != nil {
		return result, err
	}
	if roots.TrustDomain == "" {
		return result, errors.New("cluster has no CA bootstrapped yet")
	}

	// Build the service ID
	serviceID := &connect.SpiffeIDService{
		Host:       roots.TrustDomain,
		Datacenter: req.Datacenter,
		Namespace:  "default",
		Service:    req.Service,
	}

	// Create a new private key
	pk, pkPEM, err := connect.GeneratePrivateKey()
	if err != nil {
		return result, err
	}

	// Create a CSR.
	csr, err := connect.CreateCSR(serviceID, pk)
	if err != nil {
		return result, err
	}

	// Request signing
	var reply structs.IssuedCert
	args := structs.CASignRequest{
		WriteRequest: structs.WriteRequest{Token: req.Token},
		Datacenter:   req.Datacenter,
		CSR:          csr,
	}
	if err := c.RPC.RPC("ConnectCA.Sign", &args, &reply); err != nil {
		return result, err
	}
	reply.PrivateKeyPEM = pkPEM

	// Reset the forcedExpiry in the state
	state.forceExpireAfter = time.Time{}

	cert, err := connect.ParseCert(reply.CertPEM)
	if err != nil {
		return result, err
	}
	// Set the CA key ID so we can easily tell when a active root has changed.
	state.authorityKeyID = connect.HexString(cert.AuthorityKeyId)

	result.Value = &reply
	result.State = state
	result.Index = reply.ModifyIndex
	return result, nil
}

func (c *ConnectCALeaf) SupportsBlocking() bool {
	return true
}

// ConnectCALeafRequest is the cache.Request implementation for the
// ConnectCALeaf cache type. This is implemented here and not in structs
// since this is only used for cache-related requests and not forwarded
// directly to any Consul servers.
type ConnectCALeafRequest struct {
	Token         string
	Datacenter    string
	Service       string // Service name, not ID
	MinQueryIndex uint64
}

func (r *ConnectCALeafRequest) CacheInfo() cache.RequestInfo {
	return cache.RequestInfo{
		Token:      r.Token,
		Key:        r.Service,
		Datacenter: r.Datacenter,
		MinIndex:   r.MinQueryIndex,
	}
}
