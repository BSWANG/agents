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

package proxyutils

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/api/v1alpha1"
)

//goland:noinspection DuplicatedCode
func TestRequestSandbox(t *testing.T) {
	// Create test servers using httptest

	testServer := NewTestServer()
	defer testServer.Close()

	// Parse testServer.URL to get IP and port
	parsedURL, err := url.Parse(testServer.URL)
	require.NoError(t, err)
	host, portStr, err := net.SplitHostPort(parsedURL.Host)
	require.NoError(t, err)
	port, _ := strconv.Atoi(portStr)

	tests := []struct {
		name    string
		sandbox *v1alpha1.Sandbox
		wantErr bool
	}{
		{
			name: "running sandbox",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "running-sandbox",
					Namespace: "default",
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						},
					},
					PodInfo: v1alpha1.PodInfo{
						PodIP: host,
					},
				},
			},
		},
		{
			name: "paused sandbox",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "running-sandbox",
					Namespace: "default",
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxPaused,
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						},
					},
					PodInfo: v1alpha1.PodInfo{
						PodIP: testServer.URL,
					},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := requestSandbox(t.Context(), tt.sandbox, "GET", "/", port, nil)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestRequestSandboxViaHostnameEndpoint(t *testing.T) {
	type request struct {
		host   string
		path   string
		header http.Header
	}
	seen := make(chan request, 1)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- request{host: r.Host, path: r.URL.Path, header: r.Header.Clone()}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer front.Close()

	parsed, err := url.Parse(front.URL)
	require.NoError(t, err)
	sandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "hostname-sandbox", Namespace: "default"},
		Status: v1alpha1.SandboxStatus{
			Phase:   v1alpha1.SandboxRunning,
			PodInfo: v1alpha1.PodInfo{PodIP: "169.254.1.1"},
			Endpoint: &v1alpha1.SandboxEndpoint{
				Mode:       v1alpha1.SandboxEndpointModeHostname,
				Address:    parsed.Host,
				Scheme:     "http",
				Authority:  "{port}-hostname-sandbox.sbx.example.com",
				PathPrefix: "/kruise/hostname-sandbox/{port}",
				Headers: map[string]string{
					"e2b-sandbox-id":   "hostname-sandbox",
					"e2b-sandbox-port": "{port}",
				},
			},
		},
	}

	resp, err := requestSandbox(t.Context(), sandbox, http.MethodGet, "/json/version", 9222, nil)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	got := <-seen
	assert.Equal(t, "9222-hostname-sandbox.sbx.example.com", got.host)
	assert.Equal(t, "/kruise/hostname-sandbox/9222/json/version", got.path)
	assert.Equal(t, "hostname-sandbox", got.header.Get("e2b-sandbox-id"))
	assert.Equal(t, "9222", got.header.Get("e2b-sandbox-port"))
}
