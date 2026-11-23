package apppackage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestInstallRejectsRegistryLayerWithWrongDigest(t *testing.T) {
	layer := descriptor{MediaType: LayerType, Digest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("good"))), Size: 4}
	doc, err := json.Marshal(manifest{
		SchemaVersion: 2, MediaType: ManifestType, ArtifactType: ArtifactType,
		Config: descriptor{MediaType: "application/vnd.oci.empty.v1+json", Digest: emptyDigest, Size: 2},
		Layers: []descriptor{layer},
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(doc))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/workers/echo/manifests/"+digest, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != ManifestType {
			t.Error("missing OCI manifest media type")
		}
		w.Write(doc)
	})
	mux.HandleFunc("GET /v2/workers/echo/blobs/"+layer.Digest, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("evil"))
	})
	server := httptest.NewTLSServer(mux)
	defer server.Close()
	host, portText, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Install(t.Context(), server.Client(), Reference{Host: host, Port: int32(port), Repository: "workers/echo", Digest: digest})
	if err == nil || !strings.Contains(err.Error(), "SHA-256 digest") {
		t.Fatalf("digest mismatch should fail before extraction, got %v", err)
	}
}

func TestUnpackRejectsEscapingArchiveEntries(t *testing.T) {
	for _, header := range []tar.Header{
		{Name: "../outside", Typeflag: tar.TypeReg, Mode: 0600},
		{Name: "bin/app", Typeflag: tar.TypeSymlink, Linkname: "/bin/sh"},
	} {
		t.Run(header.Name, func(t *testing.T) {
			var encoded bytes.Buffer
			compressed := gzip.NewWriter(&encoded)
			archive := tar.NewWriter(compressed)
			if err := archive.WriteHeader(&header); err != nil {
				t.Fatal(err)
			}
			if err := archive.Close(); err != nil {
				t.Fatal(err)
			}
			if err := compressed.Close(); err != nil {
				t.Fatal(err)
			}
			if err := unpack(encoded.Bytes(), t.TempDir()); err == nil {
				t.Fatal("unsafe archive entry was accepted")
			}
		})
	}
}
