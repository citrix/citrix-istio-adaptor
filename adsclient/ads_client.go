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
	"citrix-xds-adaptor/certkeyhandler"
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	listener "github.com/envoyproxy/go-control-plane/envoy/config/listener/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	ads "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	resource "github.com/envoyproxy/go-control-plane/pkg/resource/v3"
	"github.com/golang/protobuf/ptypes"
	_struct "github.com/golang/protobuf/ptypes/struct"
	"github.com/hashicorp/go-hclog"
	"google.golang.org/grpc"
)

const (
	cdsURL = resource.ClusterType  //"type.googleapis.com/envoy.config.cluster.v3.Cluster"
	ldsURL = resource.ListenerType //"type.googleapis.com/envoy.config.listener.v3.Listener"
	edsURL = resource.EndpointType //"type.googleapis.com/envoy.config.endpoint.v3.ClusterLoadAssignment"
	rdsURL = resource.RouteType    //"type.googleapis.com/envoy.config.route.v3.RouteConfiguration"
)

var (
	xDSServerPort       int
	xDSServerURL        string
	xDSServerResolvedIP string
	xDSLogger           hclog.Logger
)

type cdsAddHandlerType func(*configAdaptor, *cluster.Cluster, interface{}) string
type cdsDelHandlerType func(*configAdaptor, string)
type edsAddHandlerType func(*configAdaptor, *endpoint.ClusterLoadAssignment, interface{})
type ldsAddHandlerType func(*configAdaptor, *listener.Listener) []map[string]interface{}
type ldsDelHandlerType func(*configAdaptor, string, []string)
type rdsAddHandlerType func(*configAdaptor, []*route.RouteConfiguration, interface{}) map[string]interface{}

//AdsDetails will define the members which will be read up at bootup time
type AdsDetails struct {
	AdsServerURL      string
	AdsServerSpiffeID string
	SecureConnect     bool
	NodeID            string
	ApplicationName   string
}

//NSDetails will define the members which will be read up at bootup time
type NSDetails struct {
	NetscalerURL      string
	NetscalerUsername string
	NetscalerPassword string
	NetscalerVIP      string
	NetProfile        string
	AnalyticsServerIP string
	LicenseServerIP   string
	LogProxyURL       string
	SslVerify         bool
	RootCAPath        string
	ServerName        string
	adsServerURL      string
	adsServerPort     string
	LocalHostVIP      string
	caServerPort      string
	bootStrapConfReqd bool //Very first time when xDS-adaptor comes up or every time ADC restarts, some init config is needed on ADC
}

type apiRequest struct {
	typeURL     string
	versionInfo string
	nonce       string
	resources   map[string]interface{}
	/*
		ldsURL -> [csVsName]
		rdsURL -> lds Name, CsVsName, serviceType
		cdsURL -> serviceType
		edsURL -> cds Name
	*/
	handler func(*AdsClient, *discovery.DiscoveryResponse)
}

// AdsClient is a client to an Aggregated Discovery Service
type AdsClient struct {
	nsInfo             *NSDetails
	adsServerURL       string
	adsServerSpiffeID  string
	secureConnect      bool
	nodeID             *core.Node
	apiRequests        map[string]*apiRequest
	connection         *grpc.ClientConn
	connectionMux      sync.Mutex
	stream             grpc.ClientStream
	quit               chan int
	nsConfigAdaptor    *configAdaptor
	nsConfigAdaptorMux sync.Mutex
	cdsAddHandler      cdsAddHandlerType
	cdsDelHandler      cdsDelHandlerType
	edsAddHandler      edsAddHandlerType
	ldsAddHandler      ldsAddHandlerType
	ldsDelHandler      ldsDelHandlerType
	rdsAddHandler      rdsAddHandlerType
	caInfo             *certkeyhandler.CADetails
	ckHandler          *certkeyhandler.CertKeyHandler
	ckHandlerMux       sync.Mutex
}

