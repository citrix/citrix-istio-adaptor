/*
Copyright 2020 Citrix Systems, Inc
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package adsclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"

	"path/filepath"

	"github.com/fsnotify/fsnotify"
	"github.com/spiffe/go-spiffe/spiffe"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"log"
	"net"
	"os"
	"time"
)

const (
	// CAcertFile is the location where CA certificate is stored
	CAcertFile = "/etc/rootcert/root-cert.pem"
	//ClientCertChainFile is location of certificate-chain
	ClientCertChainFile = "/etc/certs/cert-chain.pem"
	// ClientCertFile is location of client's certificate
	ClientCertFile = "/etc/certs/cert.pem"
	// ClientKeyFile is location of private key of client
	ClientKeyFile   = "/etc/certs/key.pem"
	rsaKeySize      = 2048
	orgName         = "Citrix Systems"
	certGenWaittime = 1 * time.Minute
)

// TLSPeer structure holds information about SPIFFE IDs
// which needs to be matched. This slice of IDs belong to
// the peer which needs to be mutually authenticated.
// TrustRoots is the certificate pool of CA certificates
// used for verification of other end.
type TLSPeer struct {
	// Slice of permitted SPIFFE IDs
	SpiffeIDs  []string
	TrustRoots *x509.CertPool
}

// IsFileCreated check if fileName is created within expiryTimeinSec duration or not
func IsFileCreated(fileName string, expiryTimeinSec time.Duration) (bool, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()
	dirName := filepath.Dir(fileName)
	err = watcher.Add(dirName)
	if err != nil {
		// If this is failing then mostly /var/deviceinfo is not mounted to xDS-adaptor container
		log.Printf("[DEBUG] Error: %s\n", err.Error())
		return false, fmt.Errorf("Directory %s does not seem to be mounted", dirName)
	}

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				log.Printf("[DEBUG] Watcher could not capture event")
				return false, nil
			}
			log.Println("[DEBUG] event:", event)
			if event.Op&fsnotify.Write == fsnotify.Write {
				if event.Name == fileName {
					log.Println("[DEBUG] File written: ", event.Name)
					return true, nil
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				log.Printf("[DEBUG] Watcher could not capture event")
				return false, nil
			}
			log.Printf("[DEBUG] IsFileCreated error: %s\n", err.Error())
		case <-time.After(expiryTimeinSec * time.Second):
			log.Printf("[DEBUG] %s file not created within %v seconds\n", fileName, expiryTimeinSec)
			return false, nil
		}
	}
}

// verifyPeerCertificate serves callbacks from TLS listeners/dialers. It performs
// SPIFFE-specific validation steps on behalf of the golang TLS library
func (t *TLSPeer) verifyPeerCertificate(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) (err error) {
	// First, parse all received certs
	var certs []*x509.Certificate
	for _, rawCert := range rawCerts {
		cert, err := x509.ParseCertificate(rawCert)
		if err != nil {
			log.Printf("[ERROR]: Err: %v, Could not parse certificate %v.", err, cert)
			return err
		}

		certs = append(certs, cert)
	}
	// Perform path validation
	// Leaf is the first off the wire:
	// https://tools.ietf.org/html/rfc5246#section-7.4.2
	intermediates := x509.NewCertPool()
	for _, intermediate := range certs[1:] {
		intermediates.AddCert(intermediate)
	}
	err = spiffe.VerifyCertificate(certs[0], intermediates, t.TrustRoots)
	if err != nil {
		log.Printf("[ERROR]: Certificate Verification failed! %v", err)
		return err
	}

	// Look for a known SPIFFE ID in the leaf
	err = spiffe.MatchID(t.SpiffeIDs, certs[0])
	if err != nil {
		log.Printf("[ERROR]: %v", err)
		return err
	}
	log.Printf("[TRACE]: SVID match successful!")

	// If we are here, then all is well
	return nil
}

func (t *TLSPeer) getTLSCredentials(clientCertFile, clientKeyFile string) (credentials.TransportCredentials, error) {
	var tlsConf tls.Config

	if clientCertFile != "" {
		// Load the client certificates from disk
		cert, err := tls.LoadX509KeyPair(clientCertFile, clientKeyFile)
		if err != nil {
			log.Printf("[ERROR]: Could not load client key pair: %v", err)
			return nil, err
		}

		tlsConf.Certificates = []tls.Certificate{cert}
	}
	// Setting this to true to avoid server certificate verification.
	// We are going to perform custom verification if SPIFFE ID is provided as SAN.
	// VerifyPeerCertificate, if not nil, is called after normal
	// certificate verification by either a TLS client or server. It
	// receives the raw ASN.1 certificates provided by the peer and also
	// any verified chains that normal processing found. If it returns a
	// non-nil error, the handshake is aborted and that error results.
	//
	// If normal verification fails then the handshake will abort before
	// considering this callback. If normal verification is disabled by
	// setting InsecureSkipVerify, or (for a server) when ClientAuth is
	// RequestClientCert or RequireAnyClientCert, then this callback will
	// be considered but the verifiedChains argument will always be nil.
	if len(t.SpiffeIDs) > 0 {
		tlsConf.InsecureSkipVerify = true
		tlsConf.VerifyPeerCertificate = t.verifyPeerCertificate
	} else {
		tlsConf.InsecureSkipVerify = false
		tlsConf.VerifyPeerCertificate = nil
	}

	// RootCAs defines the set of root certificate authorities
	// that clients use when verifying server certificates.
	// If RootCAs is nil, TLS uses the host's root CA set.
	// Server (in this case ads-server) uses the field ClientCAs.
	tlsConf.RootCAs = t.TrustRoots

	return credentials.NewTLS(&tlsConf), nil
}

func getRootCAs(cacertFile string) (*x509.CertPool, error) {

	if cacertFile == "" {
		log.Printf("[TRACE]: No CA Certificate!")
		return nil, nil
	}

	// Create a certificate pool from the certificate authority
	caCert, err := ioutil.ReadFile(cacertFile)
	if err != nil {
		log.Printf("[ERROR]: Could not read Root CA certificate. Err=%s", err)
		return nil, err
	}
	caCertPool := x509.NewCertPool()

	// Append the certificates from the CA
	if ok := caCertPool.AppendCertsFromPEM(caCert); !ok {
		log.Printf("[ERROR]: Could not Append CA certificate. ")
		return nil, errors.New("failed to append ca certs")
	}
	return caCertPool, nil
}

func checkCertfileCreation(certfile string, waitTime time.Duration) error {
	if _, err := os.Stat(certfile); err != nil { // If file does not exist
		if created, err := IsFileCreated(certfile, waitTime); err != nil {
			return err
		} else if created == false {
			return fmt.Errorf("Certificate and Key files not present")
		}
	}
	return nil
}

func insecureConnectToServer(address string, waitForCerts bool) (*grpc.ClientConn, error) {
	// Wait for certificate generation
	if waitForCerts == true {
		if err := checkCertfileCreation(ClientCertFile, certGenWaittime); err != nil {
			log.Printf("[ERROR] Client's certificate not created. %s", err)
			return nil, err
		}
	}
	log.Printf("[INFO] grpc Insecure dialling to %s.", address)

	dialer := func(address string, timeout time.Duration) (net.Conn, error) {
		return net.DialTimeout("tcp", address, time.Duration(60)*time.Second)
	}

	conn, err := grpc.DialContext(context.Background(),
		address,
		grpc.WithDialer(dialer),
		grpc.FailOnNonTempDialError(true),
		grpc.WithBlock(),
		grpc.WithTimeout(60*time.Second),
		grpc.WithInsecure())

	if err != nil {
		log.Printf("[ERROR] grpc Connect failed with : %v", err)
		return nil, err
	}

	return conn, nil
}

func secureConnectToServer(address, spiffeID string) (*grpc.ClientConn, error) {
	log.Printf("[INFO] grpc Secure Dialling to %s.", address)
	if len(spiffeID) > 0 {
		log.Printf("[INFO] SPIFFE ID %s must be matched", spiffeID)
	}

	RootCAs, err := getRootCAs(CAcertFile)
	if err != nil {
		log.Printf("[ERROR]: Problem in retrieving Root certificate from path %s. Err: %v", CAcertFile, err)
		return nil, err
	}

	var spiffeids []string
	if len(spiffeID) > 0 {
		spiffeids = []string{spiffeID}
	} else {
		spiffeids = make([]string, 0)
	}
	adsServer := &TLSPeer{
		SpiffeIDs:  spiffeids,
		TrustRoots: RootCAs,
	}
	// Check if files ClientCertFile and ClientKeyFile are created or not
	if err := checkCertfileCreation(ClientCertFile, certGenWaittime); err != nil {
		log.Printf("[ERROR] Client's certificate not created. %s", err)
		return nil, err
	}

	creds, err := adsServer.getTLSCredentials(ClientCertFile, ClientKeyFile)
	if err != nil {
		log.Printf("[ERROR]: Could not construct Client transport credentials. Err=%s", err)
		return nil, err
	}

	dialer := func(address string, timeout time.Duration) (net.Conn, error) {
		return net.DialTimeout("tcp", address, time.Duration(60)*time.Second)
	}

	conn, err := grpc.DialContext(context.Background(),
		address,
		grpc.WithDialer(dialer),
		grpc.FailOnNonTempDialError(true),
		grpc.WithBlock(),
		grpc.WithTimeout(60*time.Second),
		grpc.WithTransportCredentials(creds))

	if err != nil {
		log.Printf("[ERROR] grpc Connect failed with : %v. Ensure ADS Server's secure grpc port is mentioned", err)
		return nil, err
	}
	return conn, nil
}
