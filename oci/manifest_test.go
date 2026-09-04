package oci

import (
	"errors"
	"strings"
	"testing"
)

// Distinct well-formed digests for the manifest fixtures.
var (
	dCfg = Digest("sha256:" + strings.Repeat("0", 63) + "1")
	dL1  = Digest("sha256:" + strings.Repeat("0", 63) + "2")
	dL2  = Digest("sha256:" + strings.Repeat("0", 63) + "3")
	dSub = Digest("sha256:" + strings.Repeat("0", 63) + "4")
	dM1  = Digest("sha256:" + strings.Repeat("0", 63) + "5")
	dM2  = Digest("sha256:" + strings.Repeat("0", 63) + "6")
)

const ociManifestBody = `{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "config": {
    "mediaType": "application/vnd.oci.image.config.v1+json",
    "digest": "sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "size": 7023
  },
  "layers": [
    {
      "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
      "digest": "sha256:0000000000000000000000000000000000000000000000000000000000000002",
      "size": 32654,
      "urls": ["https://example.com/l1"]
    },
    {
      "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
      "digest": "sha256:0000000000000000000000000000000000000000000000000000000000000003",
      "size": 0,
      "annotations": {"org.opencontainers.image.title": "empty"}
    }
  ],
  "subject": {
    "mediaType": "application/vnd.oci.image.manifest.v1+json",
    "digest": "sha256:0000000000000000000000000000000000000000000000000000000000000004",
    "size": 7682
  },
  "annotations": {"com.example.key1": "value1"}
}`

const dockerManifestBody = `{
  "schemaVersion": 2,
  "mediaType": "application/vnd.docker.distribution.manifest.v2+json",
  "config": {
    "mediaType": "application/vnd.docker.container.image.v1+json",
    "digest": "sha256:0000000000000000000000000000000000000000000000000000000000000001",
    "size": 1469
  },
  "layers": [
    {
      "mediaType": "application/vnd.docker.image.rootfs.diff.tar.gzip",
      "digest": "sha256:0000000000000000000000000000000000000000000000000000000000000002",
      "size": 2818413
    }
  ]
}`

const ociIndexBody = `{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.index.v1+json",
  "manifests": [
    {
      "mediaType": "application/vnd.oci.image.manifest.v1+json",
      "digest": "sha256:0000000000000000000000000000000000000000000000000000000000000005",
      "size": 7143,
      "platform": {"architecture": "ppc64le", "os": "linux"}
    },
    {
      "mediaType": "application/vnd.oci.image.manifest.v1+json",
      "digest": "sha256:0000000000000000000000000000000000000000000000000000000000000006",
      "size": 7682,
      "platform": {"architecture": "amd64", "os": "linux"}
    }
  ],
  "annotations": {"com.example.key1": "value1"}
}`

const untypedIndexBody = `{
  "schemaVersion": 2,
  "manifests": [
    {
      "mediaType": "application/vnd.oci.image.manifest.v1+json",
      "digest": "sha256:0000000000000000000000000000000000000000000000000000000000000005",
      "size": 7143
    }
  ]
}`

func mustParse(t *testing.T, body string) *Manifest {
	t.Helper()
	m, err := ParseManifest([]byte(body))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	return m
}

func TestParseManifestOCI(t *testing.T) {
	m := mustParse(t, ociManifestBody)
	if m.SchemaVersion != 2 {
		t.Errorf("SchemaVersion = %d", m.SchemaVersion)
	}
	if m.MediaType != MediaTypeOCIManifest {
		t.Errorf("MediaType = %q", m.MediaType)
	}
	if m.IsIndex() {
		t.Error("IsIndex() = true for an image manifest")
	}
	if m.Config == nil || m.Config.Digest != dCfg || m.Config.Size != 7023 || m.Config.MediaType != MediaTypeOCIConfig {
		t.Errorf("Config = %+v", m.Config)
	}
	if len(m.Layers) != 2 || m.Layers[0].Digest != dL1 || m.Layers[0].Size != 32654 || m.Layers[1].Digest != dL2 || m.Layers[1].Size != 0 {
		t.Errorf("Layers = %+v", m.Layers)
	}
	if m.Layers[1].Annotations["org.opencontainers.image.title"] != "empty" {
		t.Errorf("layer annotations = %v", m.Layers[1].Annotations)
	}
	if m.Manifests != nil {
		t.Errorf("Manifests = %+v, want nil", m.Manifests)
	}
	if m.Subject == nil || m.Subject.Digest != dSub || m.Subject.Size != 7682 || m.Subject.MediaType != MediaTypeOCIManifest {
		t.Errorf("Subject = %+v", m.Subject)
	}
	if m.Annotations["com.example.key1"] != "value1" {
		t.Errorf("Annotations = %v", m.Annotations)
	}
	if m.ArtifactType != "" {
		t.Errorf("ArtifactType = %q", m.ArtifactType)
	}
}

