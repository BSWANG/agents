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

package filter

import (
	"crypto/subtle"
	"fmt"
	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"net"
	"strings"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
	"github.com/openkruise/agents/pkg/sandboxendpoint"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
	"github.com/openkruise/agents/pkg/utils"
)

var logger *zap.Logger

func init() {
	config := zap.NewProductionConfig()
	config.Level = zap.NewAtomicLevelAt(zapcore.InfoLevel)
	logger, _ = config.Build()
}

const (
	// accessTokenHeader is the HTTP header name that clients must set
	// to carry the sandbox access token for authentication.
	accessTokenHeader = "x-access-token"
	// runtimeMTLSMetadataNamespace and runtimeMTLSMetadataKey select the mTLS ORIGINAL_DST cluster.
	runtimeMTLSMetadataNamespace = "agents.kruise.io/sandbox-gateway"
	runtimeMTLSMetadataKey       = "upstream-mtls"
)

func FilterFactory(c interface{}, callbacks api.FilterCallbackHandler) api.StreamFilter {
	cfg := c.(*FilterConfig)
	return &sandboxFilter{
		callbacks:      callbacks,
		config:         cfg.Config,
		adapter:        cfg.Adapter,
		jwtAuthManager: cfg.jwtAuthManager,
	}
}

type sandboxFilter struct {
	api.PassThroughStreamFilter
	callbacks      api.FilterCallbackHandler
	config         *Config
	adapter        *adapters.E2BAdapter
	jwtAuthManager JWTAuthManager
}

func (f *sandboxFilter) DecodeHeaders(header api.RequestHeaderMap, endStream bool) api.StatusType {
	// Internal endpoint headers are control-plane output, never client input.
	// Clear them before parsing so every early Continue path is fail-closed.
	header.Del(sandboxendpoint.InternalRouteHeader)
	header.Del(sandboxendpoint.InternalHostHeader)
	header.Del(sandboxendpoint.InternalPortHeader)
	header.Set(sandboxendpoint.InternalRouteHeader, sandboxendpoint.RouteDirect)
	// Step 1: Build flat headers map from the request, including pseudo-headers
	headers := make(map[string]string)
	header.Range(func(key, value string) bool {
		headers[key] = value
		return true
	})

	// Step 2: Use adapter.ParseRequest to normalize the request
	parsed := f.adapter.ParseRequest(headers)

	// Step 3: Use the unified adapter to extract sandbox ID and port
	sandboxID, sandboxPort, extraHeaders, err := f.adapter.Map(parsed)
	if err != nil {
		logger.Debug("Adapter could not extract sandbox info, continuing",
			zap.String("authority", parsed.Authority),
			zap.String("path", parsed.Path),
			zap.Error(err))
		f.callbacks.ClearRouteCache()
		return api.Continue
	}

	logger.Debug("DecodeHeaders: adapter mapped request",
		zap.String("sandboxID", sandboxID),
		zap.Int("sandboxPort", sandboxPort))
	if sandboxPort < 1 || sandboxPort > 65535 {
		f.callbacks.DecoderFilterCallbacks().SendLocalReply(
			400,
			fmt.Sprintf("invalid sandbox port: %d", sandboxPort),
			nil,
			-1,
			"invalid_sandbox_port",
		)
		return api.LocalReply
	}

	// Look up the pod IP from registry. Readiness is read separately from the
	// route lookup, so a concurrent SetReady may flip between the two. ready
	// only moves false->true once at startup and back to false on shutdown, so
	// the worst case is one extra successful read during teardown, which is
	// harmless.
	routeRegistry := registry.GetRegistry()
	if !routeRegistry.Ready() {
		logger.Warn("Sandbox gateway route registry is not ready")
		f.callbacks.DecoderFilterCallbacks().SendLocalReply(
			503,
			"sandbox gateway is not ready",
			nil,
			-1,
			"gateway_not_ready",
		)
		return api.LocalReply
	}
	route, ok := routeRegistry.Get(sandboxID)
	if !ok {
		logger.Warn("Sandbox not found in registry", zap.String("sandboxID", sandboxID))
		f.callbacks.DecoderFilterCallbacks().SendLocalReply(
			502,
			"sandbox not found: "+sandboxID,
			nil,
			-1,
			"sandbox_not_found",
		)
		return api.LocalReply
	}

	if route.State != agentsv1alpha1.SandboxStateRunning {
		logger.Warn("Sandbox is not running", zap.String("sandboxID", sandboxID), zap.String("state", route.State))
		f.callbacks.DecoderFilterCallbacks().SendLocalReply(
			502,
			"healthy sandbox not found: "+sandboxID,
			nil,
			-1,
			"sandbox_not_running",
		)
		return api.LocalReply
	}

	if status := f.authenticate(header, route); status != api.Continue {
		return status
	}

	// Apply adapter normalization before adding the endpoint's own path prefix.
	for k, v := range extraHeaders {
		header.Set(k, v)
	}

	target, err := route.ResolveEndpoint(sandboxPort)
	if err != nil {
		logger.Warn("Sandbox endpoint is not addressable", zap.String("sandboxID", sandboxID), zap.Error(err))
		f.callbacks.DecoderFilterCallbacks().SendLocalReply(
			502,
			"sandbox endpoint is not addressable",
			nil,
			-1,
			"sandbox_endpoint_invalid",
		)
		return api.LocalReply
	}

	if !target.ViaFront {
		header.Set(sandboxendpoint.InternalRouteHeader, sandboxendpoint.RouteDirect)
		f.callbacks.StreamInfo().DynamicMetadata().Set("envoy.lb.original_dst", "host", target.DialHost)
		if f.config.EnableRuntimeMTLS && sandboxPort == utils.RuntimePort {
			f.callbacks.StreamInfo().DynamicMetadata().Set(runtimeMTLSMetadataNamespace, runtimeMTLSMetadataKey, true)
		}
		logger.Debug("Direct upstream override set", zap.String("upstreamHost", target.DialHost))
	} else {
		for name, value := range target.Headers {
			header.Set(name, value)
		}
		if target.Authority != "" {
			header.Set(":authority", target.Authority)
		}
		if target.PathPrefix != "" {
			currentPath := parsed.Path
			if mappedPath, ok := extraHeaders[":path"]; ok {
				currentPath = mappedPath
			}
			header.Set(":path", sandboxendpoint.RewritePath(target.PathPrefix, currentPath))
		}
		host, port, splitErr := net.SplitHostPort(target.DialHost)
		if splitErr != nil {
			f.callbacks.DecoderFilterCallbacks().SendLocalReply(
				502,
				"sandbox front address is invalid",
				nil,
				-1,
				"sandbox_front_address_invalid",
			)
			return api.LocalReply
		}
		header.Set(sandboxendpoint.InternalHostHeader, host)
		header.Set(sandboxendpoint.InternalPortHeader, port)
		header.Set(sandboxendpoint.InternalRouteHeader, sandboxendpoint.RouteFrontHTTP)
		if strings.EqualFold(target.Scheme, "https") {
			header.Set(sandboxendpoint.InternalRouteHeader, sandboxendpoint.RouteFrontHTTPS)
		}
		logger.Debug("Front upstream override set", zap.String("scheme", target.Scheme))
	}
	f.callbacks.ClearRouteCache()
	return api.Continue
}

