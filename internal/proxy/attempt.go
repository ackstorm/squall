// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	squallv1alpha1 "github.com/ackstorm/squall/api/squall/v1alpha1"
)

// maxReplayableBody bounds how much request body is buffered so a held
// request can be retried (§7). Chat-completion bodies are prompts; 4 MiB is
// generous for that shape and is not a CRD knob.
//
// D120: since routing itself must parse the body for "model", a body over
// this ceiling is answered 413 at the door (modelFromRequest) — it is not
// forwarded unparsed. readReplayableBody's own over-cap branch is therefore
// defence in depth for callers that bypass modelFromRequest, not a path
// ServeHTTP reaches.
const maxReplayableBody = 4 << 20

// errNoBackendURL marks a forward that failed because the PROXY has no
// route (empty status.serviceURL / SQUALL_DSTACK_URL) — a fact about our
// own configuration, which must never be charged to the replica's health
// (D118; same class as D99's client-disconnect miscount).
var errNoBackendURL = errors.New("no backend url for model")

// attemptResult classifies one outbound try.
type attemptResult int

const (
	// attemptRetry means the gateway says the engine is not serving YET —
	// §7: "503, 502, or connection-refused mean 'still waking', keep
	// holding". Nothing has been written to the client.
	attemptRetry attemptResult = iota
	// attemptCommit means the engine ANSWERED. Stream it, stop retrying.
	// This includes 4xx and 5xx that are not 502/503: an engine returning
	// 400 is ready, it just disliked the request.
	attemptCommit
)

// classifyAttempt implements §7's retry rule. A transport error (connection
// refused/reset, dial timeout) is indistinguishable from "the engine has
// not bound its port yet", which is exactly the waking state.
//
// 404 is phase-dependent, and that is not a nicety (D44). MEASURED against
// dstack 0.21.2: the service proxy answers 404 for the whole wake —
// pending, submitted, provisioning, and even running until the probe's
// success streak reaches ready_after — and 404 is ALSO what a dead or
// deregistered service answers (F23). The status line cannot tell them
// apart, so the CR's phase does: while the controller still says the model
// is coming up, keep holding; once it says otherwise, the 404 is the truth
// and the client deserves it.
//
// LIVE-6: phase alone is not sufficient. Squall's Ready is derived from
// dstack's PROBE (§6 evidence (a)), and MEASURED on a live run, dstack's own
// service proxy still answers 404 for a window AFTER the probe goes green —
// it has not registered the route yet. So there is a real window where the
// phase is Ready and the 404 is still the gateway, not the engine. body404
// is the (possibly nil) response body, peeked ONLY for 404s by the caller;
// dstack's own "service not found" shape (see isDstackServiceNotFound)
// overrides the phase and always retries — mirroring internal/dstack's own
// doctrine of classifying errors from the body, never the status code.
func classifyAttempt(resp *http.Response, err error, phase squallv1alpha1.ModelPhase, body404 []byte) attemptResult {
	if err != nil {
		return attemptRetry
	}
	switch resp.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		return attemptRetry
	case http.StatusNotFound:
		if isDstackServiceNotFound(body404) {
			return attemptRetry
		}
		// The retry set is exactly the set of phases decision.go BLOCKS on.
		// That is not a coincidence and must stay in lockstep: a phase that
		// holds is one where the controller is bringing capacity up, and
		// dstack answers 404 for that whole window (§9.5) — so committing
		// the 404 would end a hold that decision.go opened on purpose,
		// making its DeadlineState unreachable.
		//
		// Dead is in the set for that reason. decision.go answers Dead with
		// Block + WaitRecreating, and Dead is by definition the phase where
		// the backend 404s, so leaving it out made the very first attempt
		// commit a bare 404 and the WaitRecreating deadline dead code.
		// Recreating is in for the same reason (F20's dead->fresh-run path).
		// Draining is NOT: decision.go answers it 404 immediately and never
		// holds, so it never reaches here.
		switch phase {
		case squallv1alpha1.ModelPhaseAsleep, squallv1alpha1.ModelPhaseWaking,
			squallv1alpha1.ModelPhaseRecreating, squallv1alpha1.ModelPhaseDead:
			return attemptRetry
		default:
			return attemptCommit
		}
	default:
		return attemptCommit
	}
}

// dstackNotFoundBody mirrors dstack's service-proxy 404 shape, MEASURED
// (docs/references/dstack-real-api.md §8.1, LIVE-6):
//
//	{"detail":"Service main/qwen3-8-27b not found"}
//
// "main" is the dstack project and "qwen3-8-27b" the run name — both vary
// per deployment, so the matcher below only pins the stable "Service ...
// not found" shape around them.
type dstackNotFoundBody struct {
	Detail string `json:"detail"`
}

