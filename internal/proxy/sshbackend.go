// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

// SSHBackend forwards to a replica's engine port through an SSH tunnel,
// taking dstack's own service proxy OUT of the request path.
//
// Why this exists, measured 2026-08-28 against a live Vast.ai GPU with the
// same prompt and concurrency minutes apart:
//
//	           via dstack server          direct over SSH
//	conc 32    746 tok/s                  1010 tok/s
//	conc 128   97/128 ok, 31x HTTP 500    128/128 ok, 1857 tok/s
//	conc 256   -                          256/256 ok, 2106 tok/s
//
// dstack's proxy exposes its repository and auth as FastAPI `yield`
// dependencies, torn down only after the response completes, so every
// streamed generation pins TWO database connections for its whole lifetime.
// Its connection pool, not the GPU, is the ceiling: the 31 failures above
// coincided with pg_stat_activity hitting poolSize+maxOverflow+1 exactly.
//
// Head-of-line blocking was the design's main risk — SSH multiplexes every
// stream onto one TCP connection — and it was measured away: at 128
// concurrent, one connection gave 1857 tok/s against 1876 for eight. 1%. So
// this keeps ONE connection per replica and does not pool.
//
// Fallback is not an afterthought: any model without a usable endpoint, and
// any failure to dial, defers to Inner. The direct path is an optimisation,
// and an optimisation that can fail a request is a bug.
type SSHBackend struct {
	// Inner carries every request this backend cannot. Never nil in
	// production — StatusBackend, which works for every topology.
	Inner Backend

	// Cache is where status.replica is read from, per request.
	Cache *ModelCache

	// LoadSigner fetches squall's own private key. Its public half was put
	// into the run spec at Apply time, which is why the replica trusts it:
	// dstack's vastai backend authorises run_spec.ssh_key_pub alongside its
	// own project key.
	//
	// A FUNCTION rather than a value because of a startup race that is
	// otherwise unfixable: the controller mints the keypair, and on a fresh
	// install squall-proxy can easily start first. Loading once at boot would
	// leave the proxy permanently on the slow path until someone restarted
	// it, for no reason a reader could see. This retries until it succeeds,
	// then caches. Nil, or an error every time, simply means no direct path.
	LoadSigner func() (ssh.Signer, error)

	signer ssh.Signer

	// DialTimeout bounds one SSH handshake. Zero means 15s.
	DialTimeout time.Duration

	// DialBackoff is how long a FAILED dial is remembered before another is
	// attempted. Zero means 30s.
	DialBackoff time.Duration

	mu     sync.Mutex
	conns  map[string]*tunnel
	failed map[string]failedDial

	// hostMu guards hostKeys SEPARATELY from mu, and must stay separate: the
	// host-key callback runs INSIDE ssh.Dial, which is called without mu held
	// precisely so a 15s handshake cannot block every other model's requests.
	// Guarding both with one mutex deadlocks on the first successful dial,
	// since Go mutexes are not reentrant.
	hostMu sync.Mutex

	// hostKeys is trust-on-first-use pinning, keyed by host:port. dstack
	// publishes no host key for a replica, so there is nothing to verify
	// against on the FIRST connection — but every later one is checked
	// against what was seen then, which closes the window where a swapped
	// endpoint could silently intercept traffic. This is weaker than a known
	// key and much stronger than InsecureIgnoreHostKey, which the prototype
	// used and which must never ship: §12.3 already treats a marketplace host
	// as untrusted, and an unpinned tunnel is exactly its MITM surface.
	hostKeys map[string]string
}

// failedDial remembers that dialling ONE endpoint failed, so the hot path
// stops retrying it. Scoped to the endpoint rather than the model so that
// replacing the replica clears it by construction.
type failedDial struct {
	endpoint ReplicaEndpoint
	until    time.Time
}

