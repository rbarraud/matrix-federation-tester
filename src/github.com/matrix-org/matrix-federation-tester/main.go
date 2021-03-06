package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"github.com/matrix-org/golang-matrixfederation"
	"github.com/prometheus/client_golang/prometheus"
	"net/http"
	_ "net/http/pprof"
	"os"
	"time"
)

// HandleReport handles an HTTP request for a JSON report for matrix server.
// GET /api/report?server_name=matrix.org&tls_sni=whatever request.
func HandleReport(w http.ResponseWriter, req *http.Request) {
	// Set unrestricted Access-Control headers so that this API can be used by
	// web apps running in browsers.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept")
	if req.Method == "OPTIONS" {
		return
	}
	if req.Method != "GET" {
		w.WriteHeader(405)
		fmt.Printf("Unsupported method.")
		return
	}
	serverName := req.URL.Query().Get("server_name")
	tlsSNI := req.URL.Query().Get("tls_sni")
	result, err := JSONReport(serverName, tlsSNI)
	if err != nil {
		w.WriteHeader(500)
		fmt.Printf("Error Generating Report: %q", err.Error())
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write(result)
	}
}

// JSONReport generates a JSON formatted report for a matrix server.
func JSONReport(serverName, sni string) ([]byte, error) {
	results, err := Report(serverName, sni)
	if err != nil {
		return nil, err
	}
	results.touchUpReport()
	encoded, err := json.Marshal(results)
	if err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	json.Indent(&buffer, encoded, "", "  ")
	return buffer.Bytes(), nil
}

func main() {
	http.HandleFunc("/api/report", prometheus.InstrumentHandlerFunc("report", HandleReport))
	http.Handle("/metrics", prometheus.Handler())
	http.ListenAndServe(os.Getenv("BIND_ADDRESS"), nil)
}

// A ServerReport is a report for a matrix server.
type ServerReport struct {
	DNSResult         matrixfederation.DNSResult  // The result of looking up the server in DNS.
	ConnectionReports map[string]ConnectionReport // The report for each server address we could connect to.
	ConnectionErrors  map[string]error            // The errors for each server address we couldn't connect to.
}

// A ConnectionReport is information about a connection made to a matrix server.
type ConnectionReport struct {
	Certificates          []X509CertSummary                        // Summary information for each x509 certificate served up by this server.
	Cipher                CipherSummary                            // Summary information on the TLS cipher used by this server.
	Keys                  *json.RawMessage                         // The server key JSON returned by this server.
	Checks                matrixfederation.KeyChecks               // The checks applied to the server and their results.
	Ed25519VerifyKeys     map[string]matrixfederation.Base64String // The Verify keys for this server or nil if the checks were not ok.
	SHA256TLSFingerprints []matrixfederation.Base64String          // The SHA256 tls fingerprints for this server or nil if the checks were not ok.
}

// A CipherSummary is a summary of the TLS version and Cipher used in a TLS connection.
type CipherSummary struct {
	Version     string // Human readable description of the TLS version.
	CipherSuite string // Human readable description of the TLS cipher.
}

// A X509CertSummary is a summary of the information in a X509 certificate.
type X509CertSummary struct {
	SubjectCommonName string                        // The common name of the subject.
	IssuerCommonName  string                        // The common name of the issuer.
	SHA256Fingerprint matrixfederation.Base64String // The SHA256 fingerprint of the certificate.
	DNSNames          []string                      // The DNS names this certificate is valid for.
}

