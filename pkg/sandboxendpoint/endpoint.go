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

// Package sandboxendpoint turns "which sandbox, which port" into "where do I
// connect and what does the request look like".
//
// It is deliberately a leaf: no imports from this repository, so every caller
// can depend on it without dragging its own layer along. Callers project the
// sandbox's API fields into Attrs; the resolver never reads a Kubernetes object.
package sandboxendpoint

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"golang.org/x/net/http/httpguts"
)

const (
	// ModeDirect addresses the sandbox at its Pod IP. It is the zero value, so a
	// sandbox that declares nothing keeps today's behavior.
	ModeDirect = ""
	// ModeHostname addresses the sandbox through an L7 front.
	ModeHostname = "Hostname"

	// modeDirectExplicit is the spelling the API uses when Direct is written out
	// rather than left empty. Both resolve identically.
	modeDirectExplicit = "Direct"

	// portPlaceholder is the only substitution performed at call time: the caller
	// picks the port per call, so it cannot be baked in when the endpoint is
	// written. Everything else is already resolved by the writer.
	portPlaceholder = "{port}"

	// Internal Envoy headers carry the already validated front destination from
	// the Go control filter to static route/filter configuration. Both data
	// planes overwrite them before route-cache refresh, so downstream clients
	// cannot select arbitrary upstreams.
	InternalRouteHeader = "x-agents-sandbox-endpoint-route"
	InternalHostHeader  = "x-agents-sandbox-endpoint-host"
	InternalPortHeader  = "x-agents-sandbox-endpoint-port"
	RouteDirect         = "direct"
	RouteFrontHTTP      = "front-http"
	RouteFrontHTTPS     = "front-https"
	// DefaultScheme is used when an endpoint names no scheme.
	DefaultScheme = "https"
)

// Attrs is the sandbox's addressing attributes, projected from its status by the
// caller. The field names mirror the API so the projection stays obvious.
type Attrs struct {
	Mode       string
	PodIP      string
	Address    string
	Scheme     string
	Authority  string
	PathPrefix string
	Headers    map[string]string
}

// Target is where to connect and how to decorate the request. Every consumer —
// the runtime client, the CDP proxy, ext-proc, the gateway filter — applies the
// same four fields, so they cannot drift.
type Target struct {
	// DialHost is the host:port to connect to.
	DialHost string
	// Scheme is http or https.
	Scheme string
	// Authority is the value for Host / :authority, or "" to leave it to the URL.
	Authority string
	// PathPrefix is prepended to the request path, or "" for no rewrite.
	PathPrefix string
	// Headers are added to the request.
	Headers map[string]string
	// ViaFront reports whether this target is an L7 front rather than the sandbox
	// itself. Callers use it to decide whether the runtime TLS bundle applies.
	ViaFront bool
}

// BaseURL is the URL a client should be constructed against.
func (t Target) BaseURL() string {
	if t.PathPrefix == "" {
		return t.Scheme + "://" + t.DialHost
	}
	return t.Scheme + "://" + t.DialHost + t.PathPrefix
}

// RewritePath prepends a resolved endpoint prefix to an HTTP :path. The query
// remains attached to path and is therefore preserved verbatim.
func RewritePath(prefix, path string) string {
	if prefix == "" {
		return path
	}
	if path == "" {
		return prefix
	}
	if strings.HasPrefix(path, "/") {
		return prefix + path
	}
	return prefix + "/" + path
}

// Addressable reports whether the sandbox can be reached at all. It replaces the
// "Pod IP is non-empty" test, which is wrong in both directions once endpoints
// exist: a hostname-addressed sandbox has no meaningful Pod IP, and a placeholder
// Pod IP satisfies the old test while telling nobody anything.
func Addressable(a Attrs) bool {
	switch a.Mode {
	case ModeDirect, modeDirectExplicit:
		return a.PodIP != ""
	case ModeHostname:
		return a.Address != ""
	default:
		return false
	}
}

// Resolve returns the target for one call. port is the port the caller wants on
// the sandbox; under ModeHostname it is rendered into the request rather than
// dialed, because the front's listener port is fixed.
func Resolve(a Attrs, port int) (Target, error) {
	if err := Validate(a); err != nil {
		return Target{}, err
	}
	if port < 1 || port > 65535 {
		return Target{}, fmt.Errorf("sandboxendpoint: port %d is outside 1-65535", port)
	}
	if !isHostname(a.Mode) {
		if a.PodIP == "" {
			return Target{}, fmt.Errorf("sandboxendpoint: direct addressing needs a pod IP")
		}
		return Target{
			DialHost: net.JoinHostPort(a.PodIP, strconv.Itoa(port)),
			Scheme:   "http",
		}, nil
	}

	if a.Address == "" {
		return Target{}, fmt.Errorf("sandboxendpoint: hostname addressing needs an address")
	}
	scheme := a.Scheme
	if scheme == "" {
		scheme = DefaultScheme
	}
	authority := render(a.Authority, port)
	if authority == "" {
		authority = a.Address
	}
	t := Target{
		DialHost:   a.Address,
		Scheme:     scheme,
		Authority:  authority,
		PathPrefix: strings.TrimSuffix(render(a.PathPrefix, port), "/"),
		ViaFront:   true,
	}
	if len(a.Headers) > 0 {
		t.Headers = make(map[string]string, len(a.Headers))
		for k, v := range a.Headers {
			t.Headers[k] = render(v, port)
		}
	}
	return t, nil
}

