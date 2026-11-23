package apppackage

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"regexp"
	"strconv"
	"strings"
)

const (
	ReferenceEnv           = "WORKER_APP_PACKAGE"
	RegistryCAEnv          = "WORKER_APP_REGISTRY_CA_PEM"
	ManifestType           = "application/vnd.oci.image.manifest.v1+json"
	ArtifactType           = "application/vnd.imprun.worker-app.v1"
	LayerType              = "application/vnd.imprun.worker-app.layer.v1.tar+gzip"
	ContractV1             = "v1"
	SessionExpiredExitCode = 42
)

var (
	dnsName        = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
	repositoryName = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$`)
	sha256Digest   = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

type Reference struct {
	Host       string
	Port       int32
	Repository string
	Digest     string
}

func (r Reference) authority() string {
	if r.Port == 443 {
		return r.Host
	}
	return net.JoinHostPort(r.Host, strconv.Itoa(int(r.Port)))
}

func (r Reference) String() string { return r.authority() + "/" + r.Repository + "@" + r.Digest }

func ParseReference(value string) (Reference, error) {
	name, digest, pinned := strings.Cut(value, "@")
	registry, repository, qualified := strings.Cut(name, "/")
	host, portText, customPort := strings.Cut(registry, ":")
	port := int64(443)
	if customPort {
		var err error
		port, err = strconv.ParseInt(portText, 10, 32)
		if err != nil || port < 1 || port > 65535 || strconv.FormatInt(port, 10) != portText {
			return Reference{}, errors.New("invalid app registry port")
		}
	}
	if !pinned || !qualified || len(host) > 253 || net.ParseIP(host) != nil || !dnsName.MatchString(host) ||
		len(repository) > 255 || !repositoryName.MatchString(repository) || !sha256Digest.MatchString(digest) {
		return Reference{}, errors.New("app package must be an HTTPS registry repository pinned to a SHA-256 digest")
	}
	return Reference{Host: host, Port: int32(port), Repository: repository, Digest: digest}, nil
}

func AppendRegistryCA(pool *x509.CertPool, bundle string) error {
	const maxBundle = 32 << 10
	data := strings.TrimSpace(bundle)
	if data == "" || len(bundle) > maxBundle {
		return errors.New("expected an app registry PEM certificate bundle of at most 32 KiB")
	}
	for data != "" {
		block, rest := pem.Decode([]byte(data))
		if !strings.HasPrefix(data, "-----BEGIN CERTIFICATE-----") || block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return errors.New("app registry CA bundle must contain only PEM certificates")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return errors.New("invalid app registry CA certificate")
		}
		pool.AddCert(cert)
		data = strings.TrimSpace(string(rest))
	}
	return nil
}
