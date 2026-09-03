package oci

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
)

// validHex is a well-formed sha256 encoded part shared by the oci tests.
const validHex = "6c3c624b58dbbcd3c0dd82b4c53f04194d1247c6eebdaab7c610cf7d66709b3b"

func TestParseDigest(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"valid sha256", "sha256:" + validHex, true},
		{"all zero hex", "sha256:" + strings.Repeat("0", 64), true},
		{"uppercase hex", "sha256:" + strings.ToUpper(validHex), false},
		{"mixed case hex", "sha256:6C" + validHex[2:], false},
		{"63 hex chars", "sha256:" + validHex[:63], false},
		{"65 hex chars", "sha256:" + validHex + "0", false},
		{"non-hex character", "sha256:" + validHex[:63] + "g", false},
		{"sha512 rejected", "sha512:" + validHex + validHex, false},
		{"blake3 rejected", "blake3:" + validHex, false},
		{"uppercase algorithm", "SHA256:" + validHex, false},
		{"missing colon", "sha256" + validHex, false},
		{"empty encoded part", "sha256:", false},
		{"empty algorithm", ":" + validHex, false},
		{"empty string", "", false},
		{"leading space", " sha256:" + validHex, false},
		{"trailing newline", "sha256:" + validHex + "\n", false},
		{"two colons", "sha256:" + validHex[:32] + ":" + validHex[32:], false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := ParseDigest(tc.in)
			if tc.ok {
				if err != nil {
					t.Fatalf("ParseDigest(%q): unexpected error %v", tc.in, err)
				}
				if string(d) != tc.in {
					t.Fatalf("ParseDigest(%q) = %q", tc.in, d)
				}
				return
			}
			if err == nil {
				t.Fatalf("ParseDigest(%q) = %q, want error", tc.in, d)
			}
			if d != "" {
				t.Fatalf("ParseDigest(%q) returned %q alongside error", tc.in, d)
			}
			var oe *Error
			if !errors.As(err, &oe) {
				t.Fatalf("ParseDigest(%q) error %T is not *Error", tc.in, err)
			}
			if oe.Code != CodeDigestInvalid {
				t.Fatalf("ParseDigest(%q) code = %s, want %s", tc.in, oe.Code, CodeDigestInvalid)
			}
		})
	}
}

func TestDigestOfBytes(t *testing.T) {
	cases := []struct {
		in   string
		want Digest
	}{
		{"", "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"hello", "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"},
		{"{}", "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"},
	}
	for _, tc := range cases {
		got := DigestOfBytes([]byte(tc.in))
		if got != tc.want {
			t.Errorf("DigestOfBytes(%q) = %s, want %s", tc.in, got, tc.want)
		}
		if _, err := ParseDigest(string(got)); err != nil {
			t.Errorf("DigestOfBytes(%q) does not parse: %v", tc.in, err)
		}
	}
}

func TestDigestFromSum(t *testing.T) {
	sum := sha256.Sum256([]byte("hello"))
	if got, want := DigestFromSum(sum[:]), DigestOfBytes([]byte("hello")); got != want {
		t.Fatalf("DigestFromSum = %s, want %s", got, want)
	}
	h := sha256.New()
	h.Write([]byte("hel"))
	h.Write([]byte("lo"))
	if got, want := DigestFromSum(h.Sum(nil)), DigestOfBytes([]byte("hello")); got != want {
		t.Fatalf("DigestFromSum(streamed) = %s, want %s", got, want)
	}

	for _, n := range []int{0, 31, 33, 64} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("DigestFromSum with %d bytes did not panic", n)
				}
			}()
			DigestFromSum(make([]byte, n))
		}()
	}
}

func TestDigestParts(t *testing.T) {
	d, err := ParseDigest("sha256:" + validHex)
	if err != nil {
		t.Fatal(err)
	}
	if d.String() != "sha256:"+validHex {
		t.Errorf("String() = %q", d.String())
	}
	if d.Algorithm() != "sha256" || d.Algorithm() != AlgorithmSHA256 {
		t.Errorf("Algorithm() = %q", d.Algorithm())
	}
	if d.Hex() != validHex {
		t.Errorf("Hex() = %q", d.Hex())
	}
	if len(d.Hex()) != sha256.Size*2 {
		t.Errorf("Hex() length = %d", len(d.Hex()))
	}
}

func TestIsDigest(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"sha256:" + validHex, true},
		{"sha512:" + validHex + validHex, true},
		{"blake3:" + validHex, true},
		{"multihash+base58:QmRZxt2b1FVZPNqd8hsiykDL3TdBDeTSPX9Kv46HmX4Gx8", true},
		{"sha256+b64u:LCa0a2j_xo_5m0U8HTBBNBNCLXBkg7-g-YpeiGJm564", true},
		{"sha256:abc", true},                          // digest-shaped, ParseDigest rejects it later
		{"sha256:" + strings.ToUpper(validHex), true}, // same: routes by digest, then DIGEST_INVALID
		{"latest", false},
		{"v1.2.3", false},
		{"1.0", false},
		{"sha256", false},
		{"sha256:", false},
		{":" + validHex, false},
		{"SHA256:" + validHex, false},
		{"sha256:" + validHex + "/x", false},
		{"sha256:" + validHex + " ", false},
		{"sha256::" + validHex, false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsDigest(tc.in); got != tc.want {
			t.Errorf("IsDigest(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