func TestParseManifestDocker(t *testing.T) {
	m := mustParse(t, dockerManifestBody)
	if m.MediaType != MediaTypeDockerManifest {
		t.Errorf("MediaType = %q", m.MediaType)
	}
	if m.IsIndex() {
		t.Error("IsIndex() = true for a docker manifest")
	}
	if m.Config == nil || m.Config.Digest != dCfg || m.Config.MediaType != "application/vnd.docker.container.image.v1+json" {
		t.Errorf("Config = %+v", m.Config)
	}
	if len(m.Layers) != 1 || m.Layers[0].Digest != dL1 || m.Layers[0].Size != 2818413 {
		t.Errorf("Layers = %+v", m.Layers)
	}
	if m.Subject != nil {
		t.Errorf("Subject = %+v, want nil", m.Subject)
	}
}

func TestParseManifestOCIIndex(t *testing.T) {
	m := mustParse(t, ociIndexBody)
	if m.MediaType != MediaTypeOCIIndex {
		t.Errorf("MediaType = %q", m.MediaType)
	}
	if !m.IsIndex() {
		t.Error("IsIndex() = false for an OCI index")
	}
	if len(m.Manifests) != 2 || m.Manifests[0].Digest != dM1 || m.Manifests[0].Size != 7143 || m.Manifests[1].Digest != dM2 {
		t.Errorf("Manifests = %+v", m.Manifests)
	}
	if m.Config != nil || m.Layers != nil {
		t.Errorf("Config = %+v, Layers = %+v, want nil", m.Config, m.Layers)
	}
	if m.Annotations["com.example.key1"] != "value1" {
		t.Errorf("Annotations = %v", m.Annotations)
	}
}

func TestParseManifestDockerManifestList(t *testing.T) {
	body := strings.Replace(ociIndexBody, MediaTypeOCIIndex, MediaTypeDockerManifestList, 1)
	m := mustParse(t, body)
	if m.MediaType != MediaTypeDockerManifestList {
		t.Errorf("MediaType = %q", m.MediaType)
	}
	if !m.IsIndex() {
		t.Error("IsIndex() = false for a docker manifest list")
	}
	if got := m.EffectiveMediaType(""); got != MediaTypeDockerManifestList {
		t.Errorf("EffectiveMediaType(\"\") = %q", got)
	}
}