// tunnel is one live SSH connection to one replica, plus the HTTP client
// whose transport dials through it.
type tunnel struct {
	endpoint ReplicaEndpoint
	client   *ssh.Client
	http     *http.Client

	// refs counts responses still streaming over this connection; retired
	// marks a tunnel no longer handed out (endpoint changed, or shutdown).
	// A retired tunnel closes when its LAST stream ends — never before
	// (D113): every generation for a model multiplexes onto this one
	// connection, and a synchronous Close on an endpoint change killed all
	// of them mid-token. A routing decision must not terminate active work.
	refs    atomic.Int64
	retired atomic.Bool
}

// release drops one stream's reference and closes a retired tunnel whose
// last stream just ended.
func (t *tunnel) release() {
	if t.refs.Add(-1) == 0 && t.retired.Load() {
		_ = t.client.Close()
	}
}

// retire stops a tunnel from being handed out and closes it as soon as it
// is idle. If a stream sneaks in between the idle check and the close, its
// own release performs the close instead; the worst race outcome is a
// retryable transport error, never a half-dead shared connection.
func (t *tunnel) retire() {
	t.retired.Store(true)
	if t.refs.Load() == 0 {
		_ = t.client.Close()
	}
}

// refTransport wraps the tunnel's transport so every in-flight response
// holds a reference until its body is closed — the bookkeeping retire()
// and release() coordinate over.
type refTransport struct {
	t  *tunnel
	rt http.RoundTripper
}

func (r *refTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.t.refs.Add(1)
	resp, err := r.rt.RoundTrip(req)
	if err != nil {
		r.t.release()
		return nil, err
	}
	resp.Body = &releaseBody{ReadCloser: resp.Body, release: r.t.release}
	return resp, nil
}

// releaseBody releases its tunnel reference exactly once, on Close.
type releaseBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (rb *releaseBody) Close() error {
	err := rb.ReadCloser.Close()
	rb.once.Do(rb.release)
	return err
}

// URL reports where a model's traffic goes. The returned URL's host is a
// placeholder: the transport ignores it and dials the tunnel instead. Only
// the scheme and path shape matter to the caller.
func (b *SSHBackend) URL(model string) (*url.URL, bool) {
	if b.tunnelFor(model) == nil {
		return b.Inner.URL(model)
	}
	return &url.URL{Scheme: "http", Host: "replica"}, true
}

// Client reports the tunnelled transport for model, or (nil, false) when
// the direct path is unavailable. Production forwards never call this —
// they resolve URL and transport atomically through Route (D121); this
// stays as the plain accessor for anything that only needs the transport.
func (b *SSHBackend) Client(model string) (*http.Client, bool) {
	t := b.tunnelFor(model)
	if t == nil {
		return nil, false
	}
	return t.http, true
}

// Route implements RouteBackend: the URL and its transport from ONE tunnel
// resolution (D121). Two independent lookups allowed a cache update to
// pair dstack's proxy path with the SSH transport, or the placeholder host
// with http.DefaultClient — both silently wrong.
func (b *SSHBackend) Route(model string) (*url.URL, *http.Client, bool) {
	t := b.tunnelFor(model)
	if t == nil {
		u, ok := b.Inner.URL(model)
		return u, nil, ok
	}
	return &url.URL{Scheme: "http", Host: "replica"}, t.http, true
}