// isDstackServiceNotFound reports whether body is dstack's OWN "service
// proxy does not know this route" 404 — a gateway-registration-lag signal,
// not an answer from the engine. Deliberately narrow, matching
// internal/dstack's body-not-status-code doctrine:
//
//   - dstack's `detail` here is a bare STRING. The run-management API's 404
//     analogue (400 for "run not found") is a LIST of {msg, code} objects
//     (§8.1) and the auth-fault 403 is an OBJECT — both fail json.Unmarshal
//     against the string-typed field below, so neither can collide with
//     this matcher.
//   - An engine's own 404 (e.g. FastAPI/vLLM's default `{"detail":"Not
//     Found"}`) has no "Service " prefix and no " not found" suffix, so it
//     cannot match either. This is what keeps a real, engine-produced 404
//     committing exactly as before.
func isDstackServiceNotFound(body []byte) bool {
	var b dstackNotFoundBody
	if err := json.Unmarshal(body, &b); err != nil {
		return false
	}
	return strings.HasPrefix(b.Detail, "Service ") && strings.HasSuffix(b.Detail, " not found")
}

// notFoundPeekLimit bounds how many bytes attemptForward reads to classify
// a 404 body (LIVE-6). dstack's own body is one short JSON string (measured
// well under 100 bytes); this is generous headroom, not a tuned value. The
// peek never drops bytes — see peekAndRestore — so it cannot truncate a
// larger, genuinely-committed 404 body.
const notFoundPeekLimit = 4096

// peekAndRestore reads up to limit bytes of resp.Body to classify it, then
// splices those bytes back onto resp.Body via io.MultiReader so the FULL
// body — whatever its length — is still readable exactly once from the
// start. This is what lets classifyAttempt inspect a 404 body without
// costing streamCommit a single byte of a response that turns out to be
// committed (LIVE-6's "must not silently truncate real responses"
// requirement).
func peekAndRestore(resp *http.Response, limit int) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	peeked, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)))
	if err != nil {
		return nil
	}
	// D122: keep the transport's REAL Closer. NopCloser here made every
	// later resp.Body.Close() a no-op, leaking the connection and its
	// readLoop goroutine whenever a peeked body was not drained to EOF —
	// which is exactly the retry path, where the body is abandoned.
	resp.Body = &spliceBody{
		Reader: io.MultiReader(bytes.NewReader(peeked), resp.Body),
		closer: resp.Body,
	}
	return peeked
}

// spliceBody re-prefixes peeked bytes onto a body while delegating Close to
// the ORIGINAL body, so the transport's connection bookkeeping still runs.
type spliceBody struct {
	io.Reader
	closer io.Closer
}

func (s *spliceBody) Close() error { return s.closer.Close() }

// attemptForward performs ONE outbound try and writes NOTHING to the
// client — that is what makes the retry loop safe under F32 ("nothing on
// the wire before the outcome"). On attemptRetry the response body is
// drained and closed here; on attemptCommit the caller owns the body and
// MUST close it.
//
// body is the buffered request payload for replay; when nil the request's
// own body is streamed (the over-cap, single-shot path). phase is the CR
// phase to classify a 404 against (D44) — callers on a long hold must
// re-read it fresh from the cache each attempt, never capture it once,
// since the hold exists precisely because the phase is expected to change
// underneath.
// The third return value is why a non-committing attempt did not commit. It
// exists because a 503 from the engine, a dead tunnel and a caller that hung
// up all used to produce the SAME log line, which cost hours of forensics
// after the 2026-08-31 conc-64 run (D99).
func (h *Handler) attemptForward(ctx context.Context, r *http.Request, model string, body []byte, phase squallv1alpha1.ModelPhase) (*http.Response, attemptResult, error) {
	// D121: URL and transport resolved in ONE call. They used to be two
	// independent lookups (Backend.URL then forwardClient->Client), and a
	// cache update landing between them paired dstack's proxy path with
	// the SSH transport — the engine's 404 for a route it never had,
	// committed to the caller as the model's own answer — or the SSH
	// placeholder host with http.DefaultClient, which does not resolve.
	target, fwdClient, ok := h.route(model)
	if !ok {
		return nil, attemptRetry, errNoBackendURL
	}

	// The caller addresses a Model by its Kubernetes name; the engine knows
	// whatever name it was started with. squall makes those equal for every
	// engine it starts, so this is normally a no-op — but when they differ
	// the engine 404s for its own model, and rewriting one field is cheaper
	// than teaching the proxy three engines' dialects.
	// D100: ForwardModel, never ServedModel. ServedModel is the diagnostic
	// report and can be a comma-joined list; rewriting to it sent Ollama
	// `"model":"ollama-tiny:latest,qwen2.5:0.5b"` and earned a 400 on every
	// request the moment verification succeeded. Empty means the controller
	// found no safe single name, and the caller's own value stands.
	if snap, ok := h.Cache.Get(model); ok && snap.ForwardModel != "" && snap.ForwardModel != model {
		if rewritten, err := rewriteModelField(body, snap.ForwardModel); err == nil {
			body = rewritten
		}
	}

	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	} else if r.Body != nil {
		payload = r.Body
	}

	// D127: the query string travels too — target.String()+r.URL.Path
	// silently dropped every parameter the caller sent.
	targetURL := target.String() + r.URL.Path
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}
	out, err := http.NewRequestWithContext(ctx, r.Method, targetURL, payload)
	if err != nil {
		return nil, attemptRetry, fmt.Errorf("build request: %w", err)
	}
	out.Header = r.Header.Clone()
	out.Header.Del("Host")
	// D127: hop-by-hop headers describe ONE connection and must not cross
	// the proxy (RFC 7230 §6.1) — forwarding Connection/TE/Upgrade verbatim
	// breaks upstream connection reuse and can confuse an intermediary.
	stripHopByHop(out.Header)
	if h.DstackToken != "" {
		// Overwrite, don't Add: whatever the client sent here (if anything)
		// authenticates the client to squall-proxy/LiteLLM, not squall-proxy
		// to dstack. dstack must see squall's own token, never the caller's.
		out.Header.Set("Authorization", "Bearer "+h.DstackToken)
	}
	if body != nil {
		out.ContentLength = int64(len(body))
	}

	resp, err := fwdClient.Do(out)
	var body404 []byte
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		body404 = peekAndRestore(resp, notFoundPeekLimit)
	}
	result := classifyAttempt(resp, err, phase, body404)
	if result == attemptRetry && resp != nil {
		status := resp.StatusCode
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return nil, attemptRetry, fmt.Errorf("upstream status %d", status)
	}
	if err != nil {
		return nil, attemptRetry, fmt.Errorf("upstream: %w", err)
	}
	return resp, attemptCommit, nil
}