func TestParseManifestIndexWithoutMediaType(t *testing.T) {
	m := mustParse(t, untypedIndexBody)
	if m.MediaType != "" {
		t.Errorf("MediaType = %q, want empty", m.MediaType)
	}
	if !m.IsIndex() {
		t.Error("IsIndex() = false for an untyped body with manifests")
	}
	if got := m.EffectiveMediaType(""); got != MediaTypeOCIIndex {
		t.Errorf("EffectiveMediaType(\"\") = %q, want %q", got, MediaTypeOCIIndex)
	}
	if m.BlobDescriptors() != nil {
		t.Errorf("BlobDescriptors() = %+v, want nil", m.BlobDescriptors())
	}

	// An empty manifests array is still an index.
	empty := mustParse(t, `{"schemaVersion":2,"manifests":[]}`)
	if !empty.IsIndex() {
		t.Error("IsIndex() = false for {\"manifests\":[]}")
	}
	if len(empty.Manifests) != 0 || empty.Manifests == nil {
		t.Errorf("Manifests = %#v, want empty non-nil", empty.Manifests)
	}

	// No mediaType and no manifests is an image manifest.
	plain := mustParse(t, `{"schemaVersion":2,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"`+string(dCfg)+`","size":1},"layers":[]}`)
	if plain.IsIndex() {
		t.Error("IsIndex() = true for an untyped manifest with config")
	}
	if got := plain.EffectiveMediaType(""); got != MediaTypeOCIManifest {
		t.Errorf("EffectiveMediaType(\"\") = %q, want %q", got, MediaTypeOCIManifest)
	}

	// A typed manifest with a stray manifests array is not an index.
	typed := mustParse(t, `{"schemaVersion":2,"mediaType":"`+MediaTypeOCIManifest+`","manifests":[]}`)
	if typed.IsIndex() {
		t.Error("IsIndex() = true when mediaType says manifest")
	}
}

func TestParseManifestInvalid(t *testing.T) {
	badLayer := strings.Replace(ociManifestBody, string(dL1), "sha256:abc", 1)
	upperLayer := strings.Replace(ociManifestBody, string(dL1), strings.ToUpper(string(dL1)), 1)
	sha512Layer := strings.Replace(ociManifestBody, string(dL1), "sha512:"+strings.Repeat("0", 128), 1)
	badConfig := strings.Replace(ociManifestBody, string(dCfg), "sha256:", 1)
	badSubject := strings.Replace(ociManifestBody, string(dSub), "not-a-digest", 1)
	badChild := strings.Replace(ociIndexBody, string(dM2), "sha256:"+strings.Repeat("0", 63), 1)
	negativeSize := strings.Replace(ociManifestBody, `"size": 7023`, `"size": -1`, 1)
	negativeLayerSize := strings.Replace(ociManifestBody, `"size": 32654`, `"size": -32654`, 1)
	schema1 := strings.Replace(ociManifestBody, `"schemaVersion": 2`, `"schemaVersion": 1`, 1)
	schema3 := strings.Replace(ociManifestBody, `"schemaVersion": 2`, `"schemaVersion": 3`, 1)
	noSchema := strings.Replace(ociManifestBody, `"schemaVersion": 2,`, ``, 1)
	stringSchema := strings.Replace(ociManifestBody, `"schemaVersion": 2`, `"schemaVersion": "2"`, 1)
	missingDigest := strings.Replace(ociManifestBody, `"digest": "`+string(dL2)+`",`, ``, 1)

	cases := []struct {
		name string
		body string
		want string // substring of the error message
	}{
		{"empty body", "", "not valid JSON"},
		{"truncated JSON", `{"schemaVersion": 2, "layers": [`, "not valid JSON"},
		{"JSON array", `[]`, "not valid JSON"},
		{"JSON string", `"manifest"`, "not valid JSON"},
		{"JSON null", `null`, "schemaVersion 0"},
		{"empty object", `{}`, "schemaVersion 0"},
		{"schemaVersion 1", schema1, "schemaVersion 1"},
		{"schemaVersion 3", schema3, "schemaVersion 3"},
		{"missing schemaVersion", noSchema, "schemaVersion 0"},
		{"string schemaVersion", stringSchema, "not valid JSON"},
		{"short layer digest", badLayer, "layers[0]"},
		{"uppercase layer digest", upperLayer, "layers[0]"},
		{"sha512 layer digest", sha512Layer, "layers[0]"},
		{"empty config digest", badConfig, "config"},
		{"bad subject digest", badSubject, "subject"},
		{"bad index child digest", badChild, "manifests[1]"},
		{"missing layer digest", missingDigest, "layers[1]"},
		{"negative config size", negativeSize, "negative size"},
		{"negative layer size", negativeLayerSize, "negative size"},
		{"float size", strings.Replace(ociManifestBody, `"size": 7023`, `"size": 1.5`, 1), "not valid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseManifest([]byte(tc.body))
			if err == nil {
				t.Fatalf("ParseManifest succeeded: %+v", m)
			}
			if m != nil {
				t.Fatalf("ParseManifest returned %+v alongside error", m)
			}
			var oe *Error
			if !errors.As(err, &oe) {
				t.Fatalf("error %T is not *Error", err)
			}
			if oe.Code != CodeManifestInvalid {
				t.Fatalf("code = %s, want %s", oe.Code, CodeManifestInvalid)
			}
			if !strings.Contains(oe.Message, tc.want) {
				t.Fatalf("message %q does not contain %q", oe.Message, tc.want)
			}
		})
	}
}

