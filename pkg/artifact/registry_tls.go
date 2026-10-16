package artifact

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
)

func registryTransportFromEnvironment() (*http.Transport, error) {
	file := os.Getenv("SSL_CERT_FILE")
	if file == "" {
		return nil, nil
	}
	data, e := os.ReadFile(file)
	if e != nil {
		return nil, fmt.Errorf("SSL_CERT_FILE: %w", e)
	}
	roots, e := x509.SystemCertPool()
	if e != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("SSL_CERT_FILE contains no usable CA certificates")
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	var transport *http.Transport
	if ok {
		transport = base.Clone()
	} else {
		transport = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	return transport, nil
}