// tunnelFor returns a live tunnel for model, dialling one if needed, or nil
// when the direct path is unavailable for ANY reason.
func (b *SSHBackend) tunnelFor(model string) *tunnel {
	if b == nil || b.Cache == nil {
		return nil
	}
	signer := b.resolveSigner()
	if signer == nil {
		return nil
	}
	snap, ok := b.Cache.Get(model)
	if !ok || snap.Replica == nil {
		return nil
	}
	want := *snap.Replica

	backoff := b.DialBackoff
	if backoff <= 0 {
		backoff = 30 * time.Second
	}

	b.mu.Lock()
	if b.conns == nil {
		b.conns = map[string]*tunnel{}
	}
	if b.failed == nil {
		b.failed = map[string]failedDial{}
	}
	// A dial that failed usually failed STRUCTURALLY — measured live: a replica
	// provisioned before squall had a key rejects every handshake it will ever
	// be offered, and retrying per request cost 64 pointless TCP+crypto
	// handshakes on the hot path in a few minutes. Remember the failure and
	// let the fallback carry traffic until the backoff expires.
	//
	// The recorded ENDPOINT is part of the condition, not just the model: a
	// rescheduled replica is a different machine, and a failure about the old
	// one says nothing about it. Comparing only the model would strand a
	// freshly provisioned replica on the fallback for a whole backoff.
	if f, ok := b.failed[model]; ok && f.endpoint == want && time.Now().Before(f.until) {
		b.mu.Unlock()
		return nil
	}
	// A changed endpoint means a NEW replica: the old connection points at a
	// machine that is no longer this model's, so it must be dropped rather
	// than reused. This is the same reasoning that makes the controller filter
	// job submissions by deployment_num (D46).
	//
	// RETIRED, not closed (D113): generations already streaming over the
	// old connection finish on the old machine — dstack keeps a replaced
	// replica's routes alive through its own drain — and the connection
	// closes itself when the last of them ends. A synchronous Close here
	// killed every in-flight generation mid-token on the strength of a
	// routing update.
	if t, ok := b.conns[model]; ok {
		if t.endpoint == want {
			b.mu.Unlock()
			return t
		}
		t.retire()
		delete(b.conns, model)
		// A replaced replica is a different machine, so a failure recorded
		// against the old one says nothing about this one.
		delete(b.failed, model)
	}
	b.mu.Unlock()

	// Dialled WITHOUT the lock: a handshake to a marketplace host takes
	// hundreds of milliseconds and may take the full DialTimeout, and holding
	// mu across it would stall every other model's requests behind one slow
	// replica.
	t, err := b.dial(want, signer)
	if err != nil {
		// Fail SOFT: the caller falls back to dstack's proxy, which works.
		b.mu.Lock()
		if b.failed == nil {
			b.failed = map[string]failedDial{}
		}
		b.failed[model] = failedDial{endpoint: want, until: time.Now().Add(backoff)}
		b.mu.Unlock()
		slog.Warn("ssh tunnel to replica unavailable, using dstack proxy",
			"model", model, "host", want.Host, "retry_after", backoff.String(), "err", err)
		return nil
	}

	b.mu.Lock()
	// Two requests for the same cold model race here by design; the loser
	// closes its connection rather than leaking it.
	if existing, ok := b.conns[model]; ok && existing.endpoint == want {
		b.mu.Unlock()
		_ = t.client.Close()
		return existing
	}
	b.conns[model] = t
	delete(b.failed, model)
	b.mu.Unlock()

	// D106: a tunnel that dies AFTER being established must evict itself.
	// Without this, endpoint-equality reuse handed out a dead transport
	// forever: verified against a real in-process SSH server, three
	// consecutive forwards failed and Inner was never reached, while every
	// failure was charged to the replica's health — unhealthyDue then tears
	// down a perfectly healthy GPU, which is recreated, and the loop
	// repeats.
	go b.watch(model, t)

	slog.Info("ssh tunnel to replica established",
		"model", model, "host", want.Host, "ssh_port", want.SSHPort, "service_port", want.ServicePort)
	return t
}

// keepaliveInterval paces watch's liveness probe. 30s notices a NAT idle
// drop or silent host death well inside DialBackoff, without meaningfully
// loading the connection.
const keepaliveInterval = 30 * time.Second

