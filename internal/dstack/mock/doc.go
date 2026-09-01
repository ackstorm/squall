// SPDX-License-Identifier: Apache-2.0

// Package mock is an in-memory fake of the dstack server API, reproducing
// the five source-verified behaviours Squall's design rests on. It exists
// so that every mechanic in spec §5.2/§6/§7 is testable in envtest without
// provisioning a GPU.
//
// Reproduced, each with a test that fails if it regresses:
//
//	F17  `replicas` is an IN-PLACE updatable service field. Apply updates
//	     the same run id and increments deployment_num — no new run. Fixed
//	     `replicas: 0` is accepted and yields a registered, routable
//	     service with zero jobs. Asleep-but-addressable is first-class.
//
//	F18  apply_plan enforces optimistic concurrency: an apply computed
//	     against changed state fails with "Resource has been changed. Try
//	     again or use force apply". Squall NEVER sends force — the losing
//	     side of any race must fail loudly (§5.2, AC13). The fake refuses
//	     force outright, so that a future caller adding it fails a test
//	     rather than a bill.
//
//	F20  The run id survives flips but NOT terminal states. A terminal run
//	     is DEREGISTERED from the gateway (404, not 503) and the next apply
//	     mints a NEW run id. Dead is not asleep — this is what tells the
//	     proxy to recreate-and-alarm rather than merely wake. MEASURED
//	     against a real server (§9.4): a terminal run still answers Get and
//	     ListRuns successfully, with Status "terminated" — it is NOT
//	     ErrNotFound. Only a name never applied, or one explicitly deleted,
//	     is not found.
//
//	F21  Runs land on fleets. Flipping replicas to 0 terminates the JOB;
//	     the INSTANCE is released only by fleet idle_duration. The
//	     surviving instance is the warm pool.
//
//	F23  Gateway responses are immediate, never held: registered + 0
//	     replicas + auth -> 503; unregistered/terminal -> 404; bad/missing
//	     token -> 403. The gateway never wakes a service.
//
// Deliberately NOT modelled: offer selection, pricing, real provisioning
// latency, SSH tunnels. Those are dstack's job and are proven on real
// hardware in PoC 0/2, not here.
//
// Two call surfaces, one state machine: every exported method (Apply,
// GatewayGet, Terminate, Tick, ...) can be called directly from a unit
// test, and Handler mounts the identical Server behind net/http for
// envtest-based controller tests that need a real client-over-HTTP path.
// The HTTP handlers do nothing but decode/encode around the same method
// calls — there is no second implementation to drift out of sync.
//
// F21's timing is driven through internal/clock.Clock rather than owned
// here: New wires up the real clock, SetClock swaps in a
// clock.FakeClock for tests, and Tick polls elapsed time off it. That seam
// lives in its own package so production controller code can reuse it
// (Phase 7's sleep flip, Phase 8's drain) without importing this test
// double.
package mock