func TestEffectiveMediaType(t *testing.T) {
	manifest := mustParse(t, ociManifestBody)
	docker := mustParse(t, dockerManifestBody)
	index := mustParse(t, ociIndexBody)
	untypedIndex := mustParse(t, untypedIndexBody)
	untypedManifest := mustParse(t, `{"schemaVersion":2,"layers":[]}`)

	cases := []struct {
		name        string
		m           *Manifest
		contentType string
		want        string
	}{
		{"no content type, OCI manifest", manifest, "", MediaTypeOCIManifest},
		{"no content type, docker manifest", docker, "", MediaTypeDockerManifest},
		{"no content type, OCI index", index, "", MediaTypeOCIIndex},
		{"no content type, untyped index", untypedIndex, "", MediaTypeOCIIndex},
		{"no content type, untyped manifest", untypedManifest, "", MediaTypeOCIManifest},
		{"plain content type", manifest, MediaTypeOCIManifest, MediaTypeOCIManifest},
		{"content type with charset", manifest, MediaTypeOCIManifest + "; charset=utf-8", MediaTypeOCIManifest},
		{"content type with params no space", manifest, MediaTypeOCIManifest + ";charset=utf-8;q=1", MediaTypeOCIManifest},
		{"content type with surrounding space", manifest, "  " + MediaTypeOCIManifest + " ; charset=utf-8", MediaTypeOCIManifest},
		{"content type wins over mediaType", manifest, MediaTypeDockerManifest, MediaTypeDockerManifest},
		{"content type wins for untyped index", untypedIndex, MediaTypeDockerManifestList, MediaTypeDockerManifestList},
		{"unknown content type kept verbatim", manifest, "application/vnd.example.custom+json", "application/vnd.example.custom+json"},
		{"case preserved", manifest, "application/vnd.Example.Custom+json; v=2", "application/vnd.Example.Custom+json"},
		{"only params", manifest, "; charset=utf-8", MediaTypeOCIManifest},
		{"whitespace only", untypedIndex, "   ", MediaTypeOCIIndex},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.EffectiveMediaType(tc.contentType); got != tc.want {
				t.Fatalf("EffectiveMediaType(%q) = %q, want %q", tc.contentType, got, tc.want)
			}
		})
	}
}

