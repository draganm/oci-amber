package registry

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/draganm/oci-amber/blob"
	"github.com/draganm/oci-amber/image"
	"github.com/draganm/oci-amber/oci"
	"github.com/draganm/oci-amber/upload"
)

// writeJSON writes v as the JSON body of a response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		body = []byte(`{"errors":[]}`)
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	w.Write(body)
}

// writeError writes the standard error envelope with the code's default
// HTTP status.
func writeError(w http.ResponseWriter, err *oci.Error) {
	writeErrorStatus(w, err.Code.DefaultStatus(), err)
}

// writeErrorStatus writes the standard error envelope with an explicit
// status, for the cases where the spec departs from the code's default
// (416 for an upload Content-Range mismatch).
func writeErrorStatus(w http.ResponseWriter, status int, err *oci.Error) {
	writeJSON(w, status, oci.ErrorResponse{Errors: []oci.Error{*err}})
}

// writeEmptyErrors writes {"errors":[]} with the given status: unknown
// paths under /v2/, recovered panics and internal failures.
func writeEmptyErrors(w http.ResponseWriter, status int) {
	writeJSON(w, status, oci.ErrorResponse{Errors: []oci.Error{}})
}

// notFound is the answer for everything under /v2/ that is not a route.
func (s *server) notFound(w http.ResponseWriter) {
	writeEmptyErrors(w, http.StatusNotFound)
}

// methodNotAllowed answers 405 with the UNSUPPORTED code and an Allow header.
func methodNotAllowed(w http.ResponseWriter, r *http.Request, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	writeError(w, oci.NewError(oci.CodeUnsupported, "method %s is not supported for %s", r.Method, r.URL.Path))
}

// handleError maps an error from the stores to a response: *oci.Error
// carries its own code; the stores' not-found sentinels become the matching
// 404 codes; anything else is an internal failure, logged and answered with
// a 500 and an empty errors list.
func (s *server) handleError(w http.ResponseWriter, r *http.Request, err error) {
	var oerr *oci.Error
	switch {
	case errors.As(err, &oerr):
		writeError(w, oerr)
	case errors.Is(err, blob.ErrNotFound):
		writeError(w, oci.NewError(oci.CodeBlobUnknown, "blob unknown to registry"))
	case errors.Is(err, image.ErrNotFound):
		writeError(w, oci.NewError(oci.CodeManifestUnknown, "manifest unknown to registry"))
	case errors.Is(err, upload.ErrUnknown):
		writeError(w, oci.NewError(oci.CodeBlobUploadUnknown, "blob upload unknown to registry"))
	default:
		level := slog.LevelError
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			level = slog.LevelDebug
		}
		s.log.Log(r.Context(), level, "request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		writeEmptyErrors(w, http.StatusInternalServerError)
	}
}
