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

package sandboxendpoint

import (
	"reflect"
	"testing"
)

func TestResolve(t *testing.T) {
	cases := []struct {
		name        string
		attrs       Attrs
		port        int
		wantErr     bool
		wantBaseURL string
		wantTarget  Target
	}{
		{
			name:        "direct keeps today's behavior",
			attrs:       Attrs{PodIP: "10.0.0.1"},
			port:        49983,
			wantBaseURL: "http://10.0.0.1:49983",
			wantTarget:  Target{DialHost: "10.0.0.1:49983", Scheme: "http"},
		},
		{
			name:        "direct spelled out explicitly resolves the same",
			attrs:       Attrs{Mode: "Direct", PodIP: "10.0.0.1"},
			port:        9222,
			wantBaseURL: "http://10.0.0.1:9222",
			wantTarget:  Target{DialHost: "10.0.0.1:9222", Scheme: "http"},
		},
		{
			name:    "direct without a pod IP is an error, not a silent dial",
			attrs:   Attrs{PodIP: ""},
			port:    49983,
			wantErr: true,
		},
		{
			name: "authority slot renders the port and dials the front",
			attrs: Attrs{
				Mode:      ModeHostname,
				PodIP:     "169.254.1.1", // placeholder: must never be dialed
				Address:   "sbx-alb.example.com:443",
				Authority: "{port}-sbx7f3a.sbx.example.com",
			},
			port:        3000,
			wantBaseURL: "https://sbx-alb.example.com:443",
			wantTarget: Target{
				DialHost:  "sbx-alb.example.com:443",
				Scheme:    "https",
				Authority: "3000-sbx7f3a.sbx.example.com",
				ViaFront:  true,
			},
		},
		{
			name: "path slot renders the port into the prefix",
			attrs: Attrs{
				Mode:       ModeHostname,
				Address:    "sbx-alb.example.com:443",
				PathPrefix: "/kruise/sbx7f3a/{port}",
			},
			port:        49983,
			wantBaseURL: "https://sbx-alb.example.com:443/kruise/sbx7f3a/49983",
			wantTarget: Target{
				DialHost:   "sbx-alb.example.com:443",
				Scheme:     "https",
				Authority:  "sbx-alb.example.com:443",
				PathPrefix: "/kruise/sbx7f3a/49983",
				ViaFront:   true,
			},
		},
		{
			name: "header slot renders the port into the value",
			attrs: Attrs{
				Mode:    ModeHostname,
				Address: "sbx-alb.example.com:443",
				Scheme:  "http",
				Headers: map[string]string{"e2b-sandbox-id": "sbx7f3a", "e2b-sandbox-port": "{port}"},
			},
			port:        8080,
			wantBaseURL: "http://sbx-alb.example.com:443",
			wantTarget: Target{
				DialHost:  "sbx-alb.example.com:443",
				Scheme:    "http",
				Authority: "sbx-alb.example.com:443",
				Headers:   map[string]string{"e2b-sandbox-id": "sbx7f3a", "e2b-sandbox-port": "8080"},
				ViaFront:  true,
			},
		},
		{
			name: "slots combine",
			attrs: Attrs{
				Mode:       ModeHostname,
				Address:    "front:80",
				Scheme:     "http",
				Authority:  "{port}-a.example.com",
				PathPrefix: "/kruise/a/{port}/",
				Headers:    map[string]string{"e2b-sandbox-port": "{port}"},
			},
			port:        1234,
			wantBaseURL: "http://front:80/kruise/a/1234",
			wantTarget: Target{
				DialHost:   "front:80",
				Scheme:     "http",
				Authority:  "1234-a.example.com",
				PathPrefix: "/kruise/a/1234", // trailing slash trimmed so paths do not double up
				Headers:    map[string]string{"e2b-sandbox-port": "1234"},
				ViaFront:   true,
			},
		},
		{
			name:    "hostname without an address is an error",
			attrs:   Attrs{Mode: ModeHostname, Authority: "{port}-a.example.com"},
			port:    1,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(tc.attrs, tc.port)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Resolve() error = nil, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if !reflect.DeepEqual(got, tc.wantTarget) {
				t.Errorf("Resolve() = %+v, want %+v", got, tc.wantTarget)
			}
			if base := got.BaseURL(); base != tc.wantBaseURL {
				t.Errorf("BaseURL() = %q, want %q", base, tc.wantBaseURL)
			}
		})
	}
}

func TestAddressable(t *testing.T) {
	cases := []struct {
		name  string
		attrs Attrs
		want  bool
	}{
		{"direct with a pod IP", Attrs{PodIP: "10.0.0.1"}, true},
		{"direct without a pod IP", Attrs{}, false},
		{"hostname with an address", Attrs{Mode: ModeHostname, Address: "front:443"}, true},
		{"hostname without an address", Attrs{Mode: ModeHostname}, false},
		{
			// The case the old Pod-IP test gets wrong in both directions.
			name:  "hostname with a placeholder pod IP is addressable on its address alone",
			attrs: Attrs{Mode: ModeHostname, PodIP: "169.254.1.1", Address: "front:443"},
			want:  true,
		},
		{
			name:  "hostname with a placeholder pod IP but no address is not addressable",
			attrs: Attrs{Mode: ModeHostname, PodIP: "169.254.1.1"},
			want:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Addressable(tc.attrs); got != tc.want {
				t.Errorf("Addressable() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		attrs   Attrs
		wantErr bool
	}{
		{"direct needs nothing", Attrs{}, false},
		{"unknown mode", Attrs{Mode: "Weird"}, true},
		{"direct rejects front fields", Attrs{Mode: modeDirectExplicit, PathPrefix: "/{unknown}"}, true},
		{"hostname needs an address", Attrs{Mode: ModeHostname, Authority: "{port}-a"}, true},
		{"bad scheme", Attrs{Mode: ModeHostname, Address: "f:1", Scheme: "ftp", Authority: "{port}-a"}, true},
		{"path prefix must be rooted", Attrs{Mode: ModeHostname, Address: "f:1", PathPrefix: "kruise/{port}"}, true},
		{"address cannot contain placeholders", Attrs{Mode: ModeHostname, Address: "{port}:443", Authority: "{port}-a"}, true},
		{"path prefix cannot contain query", Attrs{Mode: ModeHostname, Address: "f:1", PathPrefix: "/k/{port}?fixed=1"}, true},
		{"authority rejects spaces", Attrs{Mode: ModeHostname, Address: "f:1", Authority: "bad host-{port}"}, true},
		{"path prefix cannot contain fragment", Attrs{Mode: ModeHostname, Address: "f:1", PathPrefix: "/k/{port}#fragment"}, true},
		{"authority rejects control bytes", Attrs{Mode: ModeHostname, Address: "f:1", Authority: "{port}-a\r\nx: y"}, true},
		{"header value rejects control bytes", Attrs{Mode: ModeHostname, Address: "f:1", Headers: map[string]string{"p": "{port}\r\nx"}}, true},
		{
			// Without this rule every call lands on the front's default backend port.
			name:    "no slot renders the port",
			attrs:   Attrs{Mode: ModeHostname, Address: "f:1", Authority: "a.example.com"},
			wantErr: true,
		},
		{"port in authority", Attrs{Mode: ModeHostname, Address: "f:1", Authority: "{port}-a"}, false},
		{"port in path", Attrs{Mode: ModeHostname, Address: "f:1", PathPrefix: "/k/{port}"}, false},
		{"port in header", Attrs{Mode: ModeHostname, Address: "f:1", Headers: map[string]string{"p": "{port}"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.attrs)
			if tc.wantErr != (err != nil) {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
