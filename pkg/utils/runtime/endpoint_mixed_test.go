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
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

// recorded is one request as the server saw it.
type recorded struct {
	host   string
	path   string
	header http.Header
}

// TestMixedFleetAddressing is the point of the endpoint attribute: one process,
// two sandboxes, different addressing each, decided per call from the object.
//
// The "front" is a real HTTP server that routes on the same contracts a
// sandbox-gateway or an ALB would (authority, path prefix, e2b-sandbox-id), and
// the "direct" sandbox is a second server addressed by its own address. A single
// runtime client is used for both, so any leakage between the two shows up.
func TestMixedFleetAddressing(t *testing.T) {
	var mu sync.Mutex
	var directSeen, frontSeen []recorded

	directSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		directSeen = append(directSeen, recorded{host: r.Host, path: r.URL.Path, header: r.Header.Clone()})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer directSrv.Close()

	// The front demands that the request identify the sandbox, exactly as a real
	// L7 front does: no identification, no route.
	frontSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		frontSeen = append(frontSeen, recorded{host: r.Host, path: r.URL.Path, header: r.Header.Clone()})
		mu.Unlock()
		id := r.Header.Get("e2b-sandbox-id")
		if id == "" && !strings.HasPrefix(r.Host, "49983-") && !strings.HasPrefix(r.URL.Path, "/kruise/") {
			// 5xx rather than 404, so the caller retries during startup.
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer frontSrv.Close()

	frontAddr := mustHostPort(t, frontSrv.URL)

	direct := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-direct", Namespace: "default"},
		Status: agentsv1alpha1.SandboxStatus{
			Phase:   agentsv1alpha1.SandboxRunning,
			PodInfo: agentsv1alpha1.PodInfo{PodIP: mustHost(t, directSrv.URL)},
		},
	}
	// Deliberately a placeholder Pod IP: nothing may dial it, and it must not make
	// the sandbox look addressable on its own.
	hostname := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-hostname", Namespace: "default"},
		Status: agentsv1alpha1.SandboxStatus{
			Phase:   agentsv1alpha1.SandboxRunning,
			PodInfo: agentsv1alpha1.PodInfo{PodIP: "169.254.1.1"},
			Endpoint: &agentsv1alpha1.SandboxEndpoint{
				Mode:      agentsv1alpha1.SandboxEndpointModeHostname,
				Address:   frontAddr,
				Scheme:    "http",
				Authority: "{port}-sbx-hostname.sbx.example.com",
				Headers:   map[string]string{"e2b-sandbox-id": "sbx-hostname", "e2b-sandbox-port": "{port}"},
			},
		},
	}

	// The direct sandbox's "pod IP" is a test server host, so the port has to match
	// that server rather than the well-known runtime port.
	directPort := mustPort(t, directSrv.URL)

	t.Run("direct sandbox still dials its own address", func(t *testing.T) {
		rc := &runtimeClient{sbx: direct, client: directSrv.Client()}
		base, client, err := rc.resolveTransport(direct, directSrv.Client())
		if err != nil {
			t.Fatalf("resolveTransport() error = %v", err)
		}
		// GetRuntimeURL uses the well-known runtime port; point the check at the
		// test server instead so we exercise the transport, not the constant.
		if want := fmt.Sprintf("http://%s:%d", direct.Status.PodInfo.PodIP, utils.RuntimePort); base != want {
			t.Fatalf("base URL = %q, want %q", base, want)
		}
		if client == nil {
			t.Fatal("resolveTransport() returned a nil client")
		}
		resp, err := client.Get(fmt.Sprintf("http://%s:%s/health", direct.Status.PodInfo.PodIP, directPort))
		if err != nil {
			t.Fatalf("direct request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("direct status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("hostname sandbox dials the front and identifies itself", func(t *testing.T) {
		rc := &runtimeClient{sbx: hostname, client: frontSrv.Client()}
		base, client, err := rc.resolveTransport(hostname, frontSrv.Client())
		if err != nil {
			t.Fatalf("resolveTransport() error = %v", err)
		}
		if want := "http://" + frontAddr; base != want {
			t.Fatalf("base URL = %q, want %q (must be the front, never the placeholder pod IP)", base, want)
		}
		resp, err := client.Get(base + "/health")
		if err != nil {
			t.Fatalf("front request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("front status = %d, want 200 — the front did not recognize the sandbox", resp.StatusCode)
		}
	})

	mu.Lock()
	defer mu.Unlock()

	if len(directSeen) != 1 {
		t.Fatalf("direct server saw %d requests, want 1", len(directSeen))
	}
	if len(frontSeen) != 1 {
		t.Fatalf("front saw %d requests, want 1", len(frontSeen))
	}

	// The front must have been told which sandbox and which port, with the port
	// rendered at call time.
	got := frontSeen[0]
	wantAuthority := fmt.Sprintf("%d-sbx-hostname.sbx.example.com", utils.RuntimePort)
	if got.host != wantAuthority {
		t.Errorf("front saw Host = %q, want %q", got.host, wantAuthority)
	}
	if id := got.header.Get("e2b-sandbox-id"); id != "sbx-hostname" {
		t.Errorf("front saw e2b-sandbox-id = %q, want %q", id, "sbx-hostname")
	}
	if p := got.header.Get("e2b-sandbox-port"); p != fmt.Sprint(utils.RuntimePort) {
		t.Errorf("front saw e2b-sandbox-port = %q, want %d", p, utils.RuntimePort)
	}

	// No decoration may leak onto the direct sandbox: that would mean the decision
	// was made per process rather than per sandbox.
	if id := directSeen[0].header.Get("e2b-sandbox-id"); id != "" {
		t.Errorf("direct request carried e2b-sandbox-id = %q, want none", id)
	}
	if strings.Contains(directSeen[0].host, "sbx.example.com") {
		t.Errorf("direct request carried the front authority %q", directSeen[0].host)
	}
}

func TestHostnameCallIgnoresDirectTLSBundleError(t *testing.T) {
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer front.Close()

	sbx := &agentsv1alpha1.Sandbox{
		Status: agentsv1alpha1.SandboxStatus{
			Endpoint: &agentsv1alpha1.SandboxEndpoint{
				Mode:      agentsv1alpha1.SandboxEndpointModeHostname,
				Address:   mustHostPort(t, front.URL),
				Scheme:    "http",
				Authority: "{port}-sandbox.example.com",
			},
		},
	}
	client := &runtimeClient{
		sbx:          sbx,
		client:       front.Client(),
		timeout:      time.Second,
		tlsEnabled:   true,
		tlsConfigErr: fmt.Errorf("broken direct runtime bundle"),
	}

	err := client.call(context.Background(), http.MethodGet, "/health", nil, nil)

	if err != nil {
		t.Fatalf("Hostname call failed on irrelevant Direct TLS configuration: %v", err)
	}
}

// TestTransportLogValuesReportTheFront pins what an operator can conclude from
// the logs of a Hostname call made by a TLS-enabled client. The values must
// describe the front hop, because the TLS-mode values would name a dial target
// derived from a Pod IP that is a placeholder under Hostname and was never
// dialled — the exact confusion this addressing change exists to remove.
func TestTransportLogValuesReportTheFront(t *testing.T) {
	tests := []struct {
		name       string
		endpoint   *agentsv1alpha1.SandboxEndpoint
		wantValues map[string]any
		wantAbsent []string
	}{
		{
			name: "resolved front",
			endpoint: &agentsv1alpha1.SandboxEndpoint{
				Mode:      agentsv1alpha1.SandboxEndpointModeHostname,
				Address:   "front.example.com:443",
				Scheme:    "https",
				Authority: "{port}-sbx7f3a.sbx.example.com",
			},
			wantValues: map[string]any{
				"transport":         "front",
				"runtimeTLSApplied": false,
				"forcedResolution":  false,
				"endpoint":          "https://front.example.com:443",
				"authority":         "49984-sbx7f3a.sbx.example.com",
				"sandboxPort":       49984,
			},
			wantAbsent: []string{"dialTarget", "tlsPort"},
		},
		{
			name: "unaddressable front reports the reason, not a pod IP",
			endpoint: &agentsv1alpha1.SandboxEndpoint{
				Mode: agentsv1alpha1.SandboxEndpointModeHostname,
			},
			wantValues: map[string]any{
				"transport":         "front",
				"runtimeTLSApplied": false,
			},
			wantAbsent: []string{"dialTarget", "endpoint"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					PodInfo:  agentsv1alpha1.PodInfo{PodIP: "169.254.1.1"},
					Endpoint: tt.endpoint,
				},
			}
			client := &runtimeClient{sbx: sbx, tlsEnabled: true, tlsPort: RuntimeTLSPort, authority: RuntimeServerSNI}

			got := keyValues(t, client.transportLogValues(sbx))

			for key, want := range tt.wantValues {
				if got[key] != want {
					t.Errorf("log value %q = %v, want %v", key, got[key], want)
				}
			}
			for _, key := range tt.wantAbsent {
				if _, ok := got[key]; ok {
					t.Errorf("log value %q must not be reported for a front call, got %v", key, got[key])
				}
			}
			for key, value := range got {
				if str, ok := value.(string); ok && strings.Contains(str, "169.254.1.1") {
					t.Errorf("log value %q leaks the placeholder pod IP: %v", key, str)
				}
			}
		})
	}
}

// keyValues turns structured log key-values into a map so a test can assert on
// individual keys without depending on their order.
func keyValues(t *testing.T, values []any) map[string]any {
	t.Helper()
	if len(values)%2 != 0 {
		t.Fatalf("log values must be pairs, got %d entries", len(values))
	}
	out := make(map[string]any, len(values)/2)
	for i := 0; i < len(values); i += 2 {
		key, ok := values[i].(string)
		if !ok {
			t.Fatalf("log key at %d is not a string: %v", i, values[i])
		}
		out[key] = values[i+1]
	}
	return out
}

// TestAddressableAcrossModes covers the readiness rule the projection replaces.
func TestAddressableAcrossModes(t *testing.T) {
	cases := []struct {
		name string
		sbx  *agentsv1alpha1.Sandbox
		want bool
	}{
		{"nil sandbox", nil, false},
		{
			name: "direct with a pod IP",
			sbx:  &agentsv1alpha1.Sandbox{Status: agentsv1alpha1.SandboxStatus{PodInfo: agentsv1alpha1.PodInfo{PodIP: "10.0.0.1"}}},
			want: true,
		},
		{
			name: "direct without a pod IP",
			sbx:  &agentsv1alpha1.Sandbox{},
			want: false,
		},
		{
			name: "hostname with a placeholder pod IP is addressable on its address",
			sbx: &agentsv1alpha1.Sandbox{Status: agentsv1alpha1.SandboxStatus{
				PodInfo:  agentsv1alpha1.PodInfo{PodIP: "169.254.1.1"},
				Endpoint: &agentsv1alpha1.SandboxEndpoint{Mode: agentsv1alpha1.SandboxEndpointModeHostname, Address: "front:443"},
			}},
			want: true,
		},
		{
			name: "hostname without an address is not addressable even with a pod IP",
			sbx: &agentsv1alpha1.Sandbox{Status: agentsv1alpha1.SandboxStatus{
				PodInfo:  agentsv1alpha1.PodInfo{PodIP: "169.254.1.1"},
				Endpoint: &agentsv1alpha1.SandboxEndpoint{Mode: agentsv1alpha1.SandboxEndpointModeHostname},
			}},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Addressable(tc.sbx); got != tc.want {
				t.Errorf("Addressable() = %v, want %v", got, tc.want)
			}
		})
	}
}

func mustHostPort(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Host
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Hostname()
}

func mustPort(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Port()
}
