// SPDX-License-Identifier: MIT

// Command fake-dstack ships internal/dstack/mock (Phase 4) as a standalone
// HTTP server, so Phase 11's kind e2e cluster can exercise the real
// squall-controller and squall-proxy binaries against something that
// behaves like dstack (F17/F18/F20/F21/F23) without provisioning a GPU.
//
// This is test infrastructure, not a production binary: it is built and
// kind-loaded only by test/e2e/cluster (see hack/cluster.sh), never shipped
// in a release image.
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/ackstorm/squall/internal/dstack/mock"
)

func main() {
	addr := os.Getenv("FAKE_DSTACK_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	server := mock.New()

	// F21's fleet idle release only ever evaluates on Tick (see mock's
	// package doc: "nothing expires on its own"). Production tests drive
	// Tick from a FakeClock explicitly; this binary uses the real wall
	// clock (mock.New()'s default), so something has to call Tick
	// periodically for the mock's fleetIdleDuration to ever actually
	// elapse against a live kind cluster. See ledger D169: squall itself
	// now always sends FleetIdleDuration: 0, and mock.Server.Apply only
	// overwrites fleetIdleDuration when the request's value is > 0 — so in
	// real e2e traffic this Tick loop never actually releases anything,
	// the opposite of real dstack's zero-tolerance semantics for 0.
	tickInterval := 1 * time.Second
	go func() {
		ticker := time.NewTicker(tickInterval)
		defer ticker.Stop()
		for range ticker.C {
			server.Tick()
		}
	}()

	log.Printf("fake-dstack listening on %s (tick every %s)", addr, tickInterval)
	//nolint:gosec // e2e test double, not internet-facing.
	if err := http.ListenAndServe(addr, server.Handler()); err != nil {
		log.Fatalf("fake-dstack: listen failed: %v", err)
	}
}
