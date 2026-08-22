package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDecodeJSONOrFailMapsMalformedJSONTo400 covers the existing "invalid JSON
// body" branch (unknown field, syntax error, etc.) through the new helper.
func TestDecodeJSONOrFailMapsMalformedJSONTo400(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{not-json`))
	var v map[string]any
	if decodeJSONOrFail(rec, req, &v) {
		t.Fatal("want ok=false for malformed JSON")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body)
	}
}

// TestDecodeJSONOrFailMapsOversizedBodyTo413 proves a body rejected by
// http.MaxBytesReader (the maxBytesMiddleware wraps every mutating route with
// one — see TestMaxBytesMiddlewareCapsPostAndPutBodies) maps to 413, not the
// generic 400 "invalid JSON body" (audit B3).
func TestDecodeJSONOrFailMapsOversizedBodyTo413(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"name":"`+strings.Repeat("x", 100)+`"}`))
	req.Body = http.MaxBytesReader(rec, req.Body, 10) // tiny cap forces the error deterministically
	var v map[string]any
	if decodeJSONOrFail(rec, req, &v) {
		t.Fatal("want ok=false for oversized body")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413, body = %s", rec.Code, rec.Body)
	}
}

// TestDecodeJSONRejectsTrailingGarbage proves decodeJSON rejects data after the
// single JSON value (dec.More()) — a body like `{...} junk` or `{...}{...}` is a
// 400, not a silently-accepted first object (audit B6).
func TestDecodeJSONRejectsTrailingGarbage(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"name":"ok"} garbage`))
	var v struct {
		Name string `json:"name"`
	}
	if decodeJSONOrFail(rec, req, &v) {
		t.Fatal("want ok=false for trailing data after the JSON value")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body)
	}
}

// TestDecodeJSONOrFailOKOnValidBody proves the success path still decodes and
// returns true (the pre-existing behavior every handler relies on).
func TestDecodeJSONOrFailOKOnValidBody(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"name":"ok"}`))
	var v struct {
		Name string `json:"name"`
	}
	if !decodeJSONOrFail(rec, req, &v) {
		t.Fatalf("want ok=true, body = %s", rec.Body)
	}
	if v.Name != "ok" {
		t.Fatalf("Name = %q, want ok", v.Name)
	}
}
