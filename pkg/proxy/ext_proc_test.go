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

package proxy

import (
	"context"
	"io"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	types "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	k8stypes "k8s.io/apimachinery/pkg/types"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandboxendpoint"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
)

// testRequestAdapter is a RequestAdapter implementation for testing
type testRequestAdapter struct {
	entry            string
	isSandboxRequest bool
	mapResult        mapResult
}

type mapResult struct {
	sandboxID    string
	sandboxPort  int
	extraHeaders map[string]string
	err          error
}

func (t *testRequestAdapter) Map(*adapters.ParsedRequest) (string, int, map[string]string, error) {
	return t.mapResult.sandboxID, t.mapResult.sandboxPort, t.mapResult.extraHeaders, t.mapResult.err
}

func (t *testRequestAdapter) IsSandboxRequest(string, string, int) bool {
	return t.isSandboxRequest
}

func (t *testRequestAdapter) Entry() string {
	return t.entry
}

func (t *testRequestAdapter) ParseRequest(headers map[string]string) *adapters.ParsedRequest {
	// Use a real E2BAdapter to parse, so tests exercise the real parsing logic
	a := adapters.NewE2BAdapter(0)
	return a.ParseRequest(headers)
}

// mockProcessServer is a mock implementation of the ExternalProcessor_ProcessServer interface
type mockProcessServer struct {
	extProcPb.ExternalProcessor_ProcessServer
	ctx  context.Context
	reqs []*extProcPb.ProcessingRequest
	resp []*extProcPb.ProcessingResponse
	err  error
}

func (m *mockProcessServer) Context() context.Context {
	if m.ctx != nil {
		return m.ctx
	}
	return context.Background()
}

func (m *mockProcessServer) Send(resp *extProcPb.ProcessingResponse) error {
	if m.err != nil {
		return m.err
	}
	m.resp = append(m.resp, resp)
	return nil
}

func (m *mockProcessServer) Recv() (*extProcPb.ProcessingRequest, error) {
	if m.err != nil {
		return nil, m.err
	}

	if len(m.reqs) == 0 {
		return nil, io.EOF
	}

	req := m.reqs[0]
	m.reqs = m.reqs[1:]
	return req, nil
}