// Validate rejects an endpoint that cannot work, so the mistake surfaces where it
// is written instead of on every later call.
func Validate(a Attrs) error {
	if a.Mode != ModeDirect && a.Mode != modeDirectExplicit && a.Mode != ModeHostname {
		return fmt.Errorf("sandboxendpoint: unknown mode %q", a.Mode)
	}
	if !isHostname(a.Mode) {
		if a.Address != "" || a.Scheme != "" || a.Authority != "" || a.PathPrefix != "" || len(a.Headers) != 0 {
			return fmt.Errorf("sandboxendpoint: direct mode cannot declare front routing fields")
		}
		return nil
	}
	if a.Address == "" {
		return fmt.Errorf("sandboxendpoint: mode %s requires an address", ModeHostname)
	}
	host, rawPort, err := net.SplitHostPort(a.Address)
	if err != nil || host == "" {
		return fmt.Errorf("sandboxendpoint: address %q must be host:port", a.Address)
	}
	addressPort, err := strconv.Atoi(rawPort)
	if err != nil || addressPort < 1 || addressPort > 65535 {
		return fmt.Errorf("sandboxendpoint: address %q has an invalid port", a.Address)
	}
	if strings.Contains(a.Address, "{") {
		return fmt.Errorf("sandboxendpoint: address %q cannot contain placeholders", a.Address)
	}
	if s := a.Scheme; s != "" && s != "http" && s != "https" {
		return fmt.Errorf("sandboxendpoint: scheme %q must be http or https", s)
	}
	if a.PathPrefix != "" {
		if !strings.HasPrefix(a.PathPrefix, "/") {
			return fmt.Errorf("sandboxendpoint: path prefix %q must start with /", a.PathPrefix)
		}
		if strings.ContainsAny(a.PathPrefix, "?#") {
			return fmt.Errorf("sandboxendpoint: path prefix %q must not contain a query or fragment", a.PathPrefix)
		}
	}
	if a.Authority != "" && !httpguts.ValidHostHeader(render(a.Authority, 1)) {
		return fmt.Errorf("sandboxendpoint: authority is not a valid Host header")
	}
	for name, value := range a.Headers {
		if !httpguts.ValidHeaderFieldName(name) || strings.HasPrefix(name, ":") {
			return fmt.Errorf("sandboxendpoint: header name %q is invalid", name)
		}
		if !httpguts.ValidHeaderFieldValue(value) {
			return fmt.Errorf("sandboxendpoint: header %q contains an invalid value", name)
		}
	}
	for field, value := range map[string]string{
		"authority":   a.Authority,
		"path prefix": a.PathPrefix,
	} {
		if placeholder := unknownPlaceholder(value); placeholder != "" {
			return fmt.Errorf("sandboxendpoint: %s contains unknown placeholder %q", field, placeholder)
		}
	}
	for name, value := range a.Headers {
		if placeholder := unknownPlaceholder(value); placeholder != "" {
			return fmt.Errorf("sandboxendpoint: header %q contains unknown placeholder %q", name, placeholder)
		}
	}
	// The front's listener port is fixed, so an endpoint that renders the port
	// nowhere sends every call to whatever single backend port the front defaults
	// to. That is unrecoverable at call time, hence rejected here.
	if !rendersPort(a) {
		return fmt.Errorf("sandboxendpoint: no slot renders %s, so the target port cannot reach the front", portPlaceholder)
	}
	return nil
}

func rendersPort(a Attrs) bool {
	if strings.Contains(a.Authority, portPlaceholder) || strings.Contains(a.PathPrefix, portPlaceholder) {
		return true
	}
	for _, v := range a.Headers {
		if strings.Contains(v, portPlaceholder) {
			return true
		}
	}
	return false
}

func unknownPlaceholder(value string) string {
	for start := strings.IndexByte(value, '{'); start >= 0; {
		end := strings.IndexByte(value[start:], '}')
		if end < 0 {
			return value[start:]
		}
		end += start
		placeholder := value[start : end+1]
		if placeholder != portPlaceholder {
			return placeholder
		}
		value = value[end+1:]
		start = strings.IndexByte(value, '{')
	}
	return ""
}

func isHostname(mode string) bool { return mode == ModeHostname }

func render(s string, port int) string {
	if s == "" || !strings.Contains(s, portPlaceholder) {
		return s
	}
	return strings.ReplaceAll(s, portPlaceholder, strconv.Itoa(port))
}
