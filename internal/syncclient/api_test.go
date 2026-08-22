package syncclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchArtifacts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sync/artifacts" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer AT" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artifacts":[
			{"type":"skill","name":"fmt","content":"---\nname: fmt\n---\nx"},
			{"type":"subagent","name":"rev","content":"---\nname: rev\nmemory: project\n---\ny"}
		]}`))
	}))
	defer srv.Close()

	arts, err := FetchArtifacts(context.Background(), srv.Client(), srv.URL, "AT")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(arts) != 2 || arts[0].Name != "fmt" || arts[1].Type != "subagent" {
		t.Fatalf("bad artifacts: %+v", arts)
	}
}

func TestFetchArtifactsUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	if _, err := FetchArtifacts(context.Background(), srv.Client(), srv.URL, "bad"); err == nil {
		t.Fatal("expected error on 401")
	}
}

func TestFetchArtifactsDecodesSeedFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"artifacts":[
			{"type":"subagent","name":"seeded","content":"c","memoryScope":"project","memorySeed":"seed body"},
			{"type":"skill","name":"plain","content":"c"}]}`))
	}))
	defer srv.Close()

	arts, err := FetchArtifacts(context.Background(), srv.Client(), srv.URL, "tok")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(arts) != 2 {
		t.Fatalf("want 2 artifacts, got %d", len(arts))
	}
	if arts[0].MemoryScope != "project" || arts[0].MemorySeed != "seed body" {
		t.Fatalf("seed fields not decoded: %+v", arts[0])
	}
	if arts[1].MemoryScope != "" || arts[1].MemorySeed != "" {
		t.Fatalf("absent fields must decode to empty: %+v", arts[1])
	}
}

// S8c: a 401/403 means the server rejected the token — the error must tell the
// user the one command that fixes it. Any other non-200 must NOT carry the hint
// (a 500 is not a credential problem; sending the user to re-login would be a lie).
func TestFetchArtifactsTokenRejectedHint(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()
			_, err := FetchArtifacts(context.Background(), srv.Client(), srv.URL, "bad")
			if err == nil || !strings.Contains(err.Error(), "token rejected — run 'orbeat-sync login'") {
				t.Fatalf("status %d error must carry the re-login hint, got: %v", code, err)
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := FetchArtifacts(context.Background(), srv.Client(), srv.URL, "tok")
	if err == nil || strings.Contains(err.Error(), "token rejected") {
		t.Fatalf("a 500 must not carry the re-login hint, got: %v", err)
	}
}

func TestFetchGatewayURLTokenRejectedHint(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()
			_, err := FetchGatewayURL(context.Background(), srv.Client(), srv.URL, "bad")
			if err == nil || !strings.Contains(err.Error(), "token rejected — run 'orbeat-sync login'") {
				t.Fatalf("status %d error must carry the re-login hint, got: %v", code, err)
			}
		})
	}
}

func TestFetchGatewayURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sync/config" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer AT" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"gateway_url":"https://gw"}`))
	}))
	defer srv.Close()

	gw, err := FetchGatewayURL(context.Background(), srv.Client(), srv.URL, "AT")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if gw != "https://gw" {
		t.Fatalf("gateway url = %q, want https://gw", gw)
	}
}

func TestFetchGatewayURLNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	if _, err := FetchGatewayURL(context.Background(), srv.Client(), srv.URL, "bad"); err == nil {
		t.Fatal("expected error on 401")
	}
}

func TestFetchGatewayURLEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"gateway_url":""}`))
	}))
	defer srv.Close()
	if _, err := FetchGatewayURL(context.Background(), srv.Client(), srv.URL, "tok"); err == nil {
		t.Fatal("expected error on empty gateway_url")
	}
}