func (f *sandboxFilter) authenticate(header api.RequestHeaderMap, route sandboxroute.Route) api.StatusType {
	if route.RequireTrafficAuth {
		if !f.config.EnableJWTAuth {
			return f.verifierUnavailable(route.ID)
		}
		return f.authenticateJWT(header, route)
	}
	if f.config.EnableJWTAuth {
		header.Del(f.config.GetTrafficAccessTokenHeader())
		return api.Continue
	}
	if !f.config.EnableAuth {
		return api.Continue
	}
	if route.AccessToken == "" {
		return api.Continue
	}
	requestToken, _ := header.Get(accessTokenHeader)
	if subtle.ConstantTimeCompare([]byte(requestToken), []byte(route.AccessToken)) == 1 {
		return api.Continue
	}
	logger.Warn("Access token mismatch", zap.String("sandboxID", route.ID))
	f.callbacks.DecoderFilterCallbacks().SendLocalReply(
		401,
		"unauthorized: invalid or missing access token",
		nil,
		-1,
		"unauthorized",
	)
	return api.LocalReply
}

func (f *sandboxFilter) authenticateJWT(header api.RequestHeaderMap, route sandboxroute.Route) api.StatusType {
	if f.jwtAuthManager == nil {
		return f.verifierUnavailable(route.ID)
	}
	verifier := f.jwtAuthManager.Current()
	if verifier == nil {
		return f.verifierUnavailable(route.ID)
	}
	headerName := f.config.GetTrafficAccessTokenHeader()
	rawJWT, _ := header.Get(headerName)
	claims, err := verifier.Verify(rawJWT)
	if err != nil {
		logger.Warn("Traffic access token verification failed", zap.String("sandboxID", route.ID), zap.Error(err))
		f.callbacks.DecoderFilterCallbacks().SendLocalReply(
			403,
			"forbidden: invalid or missing traffic access token",
			nil,
			-1,
			"forbidden",
		)
		return api.LocalReply
	}
	if claims.Sandbox.SandboxID != route.ID || claims.Sandbox.SandboxUID != string(route.UID) {
		logger.Warn("Traffic access token sandbox mismatch", zap.String("sandboxID", route.ID))
		f.callbacks.DecoderFilterCallbacks().SendLocalReply(
			403,
			"forbidden: traffic access token does not match sandbox",
			nil,
			-1,
			"forbidden",
		)
		return api.LocalReply
	}
	header.Del(headerName)
	return api.Continue
}

func (f *sandboxFilter) verifierUnavailable(sandboxID string) api.StatusType {
	logger.Warn("Traffic access token verifier is unavailable", zap.String("sandboxID", sandboxID))
	f.callbacks.DecoderFilterCallbacks().SendLocalReply(
		503,
		"service unavailable: traffic access token verifier is not ready",
		nil,
		-1,
		"jwt_verifier_not_ready",
	)
	return api.LocalReply
}
