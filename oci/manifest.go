package oci

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Media types the registry has to recognise. Anything else is stored and
// served verbatim.
const (
	MediaTypeOCIManifest        = "application/vnd.oci.image.manifest.v1+json"
	MediaTypeOCIIndex           = "application/vnd.oci.image.index.v1+json"
	MediaTypeDockerManifest     = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaTypeOCIConfig          = "application/vnd.oci.image.config.v1+json"
	MediaTypeOctetStream        = "application/octet-stream"
)

// Descriptor is an OCI content descriptor restricted to the fields the
// registry needs: it is what ParseManifest collects from config, layers,
// manifests and subject, and what the referrers API returns. Fields such as
// platform, urls and data are ignored on input and never produced.
type Descriptor struct {
	MediaType    string            `json:"mediaType"`
	Digest       Digest            `json:"digest"`
	Size         int64             `json:"size"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

// Manifest is the parsed shape shared by image manifests (config, layers)
// and image indexes / manifest lists (manifests). It exists only for
// validation and for collecting descriptors; the registry stores the raw
// body, never a re-encoding of this struct.
type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	ArtifactType  string            `json:"artifactType,omitempty"`
	Config        *Descriptor       `json:"config,omitempty"`
	Layers        []Descriptor      `json:"layers,omitempty"`
	Manifests     []Descriptor      `json:"manifests,omitempty"`
	Subject       *Descriptor       `json:"subject,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

// ParseManifest decodes and validates a manifest or index body. It fails
// with an *Error carrying CodeManifestInvalid when the body is not valid
// JSON, schemaVersion is not 2, any descriptor (config, layers, manifests,
// subject) has a digest that ParseDigest rejects, or any descriptor has a
// negative size. Unknown fields are ignored.
func ParseManifest(body []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, NewError(CodeManifestInvalid, "manifest is not valid JSON: %v", err)
	}
	if m.SchemaVersion != 2 {
		return nil, NewError(CodeManifestInvalid, "unsupported schemaVersion %d, want 2", m.SchemaVersion)
	}
	if m.Config != nil {
		if err := validateDescriptor("config", m.Config); err != nil {
			return nil, err
		}
	}
	for i := range m.Layers {
		if err := validateDescriptor(fmt.Sprintf("layers[%d]", i), &m.Layers[i]); err != nil {
			return nil, err
		}
	}
	for i := range m.Manifests {
		if err := validateDescriptor(fmt.Sprintf("manifests[%d]", i), &m.Manifests[i]); err != nil {
			return nil, err
		}
	}
	if m.Subject != nil {
		if err := validateDescriptor("subject", m.Subject); err != nil {
			return nil, err
		}
	}
	return &m, nil
}

// validateDescriptor checks the digest and size of one descriptor; field
// names the descriptor in the error message.
func validateDescriptor(field string, d *Descriptor) error {
	if _, err := ParseDigest(string(d.Digest)); err != nil {
		return NewError(CodeManifestInvalid, "%s: %v", field, err)
	}
	if d.Size < 0 {
		return NewError(CodeManifestInvalid, "%s: negative size %d", field, d.Size)
	}
	return nil
}

// IsIndex reports whether the manifest is an image index or manifest list:
// its mediaType is one of the index types, or it has no mediaType and a
// manifests array (even an empty one).
func (m *Manifest) IsIndex() bool {
	switch m.MediaType {
	case MediaTypeOCIIndex, MediaTypeDockerManifestList:
		return true
	case "":
		return m.Manifests != nil
	}
	return false
}

// EffectiveMediaType returns the media type to store and serve for the
// manifest: the request Content-Type with any parameters removed when the
// client sent one, else the manifest's own mediaType field, else the OCI
// index or manifest type according to IsIndex.
func (m *Manifest) EffectiveMediaType(contentType string) string {
	if ct := stripMediaTypeParams(contentType); ct != "" {
		return ct
	}
	if m.MediaType != "" {
		return m.MediaType
	}
	if m.IsIndex() {
		return MediaTypeOCIIndex
	}
	return MediaTypeOCIManifest
}

// stripMediaTypeParams returns the media type part of a Content-Type value
// (everything before the first ';'), trimmed, exactly as the client spelled
// it. Media type tokens can never contain ';', so no MIME parsing is needed.
func stripMediaTypeParams(contentType string) string {
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	return strings.TrimSpace(contentType)
}

// EffectiveArtifactType returns the manifest's artifactType, falling back
// to config.mediaType for image manifests (the pre-artifactType convention
// clients such as ggcr and oras still apply), and "" otherwise.
func (m *Manifest) EffectiveArtifactType() string {
	if m.ArtifactType != "" {
		return m.ArtifactType
	}
	if !m.IsIndex() && m.Config != nil {
		return m.Config.MediaType
	}
	return ""
}

// BlobDescriptors returns the descriptors of every blob an image manifest
// needs: the config (when present) followed by the layers in order. It is
// nil for an index, whose children are manifests, not blobs.
func (m *Manifest) BlobDescriptors() []Descriptor {
	if m.IsIndex() {
		return nil
	}
	out := make([]Descriptor, 0, len(m.Layers)+1)
	if m.Config != nil {
		out = append(out, *m.Config)
	}
	return append(out, m.Layers...)
}
