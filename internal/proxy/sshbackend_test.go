// SPDX-License-Identifier: MIT

package proxy

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// A REAL SSH server, in-process. Stubbing the tunnel would test nothing that
// matters: the first version of SSHBackend deadlocked on its first successful
// dial, because the host-key callback ran inside ssh.Dial while the caller
// still held the same mutex. Only an actual handshake reaches that code.
type fakeReplica struct {
	addr        string
	servicePort int
	hostKey     ssh.Signer
	forwards    atomic.Int32
}

// startFakeReplica runs an SSH server that answers direct-tcpip channels by
// dialling backend — the same shape dstack's replicas expose.
func startFakeReplica(t *testing.T, backend string, hostKey ssh.Signer, authorized ssh.PublicKey) *fakeReplica {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	_, portStr, _ := net.SplitHostPort(backend)
	port, _ := strconv.Atoi(portStr)
	r := &fakeReplica{addr: ln.Addr().String(), servicePort: port, hostKey: hostKey}

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if authorized != nil && string(key.Marshal()) == string(authorized.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("unauthorized key")
		},
	}
	cfg.AddHostKey(hostKey)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go r.serve(conn, cfg, backend)
		}
	}()
	return r
}

func (r *fakeReplica) serve(conn net.Conn, cfg *ssh.ServerConfig, backend string) {
	sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer func() { _ = sc.Close() }()
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "direct-tcpip" {
			_ = newCh.Reject(ssh.UnknownChannelType, "only direct-tcpip")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		go ssh.DiscardRequests(chReqs)
		r.forwards.Add(1)
		go func() {
			defer func() { _ = ch.Close() }()
			up, err := net.Dial("tcp", backend)
			if err != nil {
				return
			}
			defer func() { _ = up.Close() }()
			go func() { _, _ = io.Copy(up, ch) }()
			_, _ = io.Copy(ch, up)
		}()
	}
}

func testSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	signer, err := ssh.ParsePrivateKey(pem.EncodeToMemory(block))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return signer
}

// backendThatMustNotBeUsed fails the test if the fallback path is taken.
type backendThatMustNotBeUsed struct{ t *testing.T }

func (b backendThatMustNotBeUsed) URL(string) (*url.URL, bool) {
	b.t.Fatalf("fell back to dstack's proxy when a direct tunnel was available")
	return nil, false
}

func newSSHBackendFor(t *testing.T, replica *fakeReplica, signer ssh.Signer, cache *ModelCache, inner Backend) *SSHBackend {
	t.Helper()
	host, portStr, _ := net.SplitHostPort(replica.addr)
	port, _ := strconv.Atoi(portStr)
	cache.Set("qwen", ModelSnapshot{
		Phase: "Ready",
		Replica: &ReplicaEndpoint{
			Host: host, SSHPort: port, User: "squall", ServicePort: replica.servicePort,
		},
	})
	b := &SSHBackend{
		Inner:       inner,
		Cache:       cache,
		LoadSigner:  func() (ssh.Signer, error) { return signer, nil },
		DialTimeout: 5 * time.Second,
	}
	t.Cleanup(b.Close)
	return b
}

