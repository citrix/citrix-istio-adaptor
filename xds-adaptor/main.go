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

package main

import (
	"citrix-xds-adaptor/adsclient"
	"citrix-xds-adaptor/certkeyhandler"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	cpxpwdWaitTime = 45 // 45 seconds to wait for system generated password file creation
)

func getUserName(uFile string) (string, error) {
	userName := "nsroot"
	if uFile != "" {
		user, err := ioutil.ReadFile(uFile)
		if err != nil {
			return "", err
		}
		return string(user), nil
	}
	if os.Getenv("NS_USER") != "" {
		return os.Getenv("NS_USER"), nil
	}
	return userName, nil
}

func getPassword(pFile string) (string, error) {
	password := os.Getenv("NS_PASSWORD")
	if pFile != "" { // Read password from file
		created := false
		if _, err := os.Stat(pFile); err == nil { // If file already exists
			created = true
		} else if created, err = adsclient.IsFileCreated(pFile, cpxpwdWaitTime); err != nil {
			// Citrix ADC CPX 13.0-63.2 onwards, password is auto-generated by system.
			// CPX container creation is slower than xds-adaptor container. Wait for max cpxpwdWaitTime(45) seconds
			return "", err
		}
		if created == true {
			pass, err := ioutil.ReadFile(pFile)
			if err != nil {
				return "", err
			}
			// pass is a []byte
			return string(pass), nil
		}
	}
	// If password file not created, and env variable also not set, then return error.
	// Ensures backward compatability with older CPX versions
	if password == "" {
		return "", fmt.Errorf("password file not created/mounted")
	}
	return password, nil
}

