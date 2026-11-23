package apppackage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	maxManifestBytes = 128 << 10
	maxLayerBytes    = 32 << 20
	maxArchiveBytes  = 64 << 20
	maxArchiveFiles  = 2048
	emptyDigest      = "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"
)

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

type manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	ArtifactType  string       `json:"artifactType"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

type Installed struct {
	Dir        string
	Entrypoint string
}

// Install pulls a manifest and its application layer into a new private directory.
// The caller owns that directory after installation.
func Install(ctx context.Context, client *http.Client, ref Reference) (Installed, error) {
	base := "https://" + ref.authority() + "/v2/" + ref.Repository
	data, err := fetch(ctx, client, base+"/manifests/"+ref.Digest, ManifestType, ref.Digest, maxManifestBytes)
	if err != nil {
		return Installed{}, fmt.Errorf("pull app manifest: %w", err)
	}
	var doc manifest
	if err := json.Unmarshal(data, &doc); err != nil {
		return Installed{}, fmt.Errorf("decode app manifest: %w", err)
	}
	if doc.SchemaVersion != 2 || doc.MediaType != ManifestType || doc.ArtifactType != ArtifactType ||
		doc.Config != (descriptor{MediaType: "application/vnd.oci.empty.v1+json", Digest: emptyDigest, Size: 2}) ||
		len(doc.Layers) != 1 {
		return Installed{}, errors.New("expected a Worker Platform app manifest with one layer and empty config")
	}
	layer := doc.Layers[0]
	if layer.MediaType != LayerType || !sha256Digest.MatchString(layer.Digest) || layer.Size <= 0 || layer.Size > maxLayerBytes {
		return Installed{}, errors.New("invalid app layer descriptor")
	}
	data, err = fetch(ctx, client, base+"/blobs/"+layer.Digest, "", layer.Digest, layer.Size)
	if err != nil {
		return Installed{}, fmt.Errorf("pull app layer: %w", err)
	}
	if int64(len(data)) != layer.Size {
		return Installed{}, errors.New("app layer size does not match its descriptor")
	}
	dir, err := os.MkdirTemp("", "worker-app-")
	if err != nil {
		return Installed{}, err
	}
	if err := unpack(data, dir); err != nil {
		return Installed{}, errors.Join(fmt.Errorf("extract app layer: %w", err), os.RemoveAll(dir))
	}
	app, err := loadApp(dir)
	if err != nil {
		return Installed{}, errors.Join(err, os.RemoveAll(dir))
	}
	return app, nil
}

func fetch(ctx context.Context, client *http.Client, source, mediaType, digest string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, err
	}
	if mediaType != "" {
		req.Header.Set("Accept", mediaType)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit || fmt.Sprintf("sha256:%x", sha256.Sum256(data)) != digest {
		return nil, errors.New("registry content exceeds the limit or does not match the SHA-256 digest")
	}
	return data, nil
}

func unpack(data []byte, dir string) error {
	compressed, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer compressed.Close()
	bounded := &io.LimitedReader{R: compressed, N: maxArchiveBytes + 1}
	archive := tar.NewReader(bounded)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for count := 0; ; count++ {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			// Include tar padding and the gzip footer in the decompression check.
			_, err := io.Copy(io.Discard, bounded)
			if err == nil && bounded.N == 0 {
				err = errors.New("app archive exceeds its expansion limit")
			}
			return err
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(header.Name, "/")
		if count >= maxArchiveFiles || !validPath(name) || header.Size > bounded.N {
			return fmt.Errorf("invalid or oversized app archive entry %q", header.Name)
		}
		if err := extract(root, archive, header, name); err != nil {
			return err
		}
	}
}

func extract(root *os.Root, archive *tar.Reader, header *tar.Header, name string) error {
	switch header.Typeflag {
	case tar.TypeDir:
		return root.MkdirAll(name, 0700)
	case tar.TypeReg:
		if err := root.MkdirAll(path.Dir(name), 0700); err != nil {
			return err
		}
		mode := fs.FileMode(0600) | (header.FileInfo().Mode() & 0100)
		file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		_, err = io.Copy(file, archive)
		return errors.Join(err, file.Close())
	default:
		return fmt.Errorf("unsupported archive entry type for %q", name)
	}
}

func validPath(name string) bool {
	return name != "." && fs.ValidPath(name) && !strings.ContainsAny(name, `\:`)
}

func loadApp(dir string) (Installed, error) {
	file, err := os.Open(filepath.Join(dir, "app.json"))
	if err != nil {
		return Installed{}, err
	}
	defer file.Close()
	const maxAppJSON = 4096
	data, err := io.ReadAll(io.LimitReader(file, maxAppJSON+1))
	if err != nil {
		return Installed{}, err
	}
	var config struct {
		ContractVersion string `json:"contractVersion"`
		Entrypoint      string `json:"entrypoint"`
	}
	if len(data) > maxAppJSON || json.Unmarshal(data, &config) != nil || config.ContractVersion != ContractV1 || !validPath(config.Entrypoint) {
		return Installed{}, errors.New("app.json must specify the v1 contract and a relative entrypoint")
	}
	entrypoint := filepath.Join(dir, filepath.FromSlash(config.Entrypoint))
	info, err := os.Stat(entrypoint)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0100 == 0 {
		return Installed{}, errors.New("app entrypoint must be a regular executable file")
	}
	return Installed{Dir: dir, Entrypoint: entrypoint}, nil
}
