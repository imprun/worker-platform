package ateclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	ate "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type Options struct {
	address, ca, serverName, tokenFile    string
	routerURL, routerCA, routerServerName string
	routerCertificate, routerPrivateKey   string
}

func (o *Options) Register(flags *flag.FlagSet) {
	flags.StringVar(&o.address, "ate-address", "", "ATE Control gRPC address (host:port)")
	flags.StringVar(&o.ca, "ate-ca", "", "ATE Control server CA bundle")
	flags.StringVar(&o.serverName, "ate-server-name", "", "ATE Control TLS server name for a tunnel")
	flags.StringVar(&o.tokenFile, "ate-token-file", "", "file containing the ATE Control bearer token")
	flags.StringVar(&o.routerURL, "router-url", "", "HTTPS origin of the ATE Router readiness probe")
	flags.StringVar(&o.routerCA, "router-ca", "", "ATE Router server CA bundle")
	flags.StringVar(&o.routerServerName, "router-server-name", "", "ATE Router TLS server name for a tunnel")
	flags.StringVar(&o.routerCertificate, "router-cert", "", "client certificate for the ATE Router")
	flags.StringVar(&o.routerPrivateKey, "router-key", "", "client private key for the ATE Router")
}

type Client struct {
	Control ate.ControlClient
	conn    *grpc.ClientConn
	probe   routerProbe
}

func (o Options) Open() (*Client, error) {
	host, port, err := net.SplitHostPort(o.address)
	if err != nil || host == "" || port == "" || o.ca == "" || o.tokenFile == "" {
		return nil, errors.New("ATE deployment requires -ate-address host:port, -ate-ca, and -ate-token-file")
	}
	roots, err := loadTrust(o.ca)
	if err != nil {
		return nil, fmt.Errorf("load ATE Control trust: %w", err)
	}
	token := ateTokenFile(o.tokenFile)
	if _, err := token.GetRequestMetadata(context.Background()); err != nil {
		return nil, err
	}
	probe, err := o.newRouterProbe()
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(o.address,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: o.serverName,
		})),
		grpc.WithPerRPCCredentials(token))
	if err != nil {
		return nil, fmt.Errorf("configure ATE Control client: %w", err)
	}
	return &Client{Control: ate.NewControlClient(conn), conn: conn, probe: probe}, nil
}

func (c *Client) Ready(ctx context.Context, actor *ate.ObjectRef) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	response, err := c.probe.request(ctx, http.MethodGet, "/readyz", actor, nil)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return false, nil
	default:
		return false, fmt.Errorf("ATE Router readiness returned HTTP %d", response.StatusCode)
	}
}

func (c *Client) InvokeAttempt(ctx context.Context, actor *ate.ObjectRef, body []byte, limit int64) (int, []byte, error) {
	response, err := c.probe.request(ctx, http.MethodPost, "/attempt", actor, body)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPreconditionRequired {
		return response.StatusCode, nil, nil
	}
	result, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	return response.StatusCode, result, err
}

func (c *Client) Close() error {
	c.probe.client.CloseIdleConnections()
	return c.conn.Close()
}

type ateTokenFile string

func (file ateTokenFile) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	data, err := os.ReadFile(string(file))
	if err != nil {
		return nil, fmt.Errorf("read ATE Control token file: %w", err)
	}
	value := strings.TrimSpace(string(data))
	if value == "" || strings.ContainsAny(value, " \t\r\n") {
		return nil, errors.New("ATE Control token file must contain a single token")
	}
	return map[string]string{"authorization": "Bearer " + value}, nil
}

func (ateTokenFile) RequireTransportSecurity() bool { return true }

func loadTrust(path string) (*x509.CertPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data) {
		return nil, errors.New("CA bundle has no valid PEM certificates")
	}
	return roots, nil
}

type routerProbe struct {
	client   *http.Client
	endpoint url.URL
}

func (o Options) newRouterProbe() (routerProbe, error) {
	u, err := url.Parse(o.routerURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return routerProbe{}, errors.New("-router-url must be an HTTPS origin")
	}
	if o.routerCA == "" || o.routerCertificate == "" || o.routerPrivateKey == "" {
		return routerProbe{}, errors.New("Router readiness requires -router-ca, -router-cert, and -router-key")
	}
	roots, err := loadTrust(o.routerCA)
	if err != nil {
		return routerProbe{}, fmt.Errorf("load ATE Router trust: %w", err)
	}
	certificate, err := tls.LoadX509KeyPair(o.routerCertificate, o.routerPrivateKey)
	if err != nil {
		return routerProbe{}, fmt.Errorf("load ATE Router client identity: %w", err)
	}
	return routerProbe{
		endpoint: *u,
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				ForceAttemptHTTP2: true,
				TLSClientConfig: &tls.Config{
					MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: o.routerServerName,
					Certificates: []tls.Certificate{certificate},
				},
			},
		},
	}, nil
}

func (p routerProbe) request(ctx context.Context, method, path string, actor *ate.ObjectRef, body []byte) (*http.Response, error) {
	endpoint := p.endpoint
	endpoint.Path = path
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("ate-target-actor", actor.GetAtespace()+"/"+actor.GetName())
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
		request.GetBody = nil
	}
	return p.client.Do(request)
}