func getVserverIP(vserverIP string) (string, error) {
	if strings.EqualFold(vserverIP, "nsip") {
		return "nsip", nil
	}
	if vserverIP != "" {
		ip := net.ParseIP(vserverIP)
		if ip == nil {
			return "", fmt.Errorf("Not a valid IP address")
		}
		if strings.Contains(vserverIP, ":") {
			return "", fmt.Errorf("Not a valid IPv4 address")
		}
		return vserverIP, nil
	}
	return "", nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	// ADS connection related cli args
	adsServer := flag.String("ads-server", "", "ip/hostname:port of the ads server")
	nodeID := flag.String("ads-client-id", "", "The ads client's node ID")
	secureConnect := flag.Bool("ads-secure-connect", true, "Connect securely to the ads-server")
	adsServerSAN := flag.String("ads-server-SAN", "", "Subject Alternative Name for ads-server ie SPIFFE ID of the ads-server")

	// Application/deployment related cli args
	clusterName := flag.String("application", "", "The name of the application")
	localHostVIP := flag.String("via-ip", "", "An IP address via which application communicates with other applications. Useful in Consul Connect while running as sidecar proxy")
	istioProxyType := flag.String("istio-proxy-type", "", "The type of proxy to connect to Istio pilot's ads-server")

	// Citrix ADC related cli args
	netscalerURL := flag.String("citrix-adc", "http://127.0.0.1", "http(s)://ip|hostname:port location of the Citrix ADC to configure")
	vserverIP := flag.String("citrix-adc-vip", "", "The vserver IP for the MPX/VPX ingress gateway")
	netProfile := flag.String("citrix-adc-net-profile", "",
		"Name of the network profile which is created by Citrix Node Controller (CNC) on the VPX/MPX ingress device. "+
			"This is required to establish connectivity to the pod network from Citrix ADC VPX/MPX")
	userNameCredentialFile := flag.String("citrix-adc-user", "", "Location of file that holds Citrix ADC username")
	passWordCredentialFile := flag.String("citrix-adc-password", "", "Location of file that holds Citrix ADC password")
	analyticsServerIP := flag.String("citrix-adm", "", "Citrix ADM IP to plot the service graph")
	licenseServerIP := flag.String("citrix-license-server", "", "Licensing server IP(usually Citrix ADM IP)")
	logProxyURL := flag.String("coe", "", "Citrix-Observability-Exporter(Logproxy)'s service name")
	adcServerName := flag.String("citrix-adc-server-name", "", "Common Name or SAN used in ADC Nitro certificate")
	adcCA := flag.String("citrix-adc-server-ca", "", "The CA for the server certificate")

	// xds-adaptor related cli args
	version := flag.Bool("version", false, "Print version of the xds-adaptor and exit")

	flag.Parse()

	if *version {
		fmt.Printf("xds-adaptor version: %s %s\n", xdsAdaptorVersion, lastCommitID)
		os.Exit(0)
	}
	if *adsServer == "" {
		fmt.Printf("Required argument 'ads-server' missing\n")
		os.Exit(1)
	}
	userName, err := getUserName(*userNameCredentialFile)
	if err != nil {
		fmt.Printf("Error retrieving citrix-adc username: %v\n", err)
		os.Exit(1)
	}
	passWord, err := getPassword(*passWordCredentialFile)
	if err != nil {
		fmt.Printf("Error retrieving citrix-adc password: %v\n", err)
		os.Exit(1)
	}
	vsvrIP, err := getVserverIP(*vserverIP)
	if err != nil {
		fmt.Printf("Incorrect vserverIP '%s': %v\n", *vserverIP, err)
		os.Exit(1)
	}
	if *istioProxyType != "" {
		*nodeID = *istioProxyType + "~" + os.Getenv("INSTANCE_IP") + "~" + os.Getenv("POD_NAME") + "." + os.Getenv("POD_NAMESPACE") + "~" + os.Getenv("POD_NAMESPACE") + ".svc.cluster.local"
	}
	if *clusterName == "" {
		*clusterName = os.Getenv("APPLICATION_NAME")
	}
	adsinfo := new(adsclient.AdsDetails)
	nsinfo := new(adsclient.NSDetails)
	adsinfo.AdsServerURL = *adsServer
	adsinfo.AdsServerSpiffeID = *adsServerSAN
	adsinfo.SecureConnect = *secureConnect
	adsinfo.NodeID = *nodeID
	adsinfo.ApplicationName = *clusterName
	nsinfo.NetscalerURL = *netscalerURL
	nsinfo.NetscalerUsername = userName
	nsinfo.NetscalerPassword = passWord
	nsinfo.NetscalerVIP = vsvrIP
	nsinfo.NetProfile = *netProfile
	nsinfo.AnalyticsServerIP = *analyticsServerIP
	nsinfo.LicenseServerIP = *licenseServerIP
	nsinfo.LogProxyURL = *logProxyURL
	nsinfo.LocalHostVIP = *localHostVIP
	if *adcServerName != "" {
		nsinfo.SslVerify = true
		nsinfo.RootCAPath = *adcCA
		nsinfo.ServerName = *adcServerName
	}

	var cainfo *certkeyhandler.CADetails
	cainfo = nil
	if os.Getenv("CA_ADDR") != "" {
		cainfo = new(certkeyhandler.CADetails)
		cainfo.CAAddress = os.Getenv("CA_ADDR")
		cainfo.CAProvider = "Istiod"
		cainfo.ClusterID = os.Getenv("CLUSTER_ID")
		cainfo.Env = "onprem"
		cainfo.TrustDomain = os.Getenv("TRUST_DOMAIN")
		cainfo.NameSpace = os.Getenv("POD_NAMESPACE")
		cainfo.SAName = os.Getenv("SERVICE_ACCOUNT")
		val, err := strconv.Atoi(os.Getenv("CERT_TTL_IN_HOURS"))
		if err != nil {
			log.Printf("[ERROR] Certificate expiry value is not provided properly.")
			os.Exit(1)
		}
		cainfo.CertTTL = time.Duration(val) * time.Hour
		/* Remove earlier certficate files provided CSR is enabled */
		/* Wait for connection establishment with xDS server till fresh certificates are not created, else certificates get changed after receiving xDS resources. */
		if _, err := os.Stat(adsclient.ClientCertFile); err == nil {
			os.Remove(adsclient.ClientCertChainFile)
			os.Remove(adsclient.ClientCertFile)
			os.Remove(adsclient.ClientKeyFile)
		}
	}

	discoveryClient, err := adsclient.NewAdsClient(adsinfo, nsinfo, cainfo)
	if err != nil {
		fmt.Printf("Unable to initialize ADS client: %v\n", err)
		os.Exit(1)
	}
	log.Printf("[INFO] xds-adaptor version: %s %s", xdsAdaptorVersion, lastCommitID)
	discoveryClient.StartClient()
	<-make(chan int)
}
