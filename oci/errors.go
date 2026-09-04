package oci

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// ErrorCode is one of the standard OCI distribution error codes.
type ErrorCode string

const (
	CodeBlobUnknown         ErrorCode = "BLOB_UNKNOWN"
	CodeBlobUploadInvalid   ErrorCode = "BLOB_UPLOAD_INVALID"
	CodeBlobUploadUnknown   ErrorCode = "BLOB_UPLOAD_UNKNOWN"
	CodeDigestInvalid       ErrorCode = "DIGEST_INVALID"
	CodeManifestBlobUnknown ErrorCode = "MANIFEST_BLOB_UNKNOWN"
	CodeManifestInvalid     ErrorCode = "MANIFEST_INVALID"
	CodeManifestUnknown     ErrorCode = "MANIFEST_UNKNOWN"
	CodeNameInvalid         ErrorCode = "NAME_INVALID"
	CodeNameUnknown         ErrorCode = "NAME_UNKNOWN"
	CodeSizeInvalid         ErrorCode = "SIZE_INVALID"
	CodeUnauthorized        ErrorCode = "UNAUTHORIZED"
	CodeDenied              ErrorCode = "DENIED"
	CodeUnsupported         ErrorCode = "UNSUPPORTED"
	CodeTooManyRequests     ErrorCode = "TOOMANYREQUESTS"

	// oci-amber extensions for the rootfs API under /fs/; they are not in
	// the distribution spec's list.
	CodeRootfsUnavailable ErrorCode = "ROOTFS_UNAVAILABLE" // the image has no rootfs tree
	CodePathUnknown       ErrorCode = "PATH_UNKNOWN"       // a path component is missing or not a directory
	CodePathInvalid       ErrorCode = "PATH_INVALID"       // symlink loop, format=tar off a directory, unknown format
	CodePlatformUnknown   ErrorCode = "PLATFORM_UNKNOWN"   // an index without a usable platform
)

// DefaultStatus returns the HTTP status the registry answers with for an
// error of this code. Codes outside the standard set map to 500.
func (c ErrorCode) DefaultStatus() int {
	switch c {
	case CodeBlobUnknown, CodeBlobUploadUnknown, CodeManifestBlobUnknown,
		CodeManifestUnknown, CodeNameUnknown, CodeRootfsUnavailable, CodePathUnknown:
		return http.StatusNotFound
	case CodeBlobUploadInvalid, CodeDigestInvalid, CodeManifestInvalid,
		CodeNameInvalid, CodeSizeInvalid, CodePathInvalid, CodePlatformUnknown:
		return http.StatusBadRequest
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeDenied:
		return http.StatusForbidden
	case CodeUnsupported:
		return http.StatusMethodNotAllowed
	case CodeTooManyRequests:
		return http.StatusTooManyRequests
	}
	return http.StatusInternalServerError
}

// Error is one entry of the OCI error envelope. Every *Error returned by an
// oci-amber package is a client fault that the HTTP layer renders verbatim
// with the status from Code.DefaultStatus(); any other error is internal.
type Error struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message,omitempty"`
	Detail  any       `json:"detail,omitempty"`
}

// Error implements the error interface as "<code>: <message>", or just the
// code when the message is empty.
func (e *Error) Error() string {
	if e.Message == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Message
}

// NewError builds an *Error with a formatted message and no detail.
func NewError(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// WithDetail returns a copy of e carrying detail, arbitrary JSON data the
// client can use to resolve the problem (for example {"digest": "sha256:…"}
// on MANIFEST_BLOB_UNKNOWN). The receiver is not modified.
func (e *Error) WithDetail(detail any) *Error {
	c := *e
	c.Detail = detail
	return &c
}

// ErrorResponse is the JSON body of every error reply:
// {"errors":[{"code":…,"message":…,"detail":…}]}.
type ErrorResponse struct {
	Errors []Error `json:"errors"`
}

// MarshalJSON renders a nil Errors slice as [] rather than null, so the
// envelope always contains an array (the recovered-panic body is
// {"errors":[]}).
func (r ErrorResponse) MarshalJSON() ([]byte, error) {
	errs := r.Errors
	if errs == nil {
		errs = []Error{}
	}
	return json.Marshal(struct {
		Errors []Error `json:"errors"`
	}{Errors: errs})
}
