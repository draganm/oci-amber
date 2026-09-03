package registry

import (
	"bytes"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/draganm/oci-amber/oci"
)

const repo = "library/nested/app"

// startUpload performs the POST that opens a session and returns the
// Location path and the session id.
func (e *testEnv) startUpload(t *testing.T, name string) (location, id string) {
	t.Helper()
	resp := e.do(t, http.MethodPost, "/v2/"+name+"/blobs/uploads/", nil, nil)
	assertStatus(t, resp, http.StatusAccepted)
	assertHeader(t, resp, "Range", "0-0")
	location = resp.Header.Get("Location")
	id = resp.Header.Get("Docker-Upload-UUID")
	if id == "" {
		t.Fatal("POST upload: missing Docker-Upload-UUID")
	}
	if want := "/v2/" + name + "/blobs/uploads/" + id; location != want {
		t.Fatalf("POST upload: Location = %q, want %q", location, want)
	}
	return location, id
}

// assertNoWorkFiles checks that no spilled upload or comp-prysm spool file
// is left under the work directory.
func assertNoWorkFiles(t *testing.T, e *testEnv) {
	t.Helper()
	work := filepath.Dir(e.uploadDir)
	err := filepath.WalkDir(work, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			t.Errorf("leftover file under work dir: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUploadContainerdStyle(t *testing.T) {
	e := newTestEnv(t)
	data := append([]byte("RAW:"), randomBytes(21, 146)...)
	d := oci.DigestOfBytes(data)
	loc, id := e.startUpload(t, repo)

	resp := e.do(t, http.MethodPatch, loc, data[:100], map[string]string{"Content-Range": "0-99", "Content-Type": "application/octet-stream"})
	assertStatus(t, resp, http.StatusAccepted)
	assertHeader(t, resp, "Range", "0-99")
	assertHeader(t, resp, "Docker-Upload-UUID", id)
	assertHeader(t, resp, "Location", loc)

	// Wrong start: 416 carrying the current Range.
	resp = e.do(t, http.MethodPatch, loc, data[100:], map[string]string{"Content-Range": "0-49"})
	assertErrorCode(t, resp, http.StatusRequestedRangeNotSatisfiable, oci.CodeBlobUploadInvalid)
	assertHeader(t, resp, "Range", "0-99")
	assertHeader(t, resp, "Docker-Upload-UUID", id)

	// Malformed Content-Range: 400.
	resp = e.do(t, http.MethodPatch, loc, data[100:], map[string]string{"Content-Range": "hundred-149"})
	assertErrorCode(t, resp, http.StatusBadRequest, oci.CodeBlobUploadInvalid)

	// Neither rejected PATCH consumed anything.
	resp = e.do(t, http.MethodGet, loc, nil, nil)
	assertStatus(t, resp, http.StatusNoContent)
	assertHeader(t, resp, "Range", "0-99")

	resp = e.do(t, http.MethodPatch, loc, data[100:], map[string]string{"Content-Range": "100-149"})
	assertStatus(t, resp, http.StatusAccepted)
	assertHeader(t, resp, "Range", "0-149")

	resp = e.do(t, http.MethodPut, loc+"?digest="+d.String(), nil, nil)
	assertStatus(t, resp, http.StatusCreated)
	assertHeader(t, resp, "Location", "/v2/"+repo+"/blobs/"+d.String())
	assertHeader(t, resp, "Docker-Content-Digest", d.String())

	// The session is gone; the blob is there and byte-identical.
	resp = e.do(t, http.MethodGet, loc, nil, nil)
	assertErrorCode(t, resp, http.StatusNotFound, oci.CodeBlobUploadUnknown)
	resp = e.do(t, http.MethodGet, "/v2/"+repo+"/blobs/"+d.String(), nil, nil)
	assertStatus(t, resp, http.StatusOK)
	if !bytes.Equal(resp.body, data) {
		t.Fatal("pulled bytes differ from pushed bytes")
	}
	assertNoWorkFiles(t, e)
}

func TestUploadPodmanStyle(t *testing.T) {
	e := newTestEnv(t)
	data := rawFixture()
	d := oci.DigestOfBytes(data)
	loc, id := e.startUpload(t, repo)

	resp := e.do(t, http.MethodPatch, loc, data, nil)
	assertStatus(t, resp, http.StatusAccepted)
	assertHeader(t, resp, "Range", "0-"+strconv.Itoa(len(data)-1))
	assertHeader(t, resp, "Docker-Upload-UUID", id)

	resp = e.do(t, http.MethodPut, loc+"?digest="+d.String(), []byte{}, nil)
	assertStatus(t, resp, http.StatusCreated)
	assertHeader(t, resp, "Docker-Content-Digest", d.String())

	resp = e.do(t, http.MethodHead, "/v2/"+repo+"/blobs/"+d.String(), nil, nil)
	assertStatus(t, resp, http.StatusOK)
	assertHeader(t, resp, "Content-Length", strconv.Itoa(len(data)))
}

func TestUploadPutWithBody(t *testing.T) {
	// POST then a single PUT carrying the whole body (containerd's actual
	// sequence), twice: the second hits the whole-blob dedup and still
	// answers 201.
	e := newTestEnv(t)
	data := rawFixture()
	d := oci.DigestOfBytes(data)
	for i := 0; i < 2; i++ {
		loc, _ := e.startUpload(t, repo)
		resp := e.do(t, http.MethodPut, loc+"?digest="+d.String(), data, map[string]string{"Content-Type": "application/octet-stream"})
		assertStatus(t, resp, http.StatusCreated)
		assertHeader(t, resp, "Location", "/v2/"+repo+"/blobs/"+d.String())
		assertHeader(t, resp, "Docker-Content-Digest", d.String())
		resp = e.do(t, http.MethodGet, loc, nil, nil)
		assertErrorCode(t, resp, http.StatusNotFound, oci.CodeBlobUploadUnknown)
	}
	resp := e.do(t, http.MethodGet, "/v2/"+repo+"/blobs/"+d.String(), nil, nil)
	assertStatus(t, resp, http.StatusOK)
	if !bytes.Equal(resp.body, data) {
		t.Fatal("pulled bytes differ from pushed bytes")
	}
}

func TestUploadPutDigestErrors(t *testing.T) {
	e := newTestEnv(t)
	data := rawFixture()
	loc, _ := e.startUpload(t, repo)
	resp := e.do(t, http.MethodPatch, loc, data, nil)
	assertStatus(t, resp, http.StatusAccepted)

	// Missing digest: 400, the session survives untouched.
	resp = e.do(t, http.MethodPut, loc, nil, nil)
	assertErrorCode(t, resp, http.StatusBadRequest, oci.CodeDigestInvalid)
	resp = e.do(t, http.MethodGet, loc, nil, nil)
	assertStatus(t, resp, http.StatusNoContent)
	assertHeader(t, resp, "Range", "0-"+strconv.Itoa(len(data)-1))

	// Malformed or non-sha256 digest: 400, the session survives.
	resp = e.do(t, http.MethodPut, loc+"?digest=sha256:nothex", nil, nil)
	assertErrorCode(t, resp, http.StatusBadRequest, oci.CodeDigestInvalid)
	resp = e.do(t, http.MethodPut, loc+"?digest=sha512:"+fakeHex+fakeHex, nil, nil)
	assertErrorCode(t, resp, http.StatusBadRequest, oci.CodeDigestInvalid)
	resp = e.do(t, http.MethodGet, loc, nil, nil)
	assertStatus(t, resp, http.StatusNoContent)

	// Mismatching digest: 400, the upload is discarded and nothing is stored.
	wrong := oci.DigestOfBytes([]byte("something else"))
	resp = e.do(t, http.MethodPut, loc+"?digest="+wrong.String(), nil, nil)
	assertErrorCode(t, resp, http.StatusBadRequest, oci.CodeDigestInvalid)
	resp = e.do(t, http.MethodGet, loc, nil, nil)
	assertErrorCode(t, resp, http.StatusNotFound, oci.CodeBlobUploadUnknown)
	resp = e.do(t, http.MethodHead, "/v2/"+repo+"/blobs/"+wrong.String(), nil, nil)
	assertStatus(t, resp, http.StatusNotFound)
	resp = e.do(t, http.MethodHead, "/v2/"+repo+"/blobs/"+oci.DigestOfBytes(data).String(), nil, nil)
	assertStatus(t, resp, http.StatusNotFound)
	assertNoWorkFiles(t, e)
}

func TestUploadMonolithicPost(t *testing.T) {
	e := newTestEnv(t)
	data := largeRawFixture()
	d := oci.DigestOfBytes(data)
	base := "/v2/" + repo + "/blobs/uploads/"

	resp := e.do(t, http.MethodPost, base+"?digest="+d.String(), data, map[string]string{"Content-Type": "application/octet-stream"})
	assertStatus(t, resp, http.StatusCreated)
	assertHeader(t, resp, "Location", "/v2/"+repo+"/blobs/"+d.String())
	assertHeader(t, resp, "Docker-Content-Digest", d.String())

	resp = e.do(t, http.MethodGet, "/v2/"+repo+"/blobs/"+d.String(), nil, nil)
	assertStatus(t, resp, http.StatusOK)
	if !bytes.Equal(resp.body, data) {
		t.Fatal("pulled bytes differ from pushed bytes")
	}

	// The empty blob.
	empty := oci.DigestOfBytes(nil)
	resp = e.do(t, http.MethodPost, base+"?digest="+empty.String(), []byte{}, nil)
	assertStatus(t, resp, http.StatusCreated)
	resp = e.do(t, http.MethodGet, "/v2/"+repo+"/blobs/"+empty.String(), nil, nil)
	assertStatus(t, resp, http.StatusOK)
	assertHeader(t, resp, "Content-Length", "0")

	// Malformed and mismatching digests.
	resp = e.do(t, http.MethodPost, base+"?digest=sha256:nope", data, nil)
	assertErrorCode(t, resp, http.StatusBadRequest, oci.CodeDigestInvalid)
	wrong := oci.DigestOfBytes([]byte("other"))
	resp = e.do(t, http.MethodPost, base+"?digest="+wrong.String(), data, nil)
	assertErrorCode(t, resp, http.StatusBadRequest, oci.CodeDigestInvalid)
	resp = e.do(t, http.MethodHead, "/v2/"+repo+"/blobs/"+wrong.String(), nil, nil)
	assertStatus(t, resp, http.StatusNotFound)

	assertNoWorkFiles(t, e)
}

func TestUploadSpillsToDisk(t *testing.T) {
	// 3 MiB through a 1 MiB in-memory limit: the session spills to
	// <work>/uploads, the finalize reads it back, and nothing is left behind.
	e := newTestEnv(t)
	data := append([]byte("RAW:"), randomBytes(31, 3<<20)...)
	d := oci.DigestOfBytes(data)
	loc, _ := e.startUpload(t, repo)
	half := len(data) / 2

	resp := e.do(t, http.MethodPatch, loc, data[:half], map[string]string{"Content-Range": "0-" + strconv.Itoa(half-1)})
	assertStatus(t, resp, http.StatusAccepted)
	entries, err := os.ReadDir(e.uploadDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("after exceeding max-in-memory: %d files in the upload dir, want 1", len(entries))
	}

	resp = e.do(t, http.MethodPatch, loc, data[half:], map[string]string{"Content-Range": strconv.Itoa(half) + "-" + strconv.Itoa(len(data)-1)})
	assertStatus(t, resp, http.StatusAccepted)
	assertHeader(t, resp, "Range", "0-"+strconv.Itoa(len(data)-1))

	resp = e.do(t, http.MethodPut, loc+"?digest="+d.String(), nil, nil)
	assertStatus(t, resp, http.StatusCreated)

	resp = e.do(t, http.MethodGet, "/v2/"+repo+"/blobs/"+d.String(), nil, nil)
	assertStatus(t, resp, http.StatusOK)
	if !bytes.Equal(resp.body, data) {
		t.Fatal("pulled bytes differ from pushed bytes")
	}
	assertNoWorkFiles(t, e)
}

func TestUploadStatusAndCancel(t *testing.T) {
	e := newTestEnv(t)
	loc, id := e.startUpload(t, repo)

	resp := e.do(t, http.MethodGet, loc, nil, nil)
	assertStatus(t, resp, http.StatusNoContent)
	assertHeader(t, resp, "Range", "0-0")
	assertHeader(t, resp, "Docker-Upload-UUID", id)
	assertHeader(t, resp, "Location", loc)

	resp = e.do(t, http.MethodPatch, loc, []byte("0123456789"), nil)
	assertStatus(t, resp, http.StatusAccepted)
	resp = e.do(t, http.MethodGet, loc, nil, nil)
	assertStatus(t, resp, http.StatusNoContent)
	assertHeader(t, resp, "Range", "0-9")

	resp = e.do(t, http.MethodDelete, loc, nil, nil)
	assertStatus(t, resp, http.StatusNoContent)

	for _, m := range []string{http.MethodGet, http.MethodPatch, http.MethodPut, http.MethodDelete} {
		resp = e.do(t, m, loc+"?digest=sha256:"+fakeHex, []byte("x"), nil)
		assertErrorCode(t, resp, http.StatusNotFound, oci.CodeBlobUploadUnknown)
	}
	resp = e.do(t, http.MethodGet, "/v2/"+repo+"/blobs/uploads/deadbeef", nil, nil)
	assertErrorCode(t, resp, http.StatusNotFound, oci.CodeBlobUploadUnknown)
	assertNoWorkFiles(t, e)
}

func TestUploadMount(t *testing.T) {
	e := newTestEnv(t)
	d := e.putBlob(t, rawFixture())

	resp := e.do(t, http.MethodPost, "/v2/other/repo/blobs/uploads/?mount="+d.String()+"&from="+repo, nil, nil)
	assertStatus(t, resp, http.StatusCreated)
	assertHeader(t, resp, "Location", "/v2/other/repo/blobs/"+d.String())
	assertHeader(t, resp, "Docker-Content-Digest", d.String())

	// Miss: a normal session is opened instead.
	resp = e.do(t, http.MethodPost, "/v2/other/repo/blobs/uploads/?mount=sha256:"+fakeHex+"&from="+repo, nil, nil)
	assertStatus(t, resp, http.StatusAccepted)
	assertHeader(t, resp, "Range", "0-0")
	if resp.Header.Get("Docker-Upload-UUID") == "" || !strings.HasPrefix(resp.Header.Get("Location"), "/v2/other/repo/blobs/uploads/") {
		t.Fatalf("mount miss: Location %q, UUID %q", resp.Header.Get("Location"), resp.Header.Get("Docker-Upload-UUID"))
	}

	// A malformed mount digest is a miss too.
	resp = e.do(t, http.MethodPost, "/v2/other/repo/blobs/uploads/?mount=notadigest", nil, nil)
	assertStatus(t, resp, http.StatusAccepted)
}

func TestUploadMethodNotAllowed(t *testing.T) {
	e := newTestEnv(t)
	resp := e.do(t, http.MethodGet, "/v2/"+repo+"/blobs/uploads/", nil, nil)
	assertErrorCode(t, resp, http.StatusMethodNotAllowed, oci.CodeUnsupported)
	assertHeader(t, resp, "Allow", "POST")

	loc, _ := e.startUpload(t, repo)
	resp = e.do(t, http.MethodPost, loc, nil, nil)
	assertErrorCode(t, resp, http.StatusMethodNotAllowed, oci.CodeUnsupported)
	assertHeader(t, resp, "Allow", "PATCH, PUT, GET, DELETE")
}

func TestParseContentRange(t *testing.T) {
	cases := []struct {
		h          string
		start, end int64
		ok         bool
	}{
		{"0-99", 0, 99, true},
		{"100-149", 100, 149, true},
		{"bytes 0-99/*", 0, 99, true},
		{"bytes=0-99", 0, 99, true},
		{"0-0", 0, 0, true},
		{"5-2", 0, 0, false},
		{"x-1", 0, 0, false},
		{"0", 0, 0, false},
		{"-5", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		start, end, err := parseContentRange(c.h)
		if (err == nil) != c.ok || start != c.start || end != c.end {
			t.Errorf("parseContentRange(%q) = %d, %d, %v; want %d, %d, ok=%v", c.h, start, end, err, c.start, c.end, c.ok)
		}
	}
}

func TestUploadRangeHeader(t *testing.T) {
	for offset, want := range map[int64]string{0: "0-0", 1: "0-0", 2: "0-1", 150: "0-149"} {
		if got := uploadRange(offset); got != want {
			t.Errorf("uploadRange(%d) = %q, want %q", offset, got, want)
		}
	}
}
