// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"fmt"
	"net/url"
	"strings"
)

// TemplateBackend was D25's stopgap Backend before the controller wrote
// status.serviceURL (see StatusBackend below, which is production's
// Backend now). Kept only because the e2e model-mock fixtures still
// hand-configure a template via SQUALL_BACKEND_URL_TEMPLATE rather than
// running a real Model status round trip. Template is a fmt.Sprintf format
// string with exactly one %s verb for the model name, e.g.
// "http://%s.svc.cluster.local:8000". An empty Template always resolves
// ok=false, so an unconfigured proxy answers 502 rather than guessing a
// target.
type TemplateBackend struct {
	Template string
}

func (b TemplateBackend) URL(model string) (*url.URL, bool) {
	if b.Template == "" {
		return nil, false
	}
	u, err := url.Parse(fmt.Sprintf(b.Template, model))
	if err != nil {
		return nil, false
	}
	return u, true
}

// StatusBackend resolves a Model's forward target from its own status,
// which is where the controller records what dstack told it (D25).
//
// The previous default was a printf template in an env var. That is why an
// installed chart could not reach a real replica without an operator first
// working out dstack's routing and hand-editing a value — and why a
// mis-set template produced a proxy that was healthy and inert (D54).
type StatusBackend struct {
	Cache *ModelCache
	// DstackBaseURL is the dstack server, e.g.
	// http://dstack.squall-system.svc.cluster.local:3000. status.serviceURL
	// is a path relative to it, not an absolute URL.
	DstackBaseURL string
}

func (b StatusBackend) URL(model string) (*url.URL, bool) {
	if b.Cache == nil || b.DstackBaseURL == "" {
		return nil, false
	}
	snap, ok := b.Cache.Get(model)
	if !ok || snap.ServiceURL == "" {
		// The controller has not resolved a target. Answering 502 is right;
		// guessing one would forward a caller's payload at an address nobody
		// chose.
		return nil, false
	}
	u, err := url.Parse(strings.TrimSuffix(b.DstackBaseURL, "/") + "/" + strings.Trim(snap.ServiceURL, "/"))
	if err != nil {
		return nil, false
	}
	return u, true
}
