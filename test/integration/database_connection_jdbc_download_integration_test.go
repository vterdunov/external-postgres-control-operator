//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const (
	r4TestMavenJDBCURL = "https://repo.maven.apache.org/maven2/org/postgresql/postgresql/" +
		r4JDBCVersion + "/postgresql-" + r4JDBCVersion + ".jar"
	r4TestProjectJDBCURL = "https://jdbc.postgresql.org/download/postgresql-" + r4JDBCVersion + ".jar"
)

type r4JDBCTransportFunc func(*http.Request) (*http.Response, error)

func (f r4JDBCTransportFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type r4JDBCResponseBody struct {
	*strings.Reader
	closed bool
}

func (b *r4JDBCResponseBody) Close() error {
	b.closed = true
	return nil
}

func TestR4DownloadJDBC(t *testing.T) {
	const pinnedJAR = "pinned JDBC fixture"
	const corruptJAR = "unexpected artifact"
	const unavailable = "registry error page"
	type response struct {
		url, body string
		status    int
	}
	for _, test := range []struct {
		name      string
		responses []response
		wantError bool
	}{
		{
			name:      "uses Maven when available",
			responses: []response{{r4TestMavenJDBCURL, pinnedJAR, http.StatusOK}},
		},
		{
			name: "falls back after Maven rate limiting",
			responses: []response{
				{r4TestMavenJDBCURL, unavailable, http.StatusTooManyRequests},
				{r4TestProjectJDBCURL, pinnedJAR, http.StatusOK},
			},
		},
		{
			name: "falls back after Maven reports the artifact missing",
			responses: []response{
				{r4TestMavenJDBCURL, unavailable, http.StatusNotFound},
				{r4TestProjectJDBCURL, pinnedJAR, http.StatusOK},
			},
		},
		{
			name:      "rejects Maven checksum mismatch without another request",
			responses: []response{{r4TestMavenJDBCURL, corruptJAR, http.StatusOK}},
			wantError: true,
		},
		{
			name: "rejects fallback checksum mismatch without another request",
			responses: []response{
				{r4TestMavenJDBCURL, unavailable, http.StatusTooManyRequests},
				{r4TestProjectJDBCURL, corruptJAR, http.StatusOK},
			},
			wantError: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var bodies []*r4JDBCResponseBody
			client := &http.Client{Transport: r4JDBCTransportFunc(func(request *http.Request) (*http.Response, error) {
				index := len(bodies)
				if index >= len(test.responses) {
					t.Fatalf("unexpected request to %s", request.URL)
				}
				if index > 0 && !bodies[index-1].closed {
					t.Fatal("response body still open before the next request")
				}
				response := test.responses[index]
				if request.Method != http.MethodGet || request.URL.String() != response.url {
					t.Fatalf("request %d = %s %s, want GET %s", index+1, request.Method, request.URL, response.url)
				}
				body := &r4JDBCResponseBody{Reader: strings.NewReader(response.body)}
				bodies = append(bodies, body)
				return &http.Response{StatusCode: response.status, Body: body}, nil
			})}
			jar, err := r4DownloadJDBC(t.Context(), client, fmt.Sprintf("%x", sha256.Sum256([]byte(pinnedJAR))))
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "checksum") || len(jar) != 0 {
					t.Fatalf("bad artifact was accepted or checksum error was lost: bytes=%d err=%v", len(jar), err)
				}
			} else if err != nil || string(jar) != pinnedJAR {
				t.Fatalf("failed to fetch the pinned artifact: bytes=%q err=%v", jar, err)
			}
			if len(bodies) != len(test.responses) {
				t.Fatalf("made %d requests, want %d", len(bodies), len(test.responses))
			}
			for index, body := range bodies {
				if !body.closed {
					t.Fatalf("response body %d still open", index+1)
				}
			}
		})
	}
}

func TestR4DownloadJDBCCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	requests := 0
	client := &http.Client{Transport: r4JDBCTransportFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		cancel()
		return nil, request.Context().Err()
	})}
	jar, err := r4DownloadJDBC(ctx, client, r4JDBCDigest)
	if !errors.Is(err, context.Canceled) || len(jar) != 0 || requests != 1 {
		t.Fatalf("canceled download returned bytes=%d err=%v requests=%d", len(jar), err, requests)
	}
}