func init() {
	/* Create a logger */
	level := hclog.LevelFromString("DEBUG") // Default value
	logLevel, ok := os.LookupEnv("LOGLEVEL")
	if ok {
		lvl := hclog.LevelFromString(logLevel)
		if lvl != hclog.NoLevel {
			level = lvl
		} else {
			log.Printf("xDS-adaptor: LOGLEVEL not set to a valid log level (%s), defaulting to %d", logLevel, level)
		}
	}
	_, jsonLog := os.LookupEnv("JSONLOG")
	xDSLogger = hclog.New(&hclog.LoggerOptions{
		Name:            "xDS-Adaptor",
		Level:           level,
		Color:           hclog.AutoColor,
		JSONFormat:      jsonLog,
		IncludeLocation: true,
	})
	log.Printf("[INFO] adsclient logger created with loglevel = %s and jsonLog = %v", level, jsonLog)
}

func (client *AdsClient) writeADSRequest(req *apiRequest) {
	var resourceNames []string
	if req.typeURL == edsURL || req.typeURL == rdsURL {
		resourceNames = make([]string, len(req.resources))
		i := 0
		for k := range req.resources {
			resourceNames[i] = k
			i++
		}
	}
	msg := &discovery.DiscoveryRequest{TypeUrl: req.typeURL, Node: client.nodeID, VersionInfo: req.versionInfo, ResponseNonce: req.nonce, ResourceNames: resourceNames}
	if err := client.stream.SendMsg(msg); err != nil {
		xDSLogger.Error("writeADSRequest: Failed to send a message", "err", err)
	} else {
		xDSLogger.Trace("writeADSRequest: Wrote req message", "version", msg.VersionInfo, "nonce", msg.ResponseNonce, "type", msg.TypeUrl)
	}
}

func (client *AdsClient) callRequestHandler(msg *discovery.DiscoveryResponse) error {
	if client.apiRequests[msg.TypeUrl].handler != nil {
		client.nsConfigAdaptorMux.Lock()
		defer client.nsConfigAdaptorMux.Unlock()
		if client.nsConfigAdaptor != nil {
			client.apiRequests[msg.TypeUrl].handler(client, msg)
		} else {
			return fmt.Errorf("ADS client has no config-adaptor")
		}
	}
	return nil
}

func (client *AdsClient) readADSResponse() {
	for {
		m := new(discovery.DiscoveryResponse)
		if err := client.stream.RecvMsg(m); err != nil {
			xDSLogger.Error("readADSResponse: Failed to recv a message", "error", err)
			time.Sleep(2 * time.Second)
			return
		}
		xDSLogger.Trace("readADSResponse: Received a message", "version", m.VersionInfo, "type", m.TypeUrl, "resourceCount", len(m.Resources))
		if err := client.callRequestHandler(m); err != nil {
			xDSLogger.Error("readADSResponse: Request handler returned error", "error", err)
			return
		}
		client.apiRequests[m.TypeUrl].versionInfo = m.VersionInfo
		client.apiRequests[m.TypeUrl].nonce = m.Nonce
		client.writeADSRequest(client.apiRequests[m.TypeUrl])
	}
}

func cdsHandler(client *AdsClient, m *discovery.DiscoveryResponse) {
	clusterNames := make(map[string]bool)
	edsResources := make(map[string]interface{})
	requestEds := false
	cdsResource := &cluster.Cluster{}
	for _, resource := range m.Resources {
		if err := ptypes.UnmarshalAny(resource, cdsResource); err != nil {
			xDSLogger.Trace("cdsHandler: Could not find Unmarshal resources in CDS Handler")
			continue
		}
		clusterNames[cdsResource.Name] = true
		edsName := ""
		if _, ok := client.apiRequests[cdsURL].resources[cdsResource.Name]; ok {
			edsName = client.cdsAddHandler(client.nsConfigAdaptor, cdsResource, client.apiRequests[cdsURL].resources[cdsResource.Name])
		} else if multiClusterIngress {
			edsName = client.cdsAddHandler(client.nsConfigAdaptor, cdsResource, "HTTP")
		}
		if edsName != "" {
			edsResources[edsName] = cdsResource.Name
			if _, ok := client.apiRequests[edsURL].resources[edsName]; !ok {
				requestEds = true
			}
		}
	}
	for clusterName := range client.apiRequests[cdsURL].resources {
		if _, ok := clusterNames[clusterName]; !ok {
			client.cdsDelHandler(client.nsConfigAdaptor, clusterName)
		}
	}
	client.apiRequests[edsURL].resources = edsResources

	if requestEds == true {
		client.writeADSRequest(client.apiRequests[edsURL])
	}
}