func TestServer_Process(t *testing.T) {
	tests := []struct {
		name        string
		setupRoutes []sandboxroute.Route
		adapter     *testRequestAdapter
		requests    []*extProcPb.ProcessingRequest
		serverError error
		expectError bool
		expectResp  []*extProcPb.ProcessingResponse
	}{
		{
			name: "normal",
			setupRoutes: []sandboxroute.Route{
				{ID: "sandbox1", IP: "192.168.1.10", Owner: "user1"},
			},
			adapter: &testRequestAdapter{
				isSandboxRequest: true,
				mapResult: mapResult{
					sandboxID:   "sandbox1",
					sandboxPort: 8080,
					err:         nil,
				},
				entry: "127.0.0.1:8080",
			},
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_RequestHeaders{
						RequestHeaders: &extProcPb.HttpHeaders{
							Headers: &corev3.HeaderMap{
								Headers: []*corev3.HeaderValue{
									{Key: ":scheme", RawValue: []byte("http")},
									{Key: ":authority", RawValue: []byte("localhost:9002")},
									{Key: ":path", RawValue: []byte("/sandbox")},
								},
							},
						},
					},
				},
			},
			expectError: false,
			expectResp: []*extProcPb.ProcessingResponse{
				{
					Response: &extProcPb.ProcessingResponse_RequestHeaders{
						RequestHeaders: &extProcPb.HeadersResponse{
							Response: &extProcPb.CommonResponse{
								HeaderMutation: &extProcPb.HeaderMutation{
									SetHeaders: []*corev3.HeaderValueOption{
										{
											Header: &corev3.HeaderValue{
												Key:      "x-envoy-original-dst-host",
												RawValue: []byte("192.168.1.10:8080"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name:        "non-sandbox",
			setupRoutes: []sandboxroute.Route{},
			adapter: &testRequestAdapter{
				isSandboxRequest: false,
				entry:            "127.0.0.1:8080",
			},
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_RequestHeaders{
						RequestHeaders: &extProcPb.HttpHeaders{
							Headers: &corev3.HeaderMap{
								Headers: []*corev3.HeaderValue{
									{Key: ":scheme", RawValue: []byte("http")},
									{Key: ":authority", RawValue: []byte("api.example.com")},
									{Key: ":path", RawValue: []byte("/api")},
								},
							},
						},
					},
				},
			},
			expectError: false,
			expectResp: []*extProcPb.ProcessingResponse{
				{
					Response: &extProcPb.ProcessingResponse_RequestHeaders{
						RequestHeaders: &extProcPb.HeadersResponse{
							Response: &extProcPb.CommonResponse{
								HeaderMutation: &extProcPb.HeaderMutation{
									SetHeaders: []*corev3.HeaderValueOption{
										{
											Header: &corev3.HeaderValue{
												Key:      "x-envoy-original-dst-host",
												RawValue: []byte("127.0.0.1:8080"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
		{
			name: "mapping failed",
			setupRoutes: []sandboxroute.Route{
				{ID: "sandbox1", IP: "192.168.1.10", Owner: "user1"},
			},
			adapter: &testRequestAdapter{
				isSandboxRequest: true,
				mapResult: mapResult{
					sandboxID:   "",
					sandboxPort: 0,
					err:         status.Errorf(codes.Internal, "mapping failed"),
				},
				entry: "127.0.0.1:8080",
			},
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_RequestHeaders{
						RequestHeaders: &extProcPb.HttpHeaders{
							Headers: &corev3.HeaderMap{
								Headers: []*corev3.HeaderValue{
									{Key: ":scheme", RawValue: []byte("http")},
									{Key: ":authority", RawValue: []byte("localhost:9002")},
									{Key: ":path", RawValue: []byte("/sandbox")},
								},
							},
						},
					},
				},
			},
			expectError: false,
			expectResp: []*extProcPb.ProcessingResponse{
				{
					Response: &extProcPb.ProcessingResponse_ImmediateResponse{
						ImmediateResponse: &extProcPb.ImmediateResponse{
							Status: &types.HttpStatus{
								Code: types.StatusCode(500),
							},
							Body: []byte("failed to map request to sandbox, URL=http://localhost:9002/sandbox"),
						},
					},
				},
			},
		},
		{
			name:        "route not found",
			setupRoutes: []sandboxroute.Route{},
			adapter: &testRequestAdapter{
				isSandboxRequest: true,
				mapResult: mapResult{
					sandboxID:   "nonexistent",
					sandboxPort: 8080,
					err:         nil,
				},
				entry: "127.0.0.1:8080",
			},
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_RequestHeaders{
						RequestHeaders: &extProcPb.HttpHeaders{
							Headers: &corev3.HeaderMap{
								Headers: []*corev3.HeaderValue{
									{Key: ":scheme", RawValue: []byte("http")},
									{Key: ":authority", RawValue: []byte("localhost:9002")},
									{Key: ":path", RawValue: []byte("/sandbox")},
								},
							},
						},
					},
				},
			},
			expectError: false,
			expectResp: []*extProcPb.ProcessingResponse{
				{
					Response: &extProcPb.ProcessingResponse_ImmediateResponse{
						ImmediateResponse: &extProcPb.ImmediateResponse{
							Status: &types.HttpStatus{
								Code: types.StatusCode(502),
							},
							Body: []byte("sandbox nonexistent not found"),
						},
					},
				},
			},
		},
		{
			name:        "bad port",
			setupRoutes: []sandboxroute.Route{},
			adapter: &testRequestAdapter{
				isSandboxRequest: true,
				mapResult: mapResult{
					sandboxID:   "sandbox1",
					sandboxPort: 99999,
					err:         nil,
				},
				entry: "127.0.0.1:8080",
			},
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_RequestHeaders{
						RequestHeaders: &extProcPb.HttpHeaders{
							Headers: &corev3.HeaderMap{
								Headers: []*corev3.HeaderValue{
									{Key: ":scheme", RawValue: []byte("http")},
									{Key: ":authority", RawValue: []byte("99999-id.example.com")},
									{Key: ":path", RawValue: []byte("/sandbox")},
								},
							},
						},
					},
				},
			},
			expectError: false,
			expectResp: []*extProcPb.ProcessingResponse{
				{
					Response: &extProcPb.ProcessingResponse_ImmediateResponse{
						ImmediateResponse: &extProcPb.ImmediateResponse{
							Status: &types.HttpStatus{
								Code: types.StatusCode(400),
							},
							Body: []byte("invalid sandbox port: 99999"),
						},
					},
				},
			},
		},
		{
			name:        "sandbox not healthy",
			setupRoutes: []sandboxroute.Route{{ID: "sandbox1", IP: "192.168.1.10", Owner: "user1", State: agentsv1alpha1.SandboxStateDead}},
			adapter: &testRequestAdapter{
				isSandboxRequest: true,
				mapResult: mapResult{
					sandboxID:   "sandbox1",
					sandboxPort: 9999,
					err:         nil,
				},
				entry: "127.0.0.1:8080",
			},
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_RequestHeaders{
						RequestHeaders: &extProcPb.HttpHeaders{
							Headers: &corev3.HeaderMap{
								Headers: []*corev3.HeaderValue{
									{Key: ":scheme", RawValue: []byte("http")},
									{Key: ":authority", RawValue: []byte("99999-id.example.com")},
									{Key: ":path", RawValue: []byte("/sandbox")},
								},
							},
						},
					},
				},
			},
			expectError: false,
			expectResp: []*extProcPb.ProcessingResponse{
				{
					Response: &extProcPb.ProcessingResponse_ImmediateResponse{
						ImmediateResponse: &extProcPb.ImmediateResponse{
							Status: &types.HttpStatus{
								Code: types.StatusCode(502),
							},
							Body: []byte("healthy sandbox sandbox1 not found"),
						},
					},
				},
			},
		},
		{
			name:        "receive failed",
			setupRoutes: []sandboxroute.Route{},
			adapter: &testRequestAdapter{
				entry: "127.0.0.1:8080",
			},
			requests:    []*extProcPb.ProcessingRequest{},
			serverError: status.Errorf(codes.Unknown, "receive error"),
			expectError: true,
		},
		{
			name: "send response error",
			setupRoutes: []sandboxroute.Route{
				{ID: "sandbox1", IP: "192.168.1.10", Owner: "user1"},
			},
			adapter: &testRequestAdapter{
				isSandboxRequest: true,
				mapResult: mapResult{
					sandboxID:   "sandbox1",
					sandboxPort: 8080,
					err:         nil,
				},
				entry: "127.0.0.1:8080",
			},
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_RequestHeaders{
						RequestHeaders: &extProcPb.HttpHeaders{
							Headers: &corev3.HeaderMap{
								Headers: []*corev3.HeaderValue{
									{Key: ":scheme", RawValue: []byte("http")},
									{Key: ":authority", RawValue: []byte("localhost:9002")},
									{Key: ":path", RawValue: []byte("/sandbox")},
								},
							},
						},
					},
				},
			},
			serverError: status.Errorf(codes.Unknown, "send error"),
			expectError: true,
		},
		{
			name:        "unknown request type",
			setupRoutes: []sandboxroute.Route{},
			adapter: &testRequestAdapter{
				entry: "127.0.0.1:8080",
			},
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_ResponseHeaders{
						ResponseHeaders: &extProcPb.HttpHeaders{},
					},
				},
			},
			expectError: false,
			expectResp: []*extProcPb.ProcessingResponse{
				{
					Response: &extProcPb.ProcessingResponse_RequestHeaders{
						RequestHeaders: &extProcPb.HeadersResponse{
							Response: &extProcPb.CommonResponse{},
						},
					},
				},
			},
		},
		{
			name: "extra headers",
			setupRoutes: []sandboxroute.Route{
				{ID: "sandbox1", IP: "192.168.1.10", Owner: "user1"},
			},
			adapter: &testRequestAdapter{
				isSandboxRequest: true,
				mapResult: mapResult{
					sandboxID:   "sandbox1",
					sandboxPort: 8080,
					extraHeaders: map[string]string{
						"foo": "bar",
					},
					err: nil,
				},
				entry: "127.0.0.1:8080",
			},
			requests: []*extProcPb.ProcessingRequest{
				{
					Request: &extProcPb.ProcessingRequest_RequestHeaders{
						RequestHeaders: &extProcPb.HttpHeaders{
							Headers: &corev3.HeaderMap{
								Headers: []*corev3.HeaderValue{
									{Key: ":scheme", RawValue: []byte("http")},
									{Key: ":authority", RawValue: []byte("localhost:9002")},
									{Key: ":path", RawValue: []byte("/sandbox")},
								},
							},
						},
					},
				},
			},
			expectError: false,
			expectResp: []*extProcPb.ProcessingResponse{
				{
					Response: &extProcPb.ProcessingResponse_RequestHeaders{
						RequestHeaders: &extProcPb.HeadersResponse{
							Response: &extProcPb.CommonResponse{
								HeaderMutation: &extProcPb.HeaderMutation{
									SetHeaders: []*corev3.HeaderValueOption{
										{
											Header: &corev3.HeaderValue{
												Key:      "foo",
												RawValue: []byte("bar"),
											},
										},
										{
											Header: &corev3.HeaderValue{
												Key:      "x-envoy-original-dst-host",
												RawValue: []byte("192.168.1.10:8080"),
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create server
			server := NewServer(config.SandboxManagerOptions{ExtProcMaxConcurrency: 1000})
			server.SetRequestAdapter(tt.adapter)

			// Setup routes
			for _, route := range tt.setupRoutes {
				if route.State == "" {
					route.State = agentsv1alpha1.SandboxStateRunning
				}
				if route.UID == "" {
					route.UID = k8stypes.UID("uid-" + route.ID)
				}
				if route.ResourceVersion == "" {
					route.ResourceVersion = "1"
				}
				if route.Namespace == "" {
					route.Namespace = "ns"
				}
				if route.Name == "" {
					route.Name = route.ID
				}
				server.SetRoute(route)
			}

			// Create mock processing server
			mockServer := &mockProcessServer{
				reqs: tt.requests,
				err:  tt.serverError,
			}

			// Execute test
			err := server.Process(mockServer)

			// Verify results
			if tt.expectError {
				if err == nil {
					t.Errorf("an error is expected")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}

				// Verify response count
				if len(mockServer.resp) != len(tt.expectResp) {
					t.Errorf("expect %d responses, got %d", len(tt.expectResp), len(mockServer.resp))
				}
				// Verify response content
				for i, expected := range tt.expectResp {
					if i >= len(mockServer.resp) {
						break
					}

					actual := mockServer.resp[i]

					// Check response type
					switch expected.Response.(type) {
					case *extProcPb.ProcessingResponse_RequestHeaders:
						if actualHeader, ok := actual.Response.(*extProcPb.ProcessingResponse_RequestHeaders); ok {
							expectedHeader := expected.Response.(*extProcPb.ProcessingResponse_RequestHeaders)
							// Check if HeaderMutation exists
							if expectedHeader.RequestHeaders.Response.HeaderMutation != nil {
								if actualHeader.RequestHeaders.Response.HeaderMutation == nil {
									t.Errorf("expect HeaderMutation")
								} else {
									expectedHeaders := expectedHeader.RequestHeaders.Response.HeaderMutation.SetHeaders
									actualHeaders := actualHeader.RequestHeaders.Response.HeaderMutation.SetHeaders
									actualByName := make(map[string]string, len(actualHeaders))
									for _, header := range actualHeaders {
										actualByName[header.Header.Key] = string(header.Header.RawValue)
									}
									for _, expectedHeader := range expectedHeaders {
										actualValue, found := actualByName[expectedHeader.Header.Key]
										if !found {
											t.Errorf("expected header %s was not set", expectedHeader.Header.Key)
											continue
										}
										if expectedValue := string(expectedHeader.Header.RawValue); actualValue != expectedValue {
											t.Errorf("header %s not match, expect: %s, actual: %s",
												expectedHeader.Header.Key, expectedValue, actualValue)
										}
									}
								}
							}
						} else {
							t.Errorf("response type mismatch, expected RequestHeaders")
						}
					case *extProcPb.ProcessingResponse_ImmediateResponse:
						if actualImmediate, ok := actual.Response.(*extProcPb.ProcessingResponse_ImmediateResponse); ok {
							expectedImmediate := expected.Response.(*extProcPb.ProcessingResponse_ImmediateResponse)
							// Check status code
							assert.Equal(t, expectedImmediate.ImmediateResponse.Status.Code, actualImmediate.ImmediateResponse.Status.Code)
							// Check response body
							assert.Contains(t, string(actualImmediate.ImmediateResponse.Body), string(expectedImmediate.ImmediateResponse.Body))
						} else {
							t.Errorf("response type mismatch, expected ImmediateResponse")
						}
					}
				}
			}
		})
	}
}

// TestServer_Run_Stop tests server start and stop
func TestServer_Run_Stop(t *testing.T) {
	// Create test adapter
	adapter := &testRequestAdapter{
		entry: "127.0.0.1:8080",
	}

	// Create server
	server := NewServer(config.SandboxManagerOptions{ExtProcMaxConcurrency: 1000})

	server.SetRequestAdapter(adapter)

	// Start server in background
	go func() {
		// This will fail due to port occupation, but we only care about API calls
		_ = server.Run()
	}()

	// Wait a bit for goroutine to start
	time.Sleep(10 * time.Millisecond)

	// Stop server
	server.Stop(t.Context())
}
func TestHandleRequestHeadersHostnameEndpoint(t *testing.T) {
	server := NewServer(config.SandboxManagerOptions{})
	server.SetRequestAdapter(&testRequestAdapter{
		isSandboxRequest: true,
		mapResult: mapResult{
			sandboxID:   "sandbox1",
			sandboxPort: 9222,
		},
	})
	server.SetRoute(sandboxroute.Route{
		ID:              "sandbox1",
		Namespace:       "ns",
		Name:            "sandbox1",
		UID:             "uid-sandbox1",
		State:           agentsv1alpha1.SandboxStateRunning,
		ResourceVersion: "1",
		Endpoint: &agentsv1alpha1.SandboxEndpoint{
			Mode:       agentsv1alpha1.SandboxEndpointModeHostname,
			Address:    "front.example.com:8443",
			Scheme:     "https",
			Authority:  "{port}-sandbox1.sbx.example.com",
			PathPrefix: "/relay/sandbox1/{port}",
			Headers:    map[string]string{"x-sandbox": "sandbox1", "x-port": "{port}"},
		},
	})
	request := &extProcPb.ProcessingRequest_RequestHeaders{
		RequestHeaders: &extProcPb.HttpHeaders{Headers: &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
			{Key: ":scheme", RawValue: []byte("http")},
			{Key: ":authority", RawValue: []byte("incoming.example.com")},
			{Key: ":path", RawValue: []byte("/original?q=1")},
			{Key: "request-header-modifier", RawValue: []byte(`{"x-agents-sandbox-endpoint-route":"front-http","x-agents-sandbox-endpoint-host":"attacker.example.com","x-sandbox":"attacker"}`)},
			{Key: OrigDstHeader, RawValue: []byte("attacker.example.com:8080")},
		}}},
	}

	response := server.handleRequestHeaders(request, logr.Discard())

	headerResponse := response.Response.(*extProcPb.ProcessingResponse_RequestHeaders)
	assert.True(t, headerResponse.RequestHeaders.Response.ClearRouteCache)
	got := make(map[string]string)
	actions := make(map[string]corev3.HeaderValueOption_HeaderAppendAction)
	for _, header := range headerResponse.RequestHeaders.Response.HeaderMutation.SetHeaders {
		got[header.Header.Key] = string(header.Header.RawValue)
		actions[header.Header.Key] = header.AppendAction
	}
	for _, name := range []string{
		sandboxendpoint.InternalRouteHeader,
		sandboxendpoint.InternalHostHeader,
		sandboxendpoint.InternalPortHeader,
		OrigDstHeader,
	} {
		assert.Equal(t, corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD, actions[name])
	}
	assert.Equal(t, sandboxendpoint.RouteFrontHTTPS, got[sandboxendpoint.InternalRouteHeader])
	assert.Equal(t, "front.example.com", got[sandboxendpoint.InternalHostHeader])
	assert.Equal(t, "8443", got[sandboxendpoint.InternalPortHeader])
	assert.Equal(t, "9222-sandbox1.sbx.example.com", got[":authority"])
	assert.Equal(t, "/relay/sandbox1/9222/original?q=1", got[":path"])
	assert.Equal(t, "sandbox1", got["x-sandbox"])
	assert.Equal(t, "9222", got["x-port"])
	assert.Equal(t, "front.example.com:8443", got[OrigDstHeader])
}

func TestNonSandboxRequestClearsUntrustedEndpointRoute(t *testing.T) {
	server := NewServer(config.SandboxManagerOptions{})
	server.SetRequestAdapter(&testRequestAdapter{isSandboxRequest: false})
	request := &extProcPb.ProcessingRequest_RequestHeaders{
		RequestHeaders: &extProcPb.HttpHeaders{Headers: &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
			{Key: sandboxendpoint.InternalRouteHeader, RawValue: []byte(sandboxendpoint.RouteFrontHTTPS)},
			{Key: sandboxendpoint.InternalHostHeader, RawValue: []byte("attacker.example.com")},
		}}},
	}

	response := server.handleRequestHeaders(request, logr.Discard())

	headerResponse := response.Response.(*extProcPb.ProcessingResponse_RequestHeaders)
	assert.True(t, headerResponse.RequestHeaders.Response.ClearRouteCache)
	got := make(map[string]string)
	for _, header := range headerResponse.RequestHeaders.Response.HeaderMutation.SetHeaders {
		got[header.Header.Key] = string(header.Header.RawValue)
		if header.Header.Key == sandboxendpoint.InternalRouteHeader || header.Header.Key == OrigDstHeader {
			assert.Equal(t, corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD, header.AppendAction)
		}
	}
	assert.Equal(t, sandboxendpoint.RouteDirect, got[sandboxendpoint.InternalRouteHeader])
}
