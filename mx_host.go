package main

import (
	"errors"
	"github.com/deckarep/golang-set"
	"github.com/zmap/zgrab/zlib"
	"github.com/zmap/zgrab/ztools/x509"
	"github.com/zmap/zgrab/ztools/ztls"
	"net"
	"strings"
	"time"
)

// Summarizes the results of multiple connections to a single host
type MxHostSummary struct {
	address         net.IP    `json:"-"`
	Updated         time.Time `json:"updated"`
	Starttls        *bool     `json:"starttls"`
	tlsVersions     mapset.Set
	tlsCipherSuites mapset.Set
	certificates    []*x509.Certificate
	fingerprints    [][]byte
	validity        *CertificateValidity
	ecdheCurveType  *byte
	ecdheCurveId    *ztls.CurveID
	ecdheKeyLength  *int
	Error           *string `json:"error"` // only the first error
}

// The result of a single connection attempt using zlib.Grab
type MxHostGrab struct {
	starttls       *bool
	tlsVersion     ztls.TLSVersion
	tlsCipherSuite ztls.CipherSuite
	certificates   []*x509.Certificate
	dhParams       interface{}
	Error          *string
}

// Summry of multiple connection attemps to a single host
func NewMxHostSummary(address net.IP) *MxHostSummary {
	result := &MxHostSummary{
		address: address,
		Updated: time.Now().UTC(),
	}

	// The first connection attempt with up to TLS 1.2
	grab := NewMxHostGrab(address, ztls.VersionTLS12)

	result.Starttls = grab.starttls
	result.Error = grab.Error

	// Was the TLS handshake successful?
	if result.Starttls != nil && *result.Starttls {
		result.Append(grab)

		// Try TLS 1.0 as well if we just had a higher version
		if grab.tlsVersion > ztls.VersionTLS10 {
			if grab = NewMxHostGrab(address, ztls.VersionTLS10); grab.TLSSuccessful() {
				result.Append(grab)
			}
		}
	}

	// set fingerprints and certificate validity
	if result.certificates != nil {
		result.fingerprints = result.Fingerprints()
		result.validity = NewCertificateValidity(result.certificates)
	}

	return result
}

// Result of a single connection attempt with ZGrab
func NewMxHostGrab(address net.IP, tlsVersion uint16) *MxHostGrab {
	result := &MxHostGrab{}
	var tlsHandshake *ztls.ServerHandshake
	var tlsHello *ztls.ServerHello

	// Create a local copy of the default config
	config := *zlibConfig
	config.TLSVersion = tlsVersion // maximum TLS version

	// Grab the banner
	banner := zlib.GrabBanner(&config, &zlib.GrabTarget{Addr: address})

	// Loop trough the banner log
	for _, entry := range banner.Log {
		data := entry.Data

		switch data := data.(type) {
		case *zlib.TLSHandshakeEvent:
			tlsHandshake = data.GetHandshakeLog()
			tlsHello = tlsHandshake.ServerHello
		case *zlib.StartTLSEvent:
			val := entry.Error == nil
			result.starttls = &val
		}

		if entry.Error != nil {
			// If an error occurs we expect the log entry to be the last
			err := simplifyError(entry.Error).Error()
			result.Error = &err
		}
	}

	// Copy TLS Parameters
	if tlsHello != nil {
		result.tlsVersion = tlsHello.Version
		result.tlsCipherSuite = tlsHello.CipherSuite
	}

	if tlsHandshake != nil {
		// Certificates available?
		if certs := tlsHandshake.ServerCertificates; certs != nil {
			result.certificates = certs.ParsedCertificates
		}

		// Copy Diffie Hellman parameters
		result.dhParams = tlsHandshake.DHParams
	}

	return result
}

// The received certificates
func (summary *MxHostSummary) Fingerprints() [][]byte {

	fingerprints := make([][]byte, len(summary.certificates))
	for i, cert := range summary.certificates {
		fingerprints[i] = []byte(cert.FingerprintSHA1)
	}

	return fingerprints
}

func (summary *MxHostSummary) ServerFingerprint() *[]byte {
	if summary.fingerprints == nil {
		return nil
	}
	return &summary.fingerprints[0]
}

func (summary *MxHostSummary) CaFingerprints() [][]byte {
	if summary.fingerprints == nil {
		return nil
	}
	fingerprints := make([][]byte, len(summary.fingerprints)-1)
	for i, fingerprint := range summary.fingerprints {
		if i > 0 {
			fingerprints[i-1] = fingerprint
		}
	}
	return fingerprints
}

// Appends a MxHostGrab to the MxHostSummary
func (summary *MxHostSummary) Append(grab *MxHostGrab) {
	// Copy certificates
	if summary.certificates == nil {
		summary.certificates = grab.certificates
	}

	if grab.tlsVersion != 0 {
		// Copy TLS parameters
		if summary.tlsVersions == nil {
			summary.tlsVersions = mapset.NewThreadUnsafeSet()
			summary.tlsCipherSuites = mapset.NewThreadUnsafeSet()
		}
		summary.tlsVersions.Add(string(grab.tlsVersion.Bytes()))
		summary.tlsCipherSuites.Add(string(grab.tlsCipherSuite.Bytes()))

		// Copy ECDHE params
		if summary.ecdheCurveType == nil {
			if ecdheParams, ok := grab.dhParams.(*ztls.ECDHEParams); ok {
				summary.ecdheCurveType = &ecdheParams.CurveType
				summary.ecdheCurveId = &ecdheParams.CurveID
				summary.ecdheKeyLength = &ecdheParams.PublicKeyLength
			}
		}
	}
}

// Checks if the certificate is valid for a given domain name
func (summary *MxHostSummary) CertificateValidForDomain(domain string) bool {
	return summary.certificates[0].VerifyHostname(domain) == nil
}

// Was the TLS Handshake successful?
func (result *MxHostGrab) TLSSuccessful() bool {
	return result.certificates != nil
}

var stripErrors = []string{
	"Conversation error",
	"Could not connect",
	"dial tcp",
	"read tcp",
	"write tcp",
}

func simplifyError(err error) error {
	msg := err.Error()
	for _, prefix := range stripErrors {
		if strings.HasPrefix(msg, prefix) {
			if i := strings.LastIndex(msg, ": "); i != -1 {
				return errors.New(msg[i+2 : len(msg)])
			}
		}
	}
	return err
}