// TestSSHBackend_ForwardsThroughTunnel is the whole design in one test: an
// HTTP request reaching the engine over SSH, with dstack nowhere in the path.
func TestSSHBackend_ForwardsThroughTunnel(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"served":"over-ssh"}`))
	}))
	t.Cleanup(engine.Close)
	engineAddr := strings.TrimPrefix(engine.URL, "http://")

	client := testSigner(t)
	replica := startFakeReplica(t, engineAddr, testSigner(t), client.PublicKey())
	b := newSSHBackendFor(t, replica, client, NewCache(), backendThatMustNotBeUsed{t})

	httpClient, ok := b.Client("qwen")
	if !ok {
		t.Fatal("Client() reported no tunnel; the endpoint and key were both valid")
	}
	resp, err := httpClient.Get("http://replica/v1/models")
	if err != nil {
		t.Fatalf("request over the tunnel: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "over-ssh") {
		t.Fatalf("body = %q, want the engine's response", body)
	}
	if n := replica.forwards.Load(); n == 0 {
		t.Fatal("the replica saw no forwarded channel: the request did not go through the tunnel")
	}
}

// TestSSHBackend_ReusesOneConnection pins the measured decision not to pool:
// at 128 concurrent, one SSH connection matched eight to within 1%.
func TestSSHBackend_ReusesOneConnection(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(engine.Close)

	client := testSigner(t)
	replica := startFakeReplica(t, strings.TrimPrefix(engine.URL, "http://"), testSigner(t), client.PublicKey())
	b := newSSHBackendFor(t, replica, client, NewCache(), backendThatMustNotBeUsed{t})

	first, _ := b.Client("qwen")
	for i := 0; i < 5; i++ {
		resp, err := first.Get("http://replica/health")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	second, ok := b.Client("qwen")
	if !ok || second != first {
		t.Fatal("a second call re-dialled instead of reusing the live tunnel")
	}
}

// TestSSHBackend_UnreachableReplicaFallsBackToInner is the safety property:
// the direct path is an optimisation, and an optimisation that can fail a
// user's request is a bug.
func TestSSHBackend_UnreachableReplicaFallsBackToInner(t *testing.T) {
	// A port nothing listens on: bind, capture, close.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	dead := ln.Addr().String()
	_ = ln.Close()
	host, portStr, _ := net.SplitHostPort(dead)
	port, _ := strconv.Atoi(portStr)

	cache := NewCache()
	cache.Set("qwen", ModelSnapshot{
		Phase:   "Ready",
		Replica: &ReplicaEndpoint{Host: host, SSHPort: port, User: "squall", ServicePort: 8000},
	})
	fallback, _ := url.Parse("http://dstack.example/proxy/services/main/qwen")
	b := &SSHBackend{
		Inner:       staticBackend{u: fallback},
		Cache:       cache,
		LoadSigner:  func() (ssh.Signer, error) { return testSigner(t), nil },
		DialTimeout: time.Second,
	}
	t.Cleanup(b.Close)

	if _, ok := b.Client("qwen"); ok {
		t.Fatal("Client() offered a tunnel to an unreachable replica")
	}
	got, ok := b.URL("qwen")
	if !ok || got.String() != fallback.String() {
		t.Fatalf("URL() = %v (ok=%v), want the fallback %v", got, ok, fallback)
	}
}

// TestSSHBackend_RejectsChangedHostKey is the pinning property. dstack
// publishes no host key, so the FIRST connection cannot be verified — but a
// later one offering a different key is an endpoint swap, and forwarding user
// traffic into it is exactly the MITM §12.3 warns about.
func TestSSHBackend_RejectsChangedHostKey(t *testing.T) {
	b := &SSHBackend{}
	cb := b.pinHostKey("replica:40097")

	first := testSigner(t).PublicKey()
	if err := cb("replica:40097", nil, first); err != nil {
		t.Fatalf("first connection must be trusted (nothing to compare against): %v", err)
	}
	if err := cb("replica:40097", nil, first); err != nil {
		t.Fatalf("same key must keep being accepted: %v", err)
	}
	if err := cb("replica:40097", nil, testSigner(t).PublicKey()); err == nil {
		t.Fatal("a CHANGED host key was accepted: pinning is not in effect")
	}
}

// TestSSHBackend_NoReplicaOrNoKeyFallsBack covers the two ordinary reasons the
// direct path is unavailable, both of which must be silent and safe.
func TestSSHBackend_NoReplicaOrNoKeyFallsBack(t *testing.T) {
	fallback, _ := url.Parse("http://dstack.example/proxy/services/main/qwen")

	t.Run("no replica on status", func(t *testing.T) {
		cache := NewCache()
		cache.Set("qwen", ModelSnapshot{Phase: "Ready"})
		b := &SSHBackend{Inner: staticBackend{u: fallback}, Cache: cache,
			LoadSigner: func() (ssh.Signer, error) { return testSigner(t), nil }}
		if _, ok := b.Client("qwen"); ok {
			t.Fatal("offered a tunnel with no endpoint published")
		}
	})

	t.Run("no key yet", func(t *testing.T) {
		cache := NewCache()
		cache.Set("qwen", ModelSnapshot{Phase: "Ready",
			Replica: &ReplicaEndpoint{Host: "h", SSHPort: 1, User: "u", ServicePort: 8000}})
		var loads atomic.Int32
		b := &SSHBackend{Inner: staticBackend{u: fallback}, Cache: cache,
			LoadSigner: func() (ssh.Signer, error) { loads.Add(1); return nil, fmt.Errorf("secret not created yet") }}
		if _, ok := b.Client("qwen"); ok {
			t.Fatal("offered a tunnel with no key")
		}
		if _, ok := b.Client("qwen"); ok {
			t.Fatal("offered a tunnel with no key on the second call either")
		}
		// A failed load must NOT be cached: the controller mints the Secret
		// after the proxy starts, and giving up permanently would strand the
		// proxy on the slow path until someone restarted it.
		if n := loads.Load(); n < 2 {
			t.Fatalf("LoadSigner called %d times, want a retry on every attempt until it succeeds", n)
		}
	})
}

// TestSSHBackend_FailedDialIsNotRetriedEveryRequest is D90. Measured live:
// 64 rejected handshakes in a few minutes against one replica that had been
// provisioned before squall had a key, so its authorized_keys could never
// accept us. Every request paid a fresh TCP connect plus a rejected crypto
// handshake before falling back.
//
// The existing fallback tests all passed throughout, which is exactly why
// this hid: the OUTCOME was correct and only the cost was wrong. Asserting
// the fallback result is not enough — the retry has to be counted.
func TestSSHBackend_FailedDialIsNotRetriedEveryRequest(t *testing.T) {
	// A listener that accepts TCP and immediately closes: every SSH handshake
	// fails, exactly as a replica refusing our key does.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var dials atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			dials.Add(1)
			_ = c.Close()
		}
	}()

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	cache := NewCache()
	cache.Set("qwen", ModelSnapshot{
		Phase:   "Ready",
		Replica: &ReplicaEndpoint{Host: host, SSHPort: port, User: "squall", ServicePort: 8000},
	})
	fallback, _ := url.Parse("http://dstack.example/proxy/services/main/qwen")
	b := &SSHBackend{
		Inner:       staticBackend{u: fallback},
		Cache:       cache,
		LoadSigner:  func() (ssh.Signer, error) { return testSigner(t), nil },
		DialTimeout: 2 * time.Second,
		DialBackoff: time.Hour, // long enough that no retry is legitimate here
	}
	t.Cleanup(b.Close)

	const requests = 20
	for i := 0; i < requests; i++ {
		if _, ok := b.Client("qwen"); ok {
			t.Fatalf("request %d got a tunnel from a replica that rejects every handshake", i)
		}
	}

	if n := dials.Load(); n != 1 {
		t.Fatalf("%d dial attempts across %d requests, want exactly 1: a structurally impossible dial must be remembered, not retried on the hot path (D90)", n, requests)
	}
}

// TestSSHBackend_NewEndpointClearsTheFailure: a replaced replica is a
// different machine, so a failure recorded against the old one says nothing
// about it. Without this the backoff would keep a MODEL on the fallback after
// it had already been rescheduled onto a host that would accept us.
func TestSSHBackend_NewEndpointClearsTheFailure(t *testing.T) {
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			c, err := dead.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = dead.Close() })
	deadHost, deadPortStr, _ := net.SplitHostPort(dead.Addr().String())
	deadPort, _ := strconv.Atoi(deadPortStr)

	cache := NewCache()
	cache.Set("qwen", ModelSnapshot{
		Phase:   "Ready",
		Replica: &ReplicaEndpoint{Host: deadHost, SSHPort: deadPort, User: "squall", ServicePort: 8000},
	})
	fallback, _ := url.Parse("http://dstack.example/proxy/services/main/qwen")
	client := testSigner(t)
	b := &SSHBackend{
		Inner:       staticBackend{u: fallback},
		Cache:       cache,
		LoadSigner:  func() (ssh.Signer, error) { return client, nil },
		DialTimeout: 2 * time.Second,
		DialBackoff: time.Hour,
	}
	t.Cleanup(b.Close)

	if _, ok := b.Client("qwen"); ok {
		t.Fatal("got a tunnel from the dead endpoint")
	}

	// The controller reschedules the model onto a replica that DOES accept us.
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(engine.Close)
	good := startFakeReplica(t, strings.TrimPrefix(engine.URL, "http://"), testSigner(t), client.PublicKey())
	goodHost, goodPortStr, _ := net.SplitHostPort(good.addr)
	goodPort, _ := strconv.Atoi(goodPortStr)
	cache.Set("qwen", ModelSnapshot{
		Phase:   "Ready",
		Replica: &ReplicaEndpoint{Host: goodHost, SSHPort: goodPort, User: "squall", ServicePort: good.servicePort},
	})

	if _, ok := b.Client("qwen"); !ok {
		t.Fatal("still on the fallback after the replica was replaced: the backoff outlived the machine it was about")
	}
}
