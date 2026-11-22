package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
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

	"github.com/imprun/worker-platform/internal/platform"
	ate "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type ateOptions struct {
	address, ca, serverName, tokenFile    string
	routerURL, routerCA, routerServerName string
	routerCertificate, routerPrivateKey   string
}

func (o *ateOptions) register(flags *flag.FlagSet) {
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

func (o ateOptions) deploy(ctx context.Context, out io.Writer, worker platform.Worker, profile platform.WorkerProfile, kubeContext string) error {
	host, port, err := net.SplitHostPort(o.address)
	if err != nil || host == "" || port == "" || o.ca == "" || o.tokenFile == "" {
		return errors.New("ATE deployment requires -ate-address host:port, -ate-ca, and -ate-token-file")
	}
	roots, err := loadTrust(o.ca)
	if err != nil {
		return fmt.Errorf("load ATE Control trust: %w", err)
	}
	token := ateTokenFile(o.tokenFile)
	if _, err := token.GetRequestMetadata(ctx); err != nil {
		return err
	}
	probe, err := o.newRouterProbe()
	if err != nil {
		return err
	}
	defer probe.client.CloseIdleConnections()
	conn, err := grpc.NewClient(o.address,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: o.serverName,
		})),
		grpc.WithPerRPCCredentials(token))
	if err != nil {
		return fmt.Errorf("configure ATE Control client: %w", err)
	}
	defer conn.Close()
	deployment := platform.ATEDeployment{
		Control: ate.NewControlClient(conn), Cluster: platform.Kubernetes{Context: kubeContext}, Ready: probe.ready,
	}
	result, err := deployment.Deploy(ctx, worker, profile)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
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
	endpoint string
}

func (o ateOptions) newRouterProbe() (routerProbe, error) {
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
	u.Path = "/readyz"
	return routerProbe{
		endpoint: u.String(),
		client: &http.Client{
			Timeout:       5 * time.Second,
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

func (p routerProbe) ready(ctx context.Context, actor *ate.ObjectRef) (bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.endpoint, nil)
	if err != nil {
		return false, err
	}
	request.Header.Set("ate-target-actor", actor.GetAtespace()+"/"+actor.GetName())
	response, err := p.client.Do(request)
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

func renderATE(out io.Writer, worker platform.Worker, profile platform.WorkerProfile, selection string) error {
	plan, err := platform.BuildATEPlan(worker, profile)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if selection == "route" {
		return encoder.Encode(plan.Route)
	}
	values := []struct {
		name    string
		message proto.Message
	}{{"template", plan.Template}, {"actor", plan.Actor}, {"policy", plan.Policy}}
	for _, value := range values {
		if selection != "all" && selection != value.name {
			continue
		}
		data, err := (protojson.MarshalOptions{Indent: "  "}).Marshal(value.message)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out, string(data)); err != nil || selection == value.name {
			return err
		}
		if _, err := fmt.Fprintln(out, "---"); err != nil {
			return err
		}
	}
	if selection == "all" {
		return encoder.Encode(plan.Route)
	}
	return fmt.Errorf("unknown ATE output %q (expected all, template, actor, policy, or route)", selection)
}