// watch owns one tunnel's lifecycle after establishment (D106): it evicts
// the tunnel from the map the moment the SSH connection dies, so the next
// request redials or falls back to Inner instead of being handed a dead
// transport. The keepalive is what turns a SILENT death — a NAT table
// flushing an idle mapping, a host powering off — into a detected one;
// a death the kernel reports (RST, FIN) unblocks Wait on its own.
//
// Deliberately NOT recorded in b.failed: a connection dying is not a
// structural dial failure, and the next request should try a fresh dial
// immediately rather than ride the fallback for a whole backoff.
func (b *SSHBackend) watch(model string, t *tunnel) {
	dead := make(chan struct{})
	go func() {
		defer close(dead)
		_ = t.client.Wait()
	}()

	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-dead:
			b.mu.Lock()
			if cur, ok := b.conns[model]; ok && cur == t {
				delete(b.conns, model)
			}
			b.mu.Unlock()
			if !t.retired.Load() {
				slog.Warn("ssh tunnel to replica died; will redial on the next request",
					"model", model, "host", t.endpoint.Host)
			}
			return
		case <-ticker.C:
			// keepalive@openssh.com with want_reply=true forces a round
			// trip; an error means the connection is gone even if the
			// kernel has not noticed. Close unblocks Wait, which runs the
			// eviction above.
			if _, _, err := t.client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				_ = t.client.Close()
			}
		}
	}
}

// resolveSigner returns the cached key, loading it on first success. A failed
// load is NOT cached: the Secret may simply not exist yet.
func (b *SSHBackend) resolveSigner() ssh.Signer {
	b.mu.Lock()
	cached := b.signer
	b.mu.Unlock()
	if cached != nil {
		return cached
	}
	if b.LoadSigner == nil {
		return nil
	}
	s, err := b.LoadSigner()
	if err != nil || s == nil {
		return nil
	}
	b.mu.Lock()
	if b.signer == nil {
		b.signer = s
	}
	cached = b.signer
	b.mu.Unlock()
	return cached
}

func (b *SSHBackend) dial(e ReplicaEndpoint, signer ssh.Signer) (*tunnel, error) {
	timeout := b.DialTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	addr := net.JoinHostPort(e.Host, fmt.Sprint(e.SSHPort))

	cfg := &ssh.ClientConfig{
		User:            e.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: b.pinHostKey(addr),
		Timeout:         timeout,
	}
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", addr, err)
	}

	target := net.JoinHostPort("localhost", fmt.Sprint(e.ServicePort))
	t := &tunnel{endpoint: e, client: client}
	t.http = &http.Client{
		// No client-side timeout: a generation legitimately runs for
		// minutes and the handler already owns request deadlines. A
		// timeout here would truncate a streaming response mid-token.
		//
		// refTransport is D113's bookkeeping: each response holds a
		// reference until its body closes, so a retired tunnel can wait
		// for its last stream instead of cutting it.
		Transport: &refTransport{t: t, rt: &http.Transport{
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				return client.Dial("tcp", target)
			},
			MaxIdleConnsPerHost: 256,
			// Compression off: model output is streamed JSON over an
			// already-local hop, and the CPU is better spent elsewhere.
			DisableCompression: true,
		}},
	}
	return t, nil
}

// pinHostKey implements trust-on-first-use for one address.
func (b *SSHBackend) pinHostKey(addr string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		fingerprint := ssh.FingerprintSHA256(key)
		b.hostMu.Lock()
		defer b.hostMu.Unlock()
		if b.hostKeys == nil {
			b.hostKeys = map[string]string{}
		}
		seen, ok := b.hostKeys[addr]
		if !ok {
			b.hostKeys[addr] = fingerprint
			return nil
		}
		if seen != fingerprint {
			return fmt.Errorf("host key for %s changed (pinned %s, offered %s): refusing to forward", addr, seen, fingerprint)
		}
		return nil
	}
}

// Close retires every tunnel. Used on shutdown — retire, not close (D113):
// srv.Shutdown waits for in-flight handlers, and those handlers are still
// streaming over these connections; each closes when its last stream ends.
func (b *SSHBackend) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for model, t := range b.conns {
		t.retire()
		delete(b.conns, model)
	}
}

// ParseSigner reads squall's private key. Whitespace-only input is treated as
// absent rather than as an error, because an unmounted Secret reads that way
// and must degrade to "no direct path", not to a crash.
func ParseSigner(pem []byte) (ssh.Signer, error) {
	if len(bytes.TrimSpace(pem)) == 0 {
		return nil, nil
	}
	return ssh.ParsePrivateKey(pem)
}