func TestEffectiveArtifactType(t *testing.T) {
	withArtifact := strings.Replace(ociManifestBody, `"schemaVersion": 2,`, `"schemaVersion": 2, "artifactType": "application/vnd.example+type",`, 1)
	indexWithArtifact := strings.Replace(ociIndexBody, `"schemaVersion": 2,`, `"schemaVersion": 2, "artifactType": "application/vnd.example.index+type",`, 1)

	cases := []struct {
		name string
		body string
		want string
	}{
		{"explicit artifactType", withArtifact, "application/vnd.example+type"},
		{"falls back to config.mediaType", ociManifestBody, MediaTypeOCIConfig},
		{"docker config fallback", dockerManifestBody, "application/vnd.docker.container.image.v1+json"},
		{"index with artifactType", indexWithArtifact, "application/vnd.example.index+type"},
		{"index without artifactType", ociIndexBody, ""},
		{"untyped index", untypedIndexBody, ""},
		{"manifest without config", `{"schemaVersion":2,"layers":[]}`, ""},
		{"empty config mediaType", `{"schemaVersion":2,"config":{"digest":"` + string(dCfg) + `","size":2}}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := mustParse(t, tc.body)
			if got := m.EffectiveArtifactType(); got != tc.want {
				t.Fatalf("EffectiveArtifactType() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBlobDescriptors(t *testing.T) {
	m := mustParse(t, ociManifestBody)
	got := m.BlobDescriptors()
	if len(got) != 3 {
		t.Fatalf("BlobDescriptors() = %+v, want 3 entries", got)
	}
	if got[0].Digest != dCfg || got[0].Size != 7023 || got[0].MediaType != MediaTypeOCIConfig {
		t.Errorf("[0] = %+v, want config", got[0])
	}
	if got[1].Digest != dL1 || got[2].Digest != dL2 {
		t.Errorf("[1..2] = %+v, want layers in order", got[1:])
	}
	// The result is a copy: mutating it leaves the manifest alone.
	got[0].Size = 1
	if m.Config.Size != 7023 {
		t.Error("BlobDescriptors aliases Config")
	}

	docker := mustParse(t, dockerManifestBody)
	if d := docker.BlobDescriptors(); len(d) != 2 || d[0].Digest != dCfg || d[1].Digest != dL1 {
		t.Errorf("docker BlobDescriptors() = %+v", d)
	}

	noConfig := mustParse(t, `{"schemaVersion":2,"layers":[{"mediaType":"application/octet-stream","digest":"`+string(dL1)+`","size":5}]}`)
	if d := noConfig.BlobDescriptors(); len(d) != 1 || d[0].Digest != dL1 {
		t.Errorf("no-config BlobDescriptors() = %+v", d)
	}

	bare := mustParse(t, `{"schemaVersion":2}`)
	if d := bare.BlobDescriptors(); d == nil || len(d) != 0 {
		t.Errorf("bare BlobDescriptors() = %#v, want empty non-nil", d)
	}

	for _, body := range []string{ociIndexBody, untypedIndexBody, `{"schemaVersion":2,"manifests":[]}`} {
		if d := mustParse(t, body).BlobDescriptors(); d != nil {
			t.Errorf("index BlobDescriptors() = %+v, want nil", d)
		}
	}
}

func TestParsePlatform(t *testing.T) {
	cases := []struct {
		in   string
		want Platform
		ok   bool
	}{
		{"linux/amd64", Platform{OS: "linux", Architecture: "amd64"}, true},
		{"linux/arm64/v8", Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}, true},
		{"linux", Platform{}, false},
		{"linux/", Platform{}, false},
		{"/amd64", Platform{}, false},
		{"linux/arm64/v8/extra", Platform{}, false},
		{"", Platform{}, false},
	}
	for _, c := range cases {
		got, err := ParsePlatform(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("ParsePlatform(%q) = %+v, %v; want %+v, ok=%v", c.in, got, err, c.want, c.ok)
		}
		if c.ok && got.String() != c.in {
			t.Errorf("String() = %q, want %q", got.String(), c.in)
		}
	}
}

func TestPlatformMatches(t *testing.T) {
	arm := Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
	if !(Platform{OS: "linux", Architecture: "arm64"}).Matches(arm) {
		t.Error("a request without a variant must match any variant")
	}
	if !arm.Matches(arm) || (Platform{OS: "linux", Architecture: "arm64", Variant: "v7"}).Matches(arm) {
		t.Error("a request with a variant must match that variant only")
	}
	if (Platform{OS: "windows", Architecture: "arm64"}).Matches(arm) || (Platform{OS: "linux", Architecture: "amd64"}).Matches(arm) {
		t.Error("os and architecture must match")
	}
}

func TestParseManifestKeepsPlatform(t *testing.T) {
	m := mustParse(t, ociIndexBody)
	if len(m.Manifests) != 2 || m.Manifests[0].Platform == nil || m.Manifests[1].Platform == nil {
		t.Fatalf("platforms not kept: %+v", m.Manifests)
	}
	if got := m.Manifests[0].Platform.String(); got != "linux/ppc64le" {
		t.Fatalf("first child platform = %q", got)
	}
	if mustParse(t, untypedIndexBody).Manifests[0].Platform != nil {
		t.Fatal("a child without a platform must have a nil Platform")
	}
}