func ldsHandler(client *AdsClient, m *discovery.DiscoveryResponse) {
	rdsResources := make(map[string]interface{})
	ldsResources := make(map[string]interface{})
	requestRds := false
	requestCds := false
	ldsResource := &listener.Listener{}
	for _, resource := range m.Resources {
		if err := ptypes.UnmarshalAny(resource, ldsResource); err != nil {
			xDSLogger.Trace("ldsHandler: Could not find Unmarshal resources in LDS handler")
			continue
		}
		ldsResources[ldsResource.Name] = make([]string, 0)
		dependentResourcesList := client.ldsAddHandler(client.nsConfigAdaptor, ldsResource)
		for _, dependentResources := range dependentResourcesList {
			for _, rdsConfigName := range dependentResources["rdsNames"].([]string) {
				rdsResources[rdsConfigName] = dependentResources
				if _, ok := client.apiRequests[rdsURL].resources[rdsConfigName]; !ok {
					requestRds = true
				}
			}
			for _, cdsConfigName := range dependentResources["cdsNames"].([]string) {
				if _, ok := client.apiRequests[cdsURL].resources[cdsConfigName]; !ok {
					requestCds = true
					client.apiRequests[cdsURL].resources[cdsConfigName] = dependentResources["serviceType"]
				}
			}
			if dependentResources["csVsName"].(string) != "" {
				ldsResources[ldsResource.Name] = append(ldsResources[ldsResource.Name].([]string), dependentResources["csVsName"].(string))
			}
		}
	}
	for ldsResourceName := range client.apiRequests[ldsURL].resources {
		if _, ok := ldsResources[ldsResourceName]; !ok {
			client.ldsDelHandler(client.nsConfigAdaptor, ldsResourceName, client.apiRequests[ldsURL].resources[ldsResourceName].([]string))
		}
	}

	client.apiRequests[ldsURL].resources = ldsResources
	client.apiRequests[rdsURL].resources = rdsResources

	if requestRds == true {
		client.writeADSRequest(client.apiRequests[rdsURL])
	}
	if requestCds == true {
		client.reloadCds()
	}
}

func edsHandler(client *AdsClient, m *discovery.DiscoveryResponse) {
	edsResource := &endpoint.ClusterLoadAssignment{}
	for _, resource := range m.Resources {
		if err := ptypes.UnmarshalAny(resource, edsResource); err != nil {
			xDSLogger.Trace("edsHandler: Could not find Unmarshal resources in EDS handler")
			continue
		}
		if _, ok := client.apiRequests[edsURL].resources[edsResource.GetClusterName()]; !ok {
			xDSLogger.Error("edsHandler: Received unsubscribed EDS resource. Ignoring", "edsName", edsResource.GetClusterName())
			continue
		}
		client.edsAddHandler(client.nsConfigAdaptor, edsResource, client.apiRequests[edsURL].resources[edsResource.GetClusterName()])
	}
}

