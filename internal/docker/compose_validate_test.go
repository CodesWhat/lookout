package docker

import (
	"testing"
)

// TestValidateRequest exercises the security-critical validation paths in
// ComposeManager.validateRequest.
func TestValidateRequest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		req     ComposeRequest
		wantErr bool
	}{
		// ---- Baseline: a well-formed request must be accepted. ----
		{
			name: "valid request is accepted",
			req: ComposeRequest{
				StackName: "myapp",
				Services:  []string{"web", "db"},
				EnvVars: map[string]string{
					"MY_VAR":   "value",
					"MY_VAR_2": "another",
				},
				RegistryAuth: &RegistryAuth{
					Server:   "https://registry.example.com",
					Username: "user",
					Password: "pass",
				},
			},
			wantErr: false,
		},
		{
			name: "request without registry auth is accepted",
			req: ComposeRequest{
				StackName: "myapp",
			},
			wantErr: false,
		},

		// ---- Stack name required. ----
		{
			name:    "empty stack name is rejected",
			req:     ComposeRequest{},
			wantErr: true,
		},

		// ---- Env var value injection. ----
		{
			name: "env var value with newline is rejected",
			req: ComposeRequest{
				StackName: "myapp",
				EnvVars: map[string]string{
					"MY_VAR": "value\nLD_PRELOAD=/evil.so",
				},
			},
			wantErr: true,
		},
		{
			name: "env var value with carriage return is rejected",
			req: ComposeRequest{
				StackName: "myapp",
				EnvVars: map[string]string{
					"MY_VAR": "val\rue",
				},
			},
			wantErr: true,
		},
		{
			name: "env var value with null byte is rejected",
			req: ComposeRequest{
				StackName: "myapp",
				EnvVars: map[string]string{
					"MY_VAR": "val\x00ue",
				},
			},
			wantErr: true,
		},

		// ---- Env var key validation. ----
		{
			name: "env var key with leading digit fails pattern",
			req: ComposeRequest{
				StackName: "myapp",
				EnvVars: map[string]string{
					"1INVALID": "value",
				},
			},
			wantErr: true,
		},
		{
			name: "env var key with hyphen fails pattern",
			req: ComposeRequest{
				StackName: "myapp",
				EnvVars: map[string]string{
					"MY-VAR": "value",
				},
			},
			wantErr: true,
		},
		{
			name: "env var key with space fails pattern",
			req: ComposeRequest{
				StackName: "myapp",
				EnvVars: map[string]string{
					"MY VAR": "value",
				},
			},
			wantErr: true,
		},

		// ---- Env var denylist. ----
		{
			name: "denied key LD_PRELOAD is rejected",
			req: ComposeRequest{
				StackName: "myapp",
				EnvVars: map[string]string{
					"LD_PRELOAD": "/evil.so",
				},
			},
			wantErr: true,
		},
		{
			name: "denied key PATH is rejected",
			req: ComposeRequest{
				StackName: "myapp",
				EnvVars: map[string]string{
					"PATH": "/evil/bin:/usr/bin",
				},
			},
			wantErr: true,
		},
		{
			name: "denied key DOCKER_HOST is rejected",
			req: ComposeRequest{
				StackName: "myapp",
				EnvVars: map[string]string{
					"DOCKER_HOST": "tcp://attacker:2375",
				},
			},
			wantErr: true,
		},
		{
			name: "denied key LD_LIBRARY_PATH is rejected",
			req: ComposeRequest{
				StackName: "myapp",
				EnvVars: map[string]string{
					"LD_LIBRARY_PATH": "/evil/lib",
				},
			},
			wantErr: true,
		},
		{
			name: "denied key BASH_ENV is rejected",
			req: ComposeRequest{
				StackName: "myapp",
				EnvVars: map[string]string{
					"BASH_ENV": "/tmp/evil",
				},
			},
			wantErr: true,
		},

		// ---- Service name validation. ----
		{
			name: "service name beginning with hyphen is rejected",
			req: ComposeRequest{
				StackName: "myapp",
				Services:  []string{"-bad-service"},
			},
			wantErr: true,
		},
		{
			name: "service name beginning with double hyphen is rejected",
			req: ComposeRequest{
				StackName: "myapp",
				Services:  []string{"--rm"},
			},
			wantErr: true,
		},
		{
			name: "normal service name is accepted",
			req: ComposeRequest{
				StackName: "myapp",
				Services:  []string{"web", "db", "cache"},
			},
			wantErr: false,
		},

		// ---- RegistryAuth.Server validation. ----
		{
			name: "registry auth with empty server is rejected",
			req: ComposeRequest{
				StackName: "myapp",
				RegistryAuth: &RegistryAuth{
					Server:   "",
					Username: "user",
					Password: "pass",
				},
			},
			wantErr: true,
		},
		{
			name: "registry auth with http (non-https) server is rejected",
			req: ComposeRequest{
				StackName: "myapp",
				RegistryAuth: &RegistryAuth{
					Server:   "http://registry.example.com",
					Username: "user",
					Password: "pass",
				},
			},
			wantErr: true,
		},
		{
			name: "registry auth with unparseable server URI is rejected",
			req: ComposeRequest{
				StackName: "myapp",
				RegistryAuth: &RegistryAuth{
					Server:   "://not-a-uri",
					Username: "user",
					Password: "pass",
				},
			},
			wantErr: true,
		},
		{
			name: "registry auth with valid https server is accepted",
			req: ComposeRequest{
				StackName: "myapp",
				RegistryAuth: &RegistryAuth{
					Server:   "https://registry.example.com",
					Username: "user",
					Password: "pass",
				},
			},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cm := newTestComposeManager(t)
			err := cm.validateRequest(tc.req)

			if tc.wantErr && err == nil {
				t.Errorf("validateRequest(): expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateRequest(): unexpected error: %v", err)
			}
		})
	}
}

