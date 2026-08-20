/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package runtime

import (
	"net/http"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandboxendpoint"
	"github.com/openkruise/agents/pkg/utils"
)

// EndpointAttrs projects a sandbox's addressing attributes for the resolver. It
// is the single place that reads the CR, so the resolver itself stays free of
// Kubernetes types and every consumer projects identically.
func EndpointAttrs(sbx *agentsv1alpha1.Sandbox) sandboxendpoint.Attrs {
	if sbx == nil {
		return sandboxendpoint.Attrs{}
	}
	attrs := sandboxendpoint.Attrs{PodIP: sbx.Status.PodInfo.PodIP}
	ep := sbx.Status.Endpoint
	if ep == nil {
		return attrs
	}
	attrs.Mode = string(ep.Mode)
	attrs.Address = ep.Address
	attrs.Scheme = ep.Scheme
	attrs.Authority = ep.Authority
	attrs.PathPrefix = ep.PathPrefix
	attrs.Headers = ep.Headers
	return attrs
}

// Addressable reports whether sbx can be reached at all.
func Addressable(sbx *agentsv1alpha1.Sandbox) bool {
	return utils.IsSandboxAddressable(sbx)
}

// Endpoint resolves how to reach sbx on port: the base URL to build requests
// against, and an HTTP client that applies whatever decoration the sandbox's
// endpoint calls for. base supplies the transport and timeout; a nil base gets
// http.DefaultTransport.
//
// It is the seam every consumer outside this package uses — the CDP proxy,
// ext-proc and the gateway filter all need the same answer, and sharing this
// function is what stops them from drifting.
func Endpoint(sbx *agentsv1alpha1.Sandbox, port int, base *http.Client) (string, *http.Client, error) {
	attrs := EndpointAttrs(sbx)
	target, err := sandboxendpoint.Resolve(attrs, port)
	if err != nil {
		return "", nil, err
	}
	if !target.ViaFront {
		client := base
		if client == nil {
			client = http.DefaultClient
		}
		return target.BaseURL(), client, nil
	}
	return target.BaseURL(), decorate(base, target), nil
}

// viaFront resolves the front target for sbx when it declares hostname
// addressing, and reports ok=false when the sandbox is addressed directly so the
// caller keeps its existing behavior untouched.
func (r *runtimeClient) viaFront(sbx *agentsv1alpha1.Sandbox) (sandboxendpoint.Target, bool, error) {
	attrs := EndpointAttrs(sbx)
	if attrs.Mode != sandboxendpoint.ModeHostname {
		return sandboxendpoint.Target{}, false, nil
	}
	target, err := sandboxendpoint.Resolve(attrs, r.runtimePort())
	if err != nil {
		return sandboxendpoint.Target{}, true, err
	}
	return target, true, nil
}

// runtimePort is the port the caller wants on the sandbox. Under hostname
// addressing it is rendered into the request rather than dialed, because the
// front's listener port is fixed.
func (r *runtimeClient) runtimePort() int {
	if r.tlsEnabled && r.tlsPort > 0 {
		return r.tlsPort
	}
	return utils.RuntimePort
}

// decorate returns a client that applies the target's request decoration. The
// authority travels as the Host header rather than in the URL, so the front can
// route on it while the connection still goes to the front's own address.
func decorate(base *http.Client, target sandboxendpoint.Target) *http.Client {
	rt := http.DefaultTransport
	if base != nil && base.Transport != nil {
		rt = base.Transport
	}
	decorated := &http.Client{Transport: &decoratingTransport{rt: rt, target: target}}
	if base != nil {
		decorated.Timeout = base.Timeout
	}
	return decorated
}

// decoratingTransport sets the authority and headers the front routes on. It
// clones the request because a RoundTripper must not mutate the caller's.
type decoratingTransport struct {
	rt     http.RoundTripper
	target sandboxendpoint.Target
}

func (d *decoratingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	if d.target.Authority != "" {
		out.Host = d.target.Authority
	}
	for k, v := range d.target.Headers {
		out.Header.Set(k, v)
	}
	return d.rt.RoundTrip(out)
}