func rdsHandler(client *AdsClient, m *discovery.DiscoveryResponse) {
	requestCds := false
	rdsToLds := make(map[string][]*route.RouteConfiguration)
	for _, resource := range m.Resources {
		rdsResource := &route.RouteConfiguration{}
		if err := ptypes.UnmarshalAny(resource, rdsResource); err != nil {
			continue
		}
		if _, ok := client.apiRequests[rdsURL].resources[rdsResource.GetName()]; !ok {
			xDSLogger.Error("rdsHandler: Received unsubscribed RDS resource. Ignoring", "rdsName", rdsResource.GetName())
			continue
		}
		listenerName := client.apiRequests[rdsURL].resources[rdsResource.GetName()].(map[string]interface{})["listenerName"].(string)
		if _, ok := rdsToLds[listenerName]; !ok {
			rdsToLds[listenerName] = make([]*route.RouteConfiguration, 0)
		}
		rdsToLds[listenerName] = append(rdsToLds[listenerName], rdsResource)
	}
	xDSLogger.Trace("rdsHandler: rdsToLds details", "rdsToLds", rdsToLds)
	for _, rdsArray := range rdsToLds {
		dependentClusters := client.rdsAddHandler(client.nsConfigAdaptor, rdsArray, client.apiRequests[rdsURL].resources[rdsArray[0].GetName()])
		for _, clusterName := range dependentClusters["cdsNames"].([]string) {
			if _, ok := client.apiRequests[cdsURL].resources[clusterName]; !ok {
				requestCds = true
				client.apiRequests[cdsURL].resources[clusterName] = dependentClusters["serviceType"]
			}
		}
	}
	if requestCds == true {
		client.reloadCds()
	}
}

func (client *AdsClient) reloadCds() {
	if client.apiRequests[cdsURL].nonce == "" {
		client.writeADSRequest(client.apiRequests[cdsURL])
		return
	}
	client.connectionMux.Lock()
	adsClient := ads.NewAggregatedDiscoveryServiceClient(client.connection)
	client.connectionMux.Unlock()
	stream, err := adsClient.StreamAggregatedResources(context.Background())
	if err != nil {
		xDSLogger.Error("reloadCds: Create stream failed", "error", err)
		return
	}
	if err = stream.Send(&discovery.DiscoveryRequest{TypeUrl: cdsURL, Node: client.nodeID}); err != nil {
		xDSLogger.Error("reloadCds: Send request failed", "error", err)
	} else {
		res, err := stream.Recv()
		if err != nil {
			xDSLogger.Error("reloadCds: Recv failed", "error", err)
		} else {
			xDSLogger.Trace("reloadCds: Received message")
			cdsHandler(client, res)
		}
	}
	stream.CloseSend()
}

//NewAdsClient returns a new Aggregated Discovery Service client
func NewAdsClient(adsinfo *AdsDetails, nsinfo *NSDetails, cainfo *certkeyhandler.CADetails) (*AdsClient, error) {
	adsClient := new(AdsClient)
	adsClient.adsServerURL = adsinfo.AdsServerURL
	adsClient.adsServerSpiffeID = adsinfo.AdsServerSpiffeID
	adsClient.secureConnect = adsinfo.SecureConnect
	metadata := _struct.Struct{
		Fields: map[string]*_struct.Value{
			"CLUSTER_ID":       {Kind: &_struct.Value_StringValue{StringValue: os.Getenv("CLUSTER_ID")}},
			"CONFIG_NAMESPACE": {Kind: &_struct.Value_StringValue{StringValue: os.Getenv("POD_NAMESPACE")}},
			"MESH_ID":          {Kind: &_struct.Value_StringValue{StringValue: os.Getenv("TRUST_DOMAIN")}},
			"NAME":             {Kind: &_struct.Value_StringValue{StringValue: os.Getenv("HOSTNAME")}},
			"NAMESPACE":        {Kind: &_struct.Value_StringValue{StringValue: os.Getenv("POD_NAMESPACE")}},
			"SDS":              {Kind: &_struct.Value_StringValue{StringValue: "true"}},
			"SERVICE_ACCOUNT":  {Kind: &_struct.Value_StringValue{StringValue: os.Getenv("SERVICE_ACCOUNT")}},
			"TRUSTJWT":         {Kind: &_struct.Value_StringValue{StringValue: "true"}},
		},
	}
	adsClient.nodeID = &core.Node{Id: adsinfo.NodeID, Cluster: adsinfo.ApplicationName, Metadata: &metadata}
	xDSLogger.Info("NewAdsClient: Node details ", "nodeID", adsClient.nodeID)
	adsClient.quit = make(chan int)
	adsClient.cdsAddHandler = clusterAdd
	adsClient.cdsDelHandler = clusterDel
	adsClient.ldsAddHandler = listenerAdd
	adsClient.ldsDelHandler = listenerDel
	adsClient.edsAddHandler = clusterEndpointUpdate
	adsClient.rdsAddHandler = routeUpdate
	s := strings.Split(adsinfo.AdsServerURL, ":")
	xDSServerURL = s[0]
	nsinfo.adsServerPort = "unknown"
	if len(s) > 1 {
		nsinfo.adsServerPort = s[1]
		xDSServerPort, _ = strconv.Atoi(s[1])
	}
	if cainfo != nil {
		s = strings.Split(cainfo.CAAddress, ":")
		if len(s) > 1 {
			nsinfo.caServerPort = s[1]
		}
	}
	nsinfo.bootStrapConfReqd = true
	adsClient.nsInfo = nsinfo
	adsClient.caInfo = cainfo
	return adsClient, nil
}