// rewriteModelField replaces the OpenAI body's "model" with served, leaving
// every other field untouched. Returns an error rather than a guess when the
// body is not the JSON object we already parsed it as upstream.
func rewriteModelField(body []byte, served string) ([]byte, error) {
	if body == nil {
		return nil, errors.New("no buffered body")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(served)
	if err != nil {
		return nil, err
	}
	obj["model"] = raw
	return json.Marshal(obj)
}

// streamCommit copies a committed upstream response to the client,
// flushing as it goes so a streamed completion (SSE) reaches the caller
// progressively instead of being buffered for the length of a generation.
func streamCommit(w http.ResponseWriter, resp *http.Response) error {
	// D127: the upstream's hop-by-hop headers describe ITS connection, not
	// the client's — copying them verbatim was the response-side half of
	// the same defect stripHopByHop fixes outbound.
	stripHopByHop(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := w.Write(buf[:n]); err != nil {
				return err
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

// readReplayableBody buffers r's body up to maxReplayableBody. ok is false
// when the body is larger, meaning the caller must forward once without
// retrying. r.Body is always left readable from the start.
func readReplayableBody(r *http.Request) (body []byte, ok bool) {
	if r.Body == nil {
		return nil, true
	}
	limited := io.LimitReader(r.Body, maxReplayableBody+1)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return nil, false
	}
	if len(buf) > maxReplayableBody {
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf), r.Body))
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(buf))
	return buf, true
}

// RouteBackend is an optional Backend capability: a backend that owns the
// TRANSPORT for a model, not merely its URL, and can resolve both in one
// atomic answer. SSHBackend implements it — its connections are SSH
// channels rather than TCP sockets, and its URL is a placeholder only that
// same transport understands, so the pair MUST come from a single
// resolution (D121). A backend that does not implement it is unaffected.
type RouteBackend interface {
	// Route returns the target URL and the client to reach it with. client
	// is nil for "use the shared default"; ok false means no route at all.
	Route(model string) (target *url.URL, client *http.Client, ok bool)
}

// route resolves one forward's URL AND transport together. Resolved per
// request because a replica can be replaced underneath a Model, and a stale
// tunnel would forward to a machine that is no longer serving it.
func (h *Handler) route(model string) (*url.URL, *http.Client, bool) {
	if rb, ok := h.Backend.(RouteBackend); ok {
		target, client, ok := rb.Route(model)
		if !ok {
			return nil, nil, false
		}
		if client == nil {
			client = http.DefaultClient
		}
		return target, client, true
	}
	target, ok := h.Backend.URL(model)
	if !ok {
		return nil, nil, false
	}
	return target, http.DefaultClient, true
}

// hopByHopHeaders are connection-scoped per RFC 7230 §6.1 and are stripped
// in BOTH directions (D127). Transfer-Encoding is included: Go's transport
// and server manage framing themselves.
var hopByHopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Proxy-Connection", "TE", "Trailer", "Transfer-Encoding", "Upgrade",
}

// stripHopByHop removes the fixed hop-by-hop set plus anything the
// Connection header itself names.
func stripHopByHop(h http.Header) {
	for _, field := range strings.Split(h.Get("Connection"), ",") {
		if field = strings.TrimSpace(field); field != "" {
			h.Del(field)
		}
	}
	for _, k := range hopByHopHeaders {
		h.Del(k)
	}
}
