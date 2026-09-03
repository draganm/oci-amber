package oci

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestErrorCodeDefaultStatus(t *testing.T) {
	cases := []struct {
		code ErrorCode
		want int
	}{
		{CodeBlobUnknown, http.StatusNotFound},
		{CodeBlobUploadInvalid, http.StatusBadRequest},
		{CodeBlobUploadUnknown, http.StatusNotFound},
		{CodeDigestInvalid, http.StatusBadRequest},
		{CodeManifestBlobUnknown, http.StatusNotFound},
		{CodeManifestInvalid, http.StatusBadRequest},
		{CodeManifestUnknown, http.StatusNotFound},
		{CodeNameInvalid, http.StatusBadRequest},
		{CodeNameUnknown, http.StatusNotFound},
		{CodeSizeInvalid, http.StatusBadRequest},
		{CodeUnauthorized, http.StatusUnauthorized},
		{CodeDenied, http.StatusForbidden},
		{CodeUnsupported, http.StatusMethodNotAllowed},
		{CodeTooManyRequests, http.StatusTooManyRequests},
		{ErrorCode("BOGUS"), http.StatusInternalServerError},
		{ErrorCode(""), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(string(tc.code), func(t *testing.T) {
			if got := tc.code.DefaultStatus(); got != tc.want {
				t.Fatalf("%s.DefaultStatus() = %d, want %d", tc.code, got, tc.want)
			}
		})
	}
}

func TestErrorCodeValues(t *testing.T) {
	// The wire values are fixed by the distribution spec.
	want := map[ErrorCode]string{
		CodeBlobUnknown:         "BLOB_UNKNOWN",
		CodeBlobUploadInvalid:   "BLOB_UPLOAD_INVALID",
		CodeBlobUploadUnknown:   "BLOB_UPLOAD_UNKNOWN",
		CodeDigestInvalid:       "DIGEST_INVALID",
		CodeManifestBlobUnknown: "MANIFEST_BLOB_UNKNOWN",
		CodeManifestInvalid:     "MANIFEST_INVALID",
		CodeManifestUnknown:     "MANIFEST_UNKNOWN",
		CodeNameInvalid:         "NAME_INVALID",
		CodeNameUnknown:         "NAME_UNKNOWN",
		CodeSizeInvalid:         "SIZE_INVALID",
		CodeUnauthorized:        "UNAUTHORIZED",
		CodeDenied:              "DENIED",
		CodeUnsupported:         "UNSUPPORTED",
		CodeTooManyRequests:     "TOOMANYREQUESTS",
	}
	if len(want) != 14 {
		t.Fatalf("expected 14 standard codes, table has %d", len(want))
	}
	for code, s := range want {
		if string(code) != s {
			t.Errorf("code %q != %q", code, s)
		}
	}
}

func TestErrorMessageAndDetail(t *testing.T) {
	e := NewError(CodeManifestBlobUnknown, "blob %s unknown", "sha256:abc")
	if got, want := e.Error(), "MANIFEST_BLOB_UNKNOWN: blob sha256:abc unknown"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if got, want := (&Error{Code: CodeDenied}).Error(), "DENIED"; got != want {
		t.Fatalf("Error() without message = %q, want %q", got, want)
	}
	if e.Detail != nil {
		t.Fatalf("NewError set Detail = %v, want nil", e.Detail)
	}

	d := e.WithDetail(map[string]string{"digest": "sha256:abc"})
	if d == e {
		t.Fatal("WithDetail must return a copy")
	}
	if e.Detail != nil {
		t.Fatalf("WithDetail modified the receiver: %v", e.Detail)
	}
	if d.Code != e.Code || d.Message != e.Message {
		t.Fatalf("WithDetail lost code/message: %+v", d)
	}
	if got := d.Detail.(map[string]string)["digest"]; got != "sha256:abc" {
		t.Fatalf("Detail = %v", d.Detail)
	}

	wrapped := fmt.Errorf("put manifest: %w", e)
	var oe *Error
	if !errors.As(wrapped, &oe) {
		t.Fatalf("errors.As failed on %v", wrapped)
	}
	if oe.Code != CodeManifestBlobUnknown {
		t.Fatalf("unwrapped code = %s", oe.Code)
	}
}

func TestErrorResponseJSON(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"empty value", ErrorResponse{}, `{"errors":[]}`},
		{"empty pointer", &ErrorResponse{}, `{"errors":[]}`},
		{"explicit empty slice", ErrorResponse{Errors: []Error{}}, `{"errors":[]}`},
		{"code only", ErrorResponse{Errors: []Error{{Code: CodeBlobUnknown}}},
			`{"errors":[{"code":"BLOB_UNKNOWN"}]}`},
		{"message and detail", ErrorResponse{Errors: []Error{
			*NewError(CodeManifestBlobUnknown, "missing").WithDetail(map[string]string{"digest": "sha256:abc"}),
		}}, `{"errors":[{"code":"MANIFEST_BLOB_UNKNOWN","message":"missing","detail":{"digest":"sha256:abc"}}]}`},
		{"two errors", ErrorResponse{Errors: []Error{
			{Code: CodeNameInvalid, Message: "bad name"},
			{Code: CodeDigestInvalid, Message: "bad digest"},
		}}, `{"errors":[{"code":"NAME_INVALID","message":"bad name"},{"code":"DIGEST_INVALID","message":"bad digest"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(b) != tc.want {
				t.Fatalf("Marshal = %s, want %s", b, tc.want)
			}
		})
	}

	// Decoding the envelope (as a client would) round-trips.
	var resp ErrorResponse
	body := `{"errors":[{"code":"MANIFEST_BLOB_UNKNOWN","message":"missing","detail":{"digest":"sha256:abc"}}]}`
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(resp.Errors) != 1 || resp.Errors[0].Code != CodeManifestBlobUnknown || resp.Errors[0].Message != "missing" {
		t.Fatalf("decoded %+v", resp)
	}
	if resp.Errors[0].Detail.(map[string]any)["digest"] != "sha256:abc" {
		t.Fatalf("decoded detail %v", resp.Errors[0].Detail)
	}
}