func TestValidateRequestRegistryServerForms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		server  string
		wantErr bool
	}{
		{name: "bare hostname", server: "ghcr.io"},
		{name: "bare hostname and port", server: "registry.example.com:5443"},
		{name: "bare IPv4 and port", server: "127.0.0.1:5443"},
		{name: "bare bracketed IPv6", server: "[2001:db8::1]"},
		{name: "bare bracketed IPv6 and port", server: "[2001:db8::1]:5443"},
		{name: "explicit https hostname", server: "https://registry.example.com"},
		{name: "explicit https hostname and port", server: "https://registry.example.com:5443"},
		{name: "explicit https bracketed IPv6", server: "https://[2001:db8::1]:5443"},
		{name: "empty", wantErr: true},
		{name: "empty host", server: "https://", wantErr: true},
		{name: "http scheme", server: "http://registry.example.com", wantErr: true},
		{name: "userinfo", server: "https://user@registry.example.com", wantErr: true},
		{name: "path", server: "https://registry.example.com/v2", wantErr: true},
		{name: "bare path", server: "registry.example.com/v2", wantErr: true},
		{name: "query", server: "https://registry.example.com?mirror=1", wantErr: true},
		{name: "fragment", server: "https://registry.example.com#fragment", wantErr: true},
		{name: "malformed scheme", server: "://not-a-uri", wantErr: true},
		{name: "space in hostname", server: "registry example.com", wantErr: true},
		{name: "invalid port", server: "registry.example.com:not-a-port", wantErr: true},
		{name: "empty port", server: "registry.example.com:", wantErr: true},
		{name: "explicit empty port", server: "https://registry.example.com:", wantErr: true},
		{name: "out of range port", server: "registry.example.com:65536", wantErr: true},
		{name: "minimum valid port", server: "registry.example.com:1"},
		{name: "maximum valid port", server: "registry.example.com:65535"},
		{name: "port zero", server: "registry.example.com:0", wantErr: true},
		{name: "unbracketed IPv6", server: "2001:db8::1", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cm := newTestComposeManager(t)
			err := cm.validateRequest(ComposeRequest{
				StackName: "app",
				Operation: "up",
				RegistryAuth: &RegistryAuth{
					Server:   tt.server,
					Username: "user",
					Password: "pass",
				},
			})
			if tt.wantErr && err == nil {
				t.Fatal("validateRequest accepted an invalid registry server")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validateRequest rejected a valid registry server: %v", err)
			}
		})
	}
}