// SetLogLevel function sets the log level of adsclient package
func (client *AdsClient) SetLogLevel(level string) {
	xDSLogger.SetLevel(hclog.LevelFromString(level))
}

// GetNodeID returns the node ID of the client
func (client *AdsClient) GetNodeID() *core.Node {
	return client.nodeID
}

func (client *AdsClient) startCertKeyHandler(errCh chan<- error) error {
	if client.caInfo == nil {
		xDSLogger.Info("startCertKeyHandler: CA details are not specified. Not creating certificate key handler.")
		return nil
	}
	certinfo := new(certkeyhandler.CertDetails)
	certinfo.RootCertFile = CAcertFile
	certinfo.CertChainFile = ClientCertChainFile
	certinfo.CertFile = ClientCertFile
	certinfo.KeyFile = ClientKeyFile
	certinfo.RSAKeySize = rsaKeySize
	certinfo.Org = orgName
	certkeyhdlr, err := certkeyhandler.NewCertKeyHandler(client.caInfo, certinfo)
	if err != nil {
		xDSLogger.Error("startCertKeyHandler: Could not create certkey handler", "error", err.Error())
		return err
	}
	client.ckHandlerMux.Lock()
	client.ckHandler = certkeyhdlr
	client.ckHandlerMux.Unlock()
	go certkeyhdlr.StartHandler(errCh)
	return nil
}