// Report creates a ServerReport for a matrix server.
func Report(serverName string, sni string) (*ServerReport, error) {
	var report ServerReport
	dnsResult, err := matrixfederation.LookupServer(serverName)
	if err != nil {
		return nil, err
	}
	report.DNSResult = *dnsResult
	// Map of network address to report.
	report.ConnectionReports = make(map[string]ConnectionReport)
	// Map of network address to connection error.
	report.ConnectionErrors = make(map[string]error)
	now := time.Now()
	for _, addr := range report.DNSResult.Addrs {
		keys, connState, err := matrixfederation.FetchKeysDirect(serverName, addr, sni)
		if err != nil {
			report.ConnectionErrors[addr] = err
			continue
		}
		var connReport ConnectionReport
		for _, cert := range connState.PeerCertificates {
			fingerprint := sha256.Sum256(cert.Raw)
			summary := X509CertSummary{
				SubjectCommonName: cert.Subject.CommonName,
				IssuerCommonName:  cert.Issuer.CommonName,
				SHA256Fingerprint: fingerprint[:],
				DNSNames:          cert.DNSNames,
			}
			connReport.Certificates = append(connReport.Certificates, summary)
		}
		connReport.Cipher.Version = enumToString(tlsVersions, connState.Version)
		connReport.Cipher.CipherSuite = enumToString(tlsCipherSuites, connState.CipherSuite)
		connReport.Checks, connReport.Ed25519VerifyKeys, connReport.SHA256TLSFingerprints = matrixfederation.CheckKeys(serverName, now, *keys, connState)
		raw := json.RawMessage(keys.Raw)
		connReport.Keys = &raw
		report.ConnectionReports[addr] = connReport
	}
	return &report, nil
}

// A ReportError is a version of a golang error that is human readable when serialised as JSON.
type ReportError struct {
	Message string // The result of err.Error()
}

// Error implements the error interface.
func (e ReportError) Error() string {
	return e.Message
}

// Replace a golang error with an error that is human readable when serialised as JSON.
func asReportError(err error) error {
	if err != nil {
		return ReportError{err.Error()}
	}
	return nil
}

// touchUpReport converts all the errors in a ServerReport into forms that will be human readable after JSON serialisation.
func (report *ServerReport) touchUpReport() {
	report.DNSResult.SRVError = asReportError(report.DNSResult.SRVError)
	for host, hostReport := range report.DNSResult.Hosts {
		hostReport.Error = asReportError(hostReport.Error)
		report.DNSResult.Hosts[host] = hostReport
	}
	for addr, err := range report.ConnectionErrors {
		report.ConnectionErrors[addr] = asReportError(err)
	}
}

// enumToString converts a uint16 enum into a human readable string using a fixed mapping.
// If no mapping can be found then return a "UNKNOWN[0x%x]" string with the raw enum.
func enumToString(names map[uint16]string, value uint16) string {
	if name, ok := names[value]; ok {
		return name
	}
	return fmt.Sprintf("UNKNOWN[0x%x]", value)
}

var (
	tlsVersions = map[uint16]string{
		tls.VersionSSL30: "SSL 3.0",
		tls.VersionTLS10: "TLS 1.0",
		tls.VersionTLS11: "TLS 1.1",
		tls.VersionTLS12: "TLS 1.2",
	}
	tlsCipherSuites = map[uint16]string{
		tls.TLS_RSA_WITH_RC4_128_SHA:                "TLS_RSA_WITH_RC4_128_SHA",
		tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA:           "TLS_RSA_WITH_3DES_EDE_CBC_SHA",
		tls.TLS_RSA_WITH_AES_128_CBC_SHA:            "TLS_RSA_WITH_AES_128_CBC_SHA",
		tls.TLS_RSA_WITH_AES_256_CBC_SHA:            "TLS_RSA_WITH_AES_256_CBC_SHA",
		tls.TLS_ECDHE_ECDSA_WITH_RC4_128_SHA:        "TLS_ECDHE_ECDSA_WITH_RC4_128_SHA",
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA:    "TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA",
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA:    "TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA",
		tls.TLS_ECDHE_RSA_WITH_RC4_128_SHA:          "TLS_ECDHE_RSA_WITH_RC4_128_SHA",
		tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA:     "TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA",
		tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA:      "TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA",
		tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA:      "TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA",
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256:   "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256",
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256: "TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256",
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384: "TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384",
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384:   "TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384",
		// go1.5.3 doesn't have these enums, but they appear in more recent version.
		// tls.TLS_RSA_WITH_AES_128_GCM_SHA256:         "TLS_RSA_WITH_AES_128_GCM_SHA256",
		// tls.TLS_RSA_WITH_AES_256_GCM_SHA384:         "TLS_RSA_WITH_AES_256_GCM_SHA384",
	}
)
