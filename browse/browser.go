package browse

import (
	"fmt"

	"github.com/draganm/oci-amber/blob"
	"github.com/draganm/oci-amber/image"
	"github.com/draganm/oci-amber/oci"
	"github.com/draganm/oci-amber/store"
	"github.com/draganm/oci-amber/tui"
)

// Browser builds nodes over an open store. It only reads.
type Browser struct {
	st     *store.Store
	blobs  *blob.Store
	images *image.Store
}

// New returns a Browser over st. blobs and images must be stores over the
// same st; blob.NewReadOnly is enough for blobs.
func New(st *store.Store, blobs *blob.Store, images *image.Store) *Browser {
	return &Browser{st: st, blobs: blobs, images: images}
}

// shortRef abbreviates a digest for rows and crumbs: "sha256:4f7c9a1e".
func shortRef(d oci.Digest) string { return d.Algorithm() + ":" + tui.ShortDigest(d) }

// plural renders n with word: "1 tag", "1,234 files".
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%s %ss", tui.FormatCount(int64(n)), word)
}