// StartClient starts connecting and listening to the ADS server
func (client *AdsClient) StartClient() {
	var err error
	xDSLogger.Trace("Starting ADS client")
	go func() {
		ckHandlerStarted := false
		ckhErrCh := make(chan error)
		for {
			select {
			case <-client.quit:
				xDSLogger.Trace("Stopping ADS client")
				return
			case ckherr := <-ckhErrCh:
				if ckherr != nil {
					xDSLogger.Error("StartClient: Certificate Key Handler Problem", "ckherr", ckherr.Error())
					ckHandlerStarted = false
					client.ckHandlerMux.Lock()
					client.ckHandler = nil
					client.ckHandlerMux.Unlock()
					// Start handler again
					if err := client.startCertKeyHandler(ckhErrCh); err != nil {
						xDSLogger.Error("StartClient: Could not start certificate key handler", "error", err.Error())
						return
					}
					ckHandlerStarted = true
				}
			default:
				err = client.assignConfigAdaptor()
				if err != nil {
					continue
				}
				if client.caInfo != nil && ckHandlerStarted == false {
					if err := client.startCertKeyHandler(ckhErrCh); err != nil {
						xDSLogger.Error("StartClient: Could not start certificate key handler", "error", err.Error())
						return
					}
					ckHandlerStarted = true
				}
				client.connectionMux.Lock()
				if client.secureConnect == true {
					client.connection, err = secureConnectToServer(client.adsServerURL, client.adsServerSpiffeID, ckHandlerStarted)
				} else {
					client.connection, err = insecureConnectToServer(client.adsServerURL, ckHandlerStarted)
				}
				if err != nil {
					xDSLogger.Trace("StartClient: Connection to grpc server failed", "error", err)
					client.connectionMux.Unlock()
					time.Sleep(1 * time.Second)
					continue
				}
				adsClient := ads.NewAggregatedDiscoveryServiceClient(client.connection)
				client.connectionMux.Unlock()
				client.stream, err = adsClient.StreamAggregatedResources(context.Background())
				if err != nil {
					xDSLogger.Error("StartClient: grpc new stream creation failed", "error", err)
					continue
				}
				client.apiRequests = map[string]*apiRequest{
					cdsURL: &apiRequest{typeURL: cdsURL, handler: cdsHandler, resources: make(map[string]interface{})},
					ldsURL: &apiRequest{typeURL: ldsURL, handler: ldsHandler, resources: make(map[string]interface{})},
					edsURL: &apiRequest{typeURL: edsURL, handler: edsHandler, resources: make(map[string]interface{})},
					rdsURL: &apiRequest{typeURL: rdsURL, handler: rdsHandler, resources: make(map[string]interface{})},
				}
				client.writeADSRequest(client.apiRequests[ldsURL])
				if multiClusterIngress {
					client.writeADSRequest(client.apiRequests[cdsURL])
				}
				client.readADSResponse()
				client.stopClientConnection(true)
			}
		}
	}()
}

func (client *AdsClient) assignConfigAdaptor() error {
	var err error
	client.nsConfigAdaptorMux.Lock()
	defer client.nsConfigAdaptorMux.Unlock()
	if client.nsConfigAdaptor == nil {
		client.nsConfigAdaptor, err = newConfigAdaptor(client.nsInfo)
		if err != nil {
			xDSLogger.Error("assignConfigAdaptor: Could not create nsConfigAdaptor", "error", err.Error())
			return err
		}
		client.nsConfigAdaptor.startConfigAdaptor(client)
	}
	return nil
}

func (client *AdsClient) releaseConfigAdaptor(shouldStopConfigAdaptor bool) {
	client.nsConfigAdaptorMux.Lock()
	defer client.nsConfigAdaptorMux.Unlock()
	if client.nsConfigAdaptor != nil {
		client.nsConfigAdaptor.client.Logout() // TODO: Might need to move to other file
		if shouldStopConfigAdaptor == true {
			client.nsConfigAdaptor.stopConfigAdaptor()
		}
		client.nsConfigAdaptor = nil
	}
}

func (client *AdsClient) stopClientConnection(shouldStopConfigAdaptor bool) {
	/* Close grpc stream first */
	if client.stream != nil {
		err := client.stream.CloseSend()
		if err != nil {
			xDSLogger.Error("stopClientConnection: Error in closing client stream", "error", err.Error())
		} else {
			xDSLogger.Trace("stopClientConnection: Successfully closed client stream")
		}
		client.stream = nil
	}
	client.connectionMux.Lock()
	if client.connection != nil {
		err := client.connection.Close()
		if err != nil {
			xDSLogger.Error("stopClientConnection: gRPC connection closing error", "error", err.Error())
		}
		client.connection = nil
	}
	client.connectionMux.Unlock()
	client.releaseConfigAdaptor(shouldStopConfigAdaptor)
	xDSLogger.Trace("stopClientConnection: closed client connection")
}

// StopClient closes the connection to the ADS server
func (client *AdsClient) StopClient() {
	client.stopClientConnection(true)
	client.ckHandlerMux.Lock()
	if client.ckHandler != nil {
		client.ckHandler.StopHandler()
	}
	client.ckHandlerMux.Unlock()
	client.quit <- 1
	xDSLogger.Trace("Stopped adsClient")
}
