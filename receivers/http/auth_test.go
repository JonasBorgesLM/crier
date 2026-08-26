package httpreceiver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JonasBorgesLM/moat/secret"
)

const testSecret = "correct-horse-battery-staple"

func testCredentials(t *testing.T) *StaticCredentials {
	t.Helper()
	auth, err := NewStaticCredentials(map[string]secret.Value{
		"task-api":     secret.New([]byte(testSecret)),
		"gateway-auth": secret.New([]byte("another-secret")),
	})
	if err != nil {
		t.Fatalf("NewStaticCredentials: %v", err)
	}
	return auth
}

func requestWith(authorization string) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, V1.Path(), strings.NewReader(`{"records":[]}`))
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	return r
}

func TestNewStaticCredentialsValidatesEagerly(t *testing.T) {
	for _, tc := range []struct {
		name  string
		creds map[string]secret.Value
		want  string
	}{
		{
			// Safe, but almost certainly a mistake — and it would surface as
			// "the endpoint rejects everything" hours later.
			name:  "no credentials",
			creds: map[string]secret.Value{},
			want:  "would reject every request",
		},
		{
			name:  "empty source",
			creds: map[string]secret.Value{"": secret.New([]byte("s"))},
			want:  "empty source identifier",
		},
		{
			name:  "source containing the separator",
			creds: map[string]secret.Value{"task:api": secret.New([]byte("s"))},
			want:  "separator",
		},
		{
			name:  "empty credential",
			creds: map[string]secret.Value{"task-api": {}},
			want:  "empty credential",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewStaticCredentials(tc.creds)
			if err == nil {
				t.Fatal("configuration accepted, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestValidCredentialYieldsTheSource(t *testing.T) {
	auth := testCredentials(t)

	source, err := auth.Authenticate(requestWith("Bearer task-api:" + testSecret))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if source != "task-api" {
		t.Errorf("source = %q, want %q", source, "task-api")
	}

	// The scheme is matched without regard to case, as RFC 7235 requires.
	if _, err := auth.Authenticate(requestWith("bearer task-api:" + testSecret)); err != nil {
		t.Errorf("lowercase scheme rejected: %v", err)
	}
}

// The acceptance criterion for #23, first half.
func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	auth := testCredentials(t)

	for _, tc := range []struct{ name, header string }{
		{"no header", ""},
		{"wrong scheme", "Basic dGFzay1hcGk6c2VjcmV0"},
		{"no separator", "Bearer task-api"},
		{"empty source", "Bearer :" + testSecret},
		{"empty secret", "Bearer task-api:"},
		{"unknown source", "Bearer nobody:" + testSecret},
		{"wrong secret for a known source", "Bearer task-api:wrong"},
		{"another source's secret", "Bearer task-api:another-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, err := auth.Authenticate(requestWith(tc.header))
			if err == nil {
				t.Fatalf("authenticated as %q, want a rejection", source)
			}
			if !errors.Is(err, ErrUnauthenticated) {
				t.Errorf("error does not wrap ErrUnauthenticated: %v", err)
			}
			if source != "" {
				t.Errorf("source = %q, want empty on failure", source)
			}
		})
	}
}

// The acceptance criterion for #23, second half: credential material must
// never appear in output. A rejection path is reachable by anyone who can open
// a connection, so anything it prints is effectively public.
func TestCredentialMaterialNeverAppearsInOutput(t *testing.T) {
	auth := testCredentials(t)

	var rendered []string
	for _, header := range []string{
		"Bearer task-api:" + testSecret,
		"Bearer task-api:wrong-but-secret-shaped",
		"Bearer nobody:" + testSecret,
	} {
		if _, err := auth.Authenticate(requestWith(header)); err != nil {
			rendered = append(rendered, err.Error())
		}
	}
	// The configured secrets, however the value is formatted.
	value := secret.New([]byte(testSecret))
	rendered = append(rendered,
		value.String(), fmt.Sprintf("%+v", value),
		fmt.Sprintf("%#v", value), fmt.Sprintf("%v", auth), fmt.Sprint(auth.Sources()),
	)

	for _, out := range rendered {
		for _, leak := range []string{testSecret, "another-secret", "wrong-but-secret-shaped"} {
			if strings.Contains(out, leak) {
				t.Errorf("credential material leaked into %q", out)
			}
		}
	}
}

// Telling a caller that a source exists but the secret is wrong turns the
// credential store into an enumeration oracle, and the caller can do nothing
// with the distinction anyway.
func TestUnknownSourceIsIndistinguishableFromAWrongSecret(t *testing.T) {
	auth := testCredentials(t)

	_, unknownErr := auth.Authenticate(requestWith("Bearer nobody:" + testSecret))
	_, wrongErr := auth.Authenticate(requestWith("Bearer task-api:wrong"))

	if unknownErr == nil || wrongErr == nil {
		t.Fatal("both cases must fail")
	}
	if unknownErr.Error() != wrongErr.Error() {
		t.Errorf("the two cases are distinguishable:\n  unknown source: %v\n  wrong secret:   %v", unknownErr, wrongErr)
	}
}

func TestSourcesListsConfiguredIdentifiers(t *testing.T) {
	got := testCredentials(t).Sources()
	if len(got) != 2 {
		t.Fatalf("Sources() = %v, want 2 entries", got)
	}
	found := map[string]bool{}
	for _, s := range got {
		found[s] = true
	}
	if !found["task-api"] || !found["gateway-auth"] {
		t.Errorf("Sources() = %v, want both configured sources", got)
	}
}
