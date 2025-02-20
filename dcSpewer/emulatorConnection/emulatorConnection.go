package emulatorConnection

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"reflect"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/config"
	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/inventory"
	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/plugins"
	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/storage"
	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/utils"
	adapp_stim_base "bitbucket-eng-sjc1.cisco.com/an/barcelona/adapp/stim/base"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adconnector"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adio"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adlog"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adsec"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adutil"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// Process registers and connects "emulated" device connector connections with a cloud instance
// Usage: EMU_CLOUD= <cloud dc endpoint> EMU_CONNECTIONS=<numberOfConnections> ./spewer
//
// E.g. EMU_CLOUD=qaconnect.starshipcloud.com EMU_CONNECTIONS=2 ./spewer
//
// Connections will be auto-disconnected at randome intervals between "rate of disconnect" and "rate of disconnect" * 2
// To disable auto-disconnect provide a value of 0 for "rate of disconnect"

// The cloud to register and connect
var cloud string

// Number of connections to spawn
var connections int

// Shared client for all connections
var c *http.Client

// Shared websocket dialer
var dialer *websocket.Dialer

// Cache of all created connections
var emulators = make(map[string]*deviceConnector)
var emulatorsMu sync.RWMutex

// Rate at which connections should break their connection to Intersight.
// If zero no disconnects will be made.
var disconnectRate time.Duration

// Time between connection interruption connections will wait before re-connecting.
// If zero the connection will wait 10 seconds or the sleep time the cloud requests
// if honorServerRequests is true.
var reconnectRate time.Duration

// Marks whether to honor and follow cloud instructions i.r.t re-connect rate.
var honorServerRequests = true

// Rate at which connections are created to register to Intersight
var spawnRate time.Duration

// Global logger
var glogger *zap.Logger

const (
	leaderPrimary   = "Primary"
	leaderSecondary = "Secondary"
	connectedStatus = "Connected"
)

// DC Emulator Statuses
const (
	emulatorStatusCreated  = 0
	emulatorStatusStarting = 1
	emulatorStatusStarted  = 2
)

// Overall limiter for messages read off websockets.
var readLimiter chan struct{}

const apiVersion = 8

func init() {
	// Log to stdout
	ferr := flag.Set("log_path", "")
	if ferr != nil {
		panic(ferr)
	}
	glogger = adlog.InitSimpleLogger(10 << 10 << 10)

	// Retrieve the configuration for the emulators from the environment.
	disconnectRateRaw := os.Getenv("EMU_DISCONNECT_RATE")

	if disconnectRateRaw != "" {
		disconnectRateSec, err := strconv.Atoi(disconnectRateRaw)
		if err != nil {
			utils.SetErrorState(fmt.Sprintf("failed to parse disconnect rate: %s", err.Error()))
			return
		}

		disconnectRate = time.Millisecond * time.Duration(disconnectRateSec)
	}

	reconnectRateRaw := os.Getenv("RECONNECT_RATE")

	if reconnectRateRaw != "" {
		reconnectRateSec, err := strconv.Atoi(reconnectRateRaw)
		if err != nil {
			utils.SetErrorState(fmt.Sprintf("failed to parse reconnect rate: %s", err.Error()))
			return
		}

		reconnectRate = time.Millisecond * time.Duration(reconnectRateSec)
	}

	if reconnectRate == 0 {
		reconnectRate = 10 * time.Second
	}

	spawnRateRaw := os.Getenv("SPAWN_RATE")

	if spawnRateRaw != "" {
		spawnRateSec, err := strconv.Atoi(spawnRateRaw)
		if err != nil {
			utils.SetErrorState(fmt.Sprintf("failed to parse spawn rate: %s", err.Error()))
			return
		}

		spawnRate = time.Millisecond * time.Duration(spawnRateSec)
	}

	if spawnRate == 0 {
		spawnRate = 1 * time.Second
	}

	honorServer := os.Getenv("HONOR_SERVER_BACKOFF")
	if honorServer == "false" {
		honorServerRequests = false
	}

	// Setup HTTPS client
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true, // #nosec
	} // #nosec

	d := net.Dialer{DualStack: true}
	dialer = &websocket.Dialer{
		TLSClientConfig:  tlsConfig,
		HandshakeTimeout: 30 * time.Second,
		// Override websocket net dial func to wrap the collection for recording network stats
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := d.DialContext(ctx, network, addr)
			return c, err
		},
	}

	var proxyUrl *url.URL
	proxyEnv := os.Getenv("DC_PROXY")
	if proxyEnv != "" {
		var perr error
		proxyUrl, perr = url.Parse(proxyEnv)
		if perr != nil {
			panic("invalid dc proxy url : " + proxyEnv)
		}
	}

	if proxyUrl != nil {
		dialer.Proxy = func(r *http.Request) (*url.URL, error) {
			return proxyUrl, nil
		}
	}

	c = &http.Client{Timeout: time.Second * 20}

	c.Transport = &http.Transport{
		TLSClientConfig: tlsConfig}

	httpt := &http.Transport{
		TLSClientConfig:       tlsConfig,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DialContext:           d.DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
	}

	if proxyUrl != nil {
		httpt.Proxy = func(r *http.Request) (*url.URL, error) {
			return proxyUrl, nil
		}
	}

	c.Transport = httpt

	cloud = os.Getenv("EMU_CLOUD")
	if cloud == "" {
		utils.SetErrorState("no cloud defined")
		return
	}
	connectionsRaw := os.Getenv("EMU_CONNECTIONS")
	var err error
	connections, err = strconv.Atoi(connectionsRaw)
	if err != nil {
		utils.SetErrorState(fmt.Sprintf("failed to parse number of connections: %s", err.Error()))
		return
	}

	mathrand.Seed(time.Now().UnixNano())

	validateDeviceConfig()
	numChannels := connections * 6

	// Be more aggressive with memory reclaiming
	debug.SetGCPercent(10)

	readLimiter = make(chan struct{}, numChannels)
}

func validateDeviceConfig() {
	deviceConfigJson := os.Getenv("DEVICE_CONFIG")
	config := config.DeviceConfiguration{}
	if deviceConfigJson != "" {
		err := json.Unmarshal([]byte(deviceConfigJson), &config)
		if err != nil {
			utils.SetErrorState(fmt.Sprintf("failed to parse device configuration: %s", err.Error()))
			return
		}

		fmt.Printf("config: %+v\n", config)
		err = utils.PopulateTraces(config)
		if err != nil {
			utils.SetErrorState(fmt.Sprintf("failed to get traces: %s", err.Error()))
			return
		}
	}
}

type deviceConnector struct {
	// The configuration of this device
	config config.DeviceConfiguration

	// Set of connections for this device
	connections []*connectionEmulator

	// The parent of this device, if nil device has no parent.
	parentDevice *deviceConnector

	// The account name to which this device has been claimed.
	// Empty if device is unclaimed.
	account   string
	claimuser string

	// The devices private key to authenticate to intersight
	privateKey *rsa.PrivateKey

	// Devices pem encoded public key
	publicKey string

	// The access key id acquired during registration
	accessKey string

	// The MOID of the asset.DeviceRegistration associated with this connection
	id string

	// The serial number(s) of this connection.
	identifier []string

	// Vendor name
	vendor string

	// PID of the devices
	pid []string

	// The ip(s) of this device
	ip []string

	// The ipv6(s) of this device
	ipv6 []string

	// Mutex for the device
	mu sync.Mutex

	// Retry time for registration attempts
	registerRetry time.Duration

	// List of child deviceConnector Objects
	children []*deviceConnector

	// List of non-connected inventory items
	inventory []inventory.InventoryItem

	// Current status of device connector
	status int

	// Connector version to report to cloud
	connectorVersion string
}

// An emulated device connector connection
type connectionEmulator struct {
	// Context of the connection. Used to stop routines when context is cancelled
	ctx context.Context

	// The device this connection belongs to.
	device *deviceConnector

	//
	// The websocket handle, nil if connection is disconnected
	ws *websocket.Conn

	// The backoff duration requested by Intersight.
	serverBackoffDur time.Duration

	// The status of the connection. ["Connected", "NotConnected"]
	connectionStatus string

	// Map of pending sync requests made by this connection. e.g. A rest stim request to fetch an MO from Intersight
	rspMap map[string]chan adconnector.Body

	// Channel of messages destined for the Intersight service
	writeCh chan *writeMesage

	// Mutex for the connection
	mu sync.Mutex

	// Plugin manager used to delegate messages received from Intersight
	platform *plugins.PluginManager

	// The node id of the node within a 'cluster' of device.
	// Currently only a single node is supported
	nodeId string

	// Leadership of this connection, static initialized before connect
	leadership string

	// Marks that the remote server supports and expects flow control message exchange
	// Set when server responds with 'flow-control' in its "X-Intersight-Server-Cap" response header
	flowControlEnabled bool

	// Channel for sending flow control messages from read routine to write
	flowChannel chan *flowMessage

	// Marks that the remote server will send compressed messages across the websocket connection
	// Set when server responds with "write-compression" in its "X-Intersight-Server-Cap" response header
	readCompressionEnabled bool

	cancel context.CancelFunc

	// The id of the connection
	connectionId string

	// Claim notification channel
	claim chan struct{}
}

// Parse the device configuration from the environment if it exists
// Else build a default configuration derived from other values.
func (ce *deviceConnector) parseDeviceConfiguration() error {
	deviceConfigJson := os.Getenv("DEVICE_CONFIG")
	ce.config = config.DeviceConfiguration{}
	if deviceConfigJson != "" {
		err := json.Unmarshal([]byte(deviceConfigJson), &ce.config)
		if err != nil {
			return fmt.Errorf("failed to parse device configuration: %s", err.Error())
		}

		err = utils.PopulateTraces(ce.config)
		if err != nil {
			return fmt.Errorf("failed to get traces: %s", err.Error())
		}
	} else {
		platformType := os.Getenv("EMU_PLATFORM")
		if platformType == "" {
			return fmt.Errorf("no platform type defined")
		}

		ce.config.PlatformType = platformType
	}

	if ce.config.Hostname == "" {
		ce.config.Hostname = "dcemulator"
	}
	return nil
}

// Returns the current connection status
func (ce *connectionEmulator) getConnectionStatus() string {
	ce.mu.Lock()
	defer ce.mu.Unlock()
	return ce.connectionStatus
}

// Returns the current connection status
func (ce *deviceConnector) GetConnectionStatuses() []connectionState {
	ce.mu.Lock()
	defer ce.mu.Unlock()
	var states []connectionState
	for _, conn := range ce.connections {
		states = append(states, connectionState{conn.nodeId, conn.getConnectionStatus()})
	}
	return states
}

// Sets the connection status
func (ce *connectionEmulator) setConnectionStatus(status string) {
	ce.mu.Lock()
	defer ce.mu.Unlock()

	ce.connectionStatus = status
}

// Returns the current account to which the device has been claimed
func (ce *deviceConnector) GetAccount() string {
	ce.mu.Lock()
	defer ce.mu.Unlock()
	return ce.account
}

// Sets the current account to which the device has been claimed
func (ce *deviceConnector) setClaimedToAccount(inaccount string) {
	ce.mu.Lock()
	defer ce.mu.Unlock()
	ce.account = inaccount
}

func (ce *deviceConnector) setupConnections(ctx context.Context) {
	if len(ce.config.Nodes) == 0 {
		ce.config.Nodes = []config.DeviceNode{
			{
				NodeId: adutil.NewRandStr(5),
			},
		}
	}

	for i, node := range ce.config.Nodes {

		var leadership string
		if i == 0 { // TODO: Always first is primary for now
			leadership = leaderPrimary
		} else {
			leadership = leaderSecondary
		}

		connection := &connectionEmulator{device: ce, nodeId: node.NodeId, leadership: leadership}
		ce.connections = append(ce.connections, connection)
		connection.setupPluginManager(ctx)
	}
}

// Sets the claimed by user name of the device
func (ce *deviceConnector) setClaimedByUser(user string) {
	ce.mu.Lock()
	defer ce.mu.Unlock()
	ce.claimuser = user
}

func (ce *deviceConnector) start(inctx context.Context) {

	ce.status = emulatorStatusStarting
	logger := glogger.With(zap.String(adlog.LogDeviceMoId, ce.id))

	ctx := adlog.ContextWithValue(inctx, logger)

	if ce.parentDevice != nil {
		for {
			// Wait for parent to be started
			emulatorsMu.Lock()
			parent := emulators[ce.parentDevice.id]
			if parent != nil {
				ce.parentDevice = parent
				emulatorsMu.Unlock()
				if parent.getChild(ce.config) == nil {
					parent.children = append(parent.children, ce)
				}
				break
			}
			emulatorsMu.Unlock()

			logger.Info("child waiting for parent to start")
			time.Sleep(reconnectRate)
		}
	}

	// Generate a private key
	if ce.privateKey == nil {
		ce.privateKey, ce.publicKey = utils.GenerateRsaKeyPair()
	} else {
		emulatorsMu.Lock()
		emulators[ce.id] = ce
		emulatorsMu.Unlock()
	}

	ce.connectorVersion = "1.0.9-275" // TODO: Configure

	// Register if we haven't yet.
	first := true
	if ce.id == "" {
		// create a temp id for reporting registration status
		tempid := "registering_" + adutil.NewRandStr(8)
		emulatorsMu.Lock()
		emulators[tempid] = ce
		emulatorsMu.Unlock()

		ce.connections = []*connectionEmulator{&connectionEmulator{}}
		ce.registerRetry = reconnectRate
		for {
			if !first {
				logger.Sugar().Infof("register failed, sleeping: %s", reconnectRate.String())
				time.Sleep(ce.registerRetry)
			}
			first = false

			if !ce.register(ctx) {
				continue
			}

			emulatorsMu.Lock()
			delete(emulators, tempid)
			emulators[ce.id] = ce
			emulatorsMu.Unlock()
			ce.connections = []*connectionEmulator{}

			logger = logger.With(zap.String(adlog.LogDeviceMoId, ce.id))
			ctx = adlog.ContextWithValue(ctx, logger)
			break
		}
	} else {
		numNodes := len(ce.config.Nodes)
		if numNodes == 0 {
			numNodes = 1
		}
		ce.setupIp(numNodes)
		ce.setupIpV6(numNodes)
	}

	if len(ce.connections) == 0 {
		ce.setupConnections(ctx)
	}

	for _, con := range ce.connections {
		if ce.connections[0].connectionStatus != "Connected" {
			go con.run(ctx)
		}
	}
	ce.status = emulatorStatusStarted
}

func (dc *deviceConnector) getChild(childConfig config.DeviceConfiguration) *deviceConnector {
	for _, child := range dc.children {
		nodes := child.config.Nodes
		hostname := child.config.Hostname
		child.config.Nodes = nil
		child.config.Hostname = childConfig.Hostname
		exists := reflect.DeepEqual(child.config, childConfig)
		child.config.Nodes = nodes
		child.config.Hostname = hostname
		if exists {
			return child
		}
	}
	return nil
}

func (dc *deviceConnector) StartChildDc(ctx context.Context, childConfig config.DeviceConfiguration) {
	child := dc.getChild(childConfig)
	child.mu.Lock()
	defer child.mu.Unlock()
	bkndCtx := utils.CreateTraceContext(context.Background(), utils.GetTraceFromContext(ctx))
	if child.status == emulatorStatusCreated {
		go child.start(bkndCtx)
	}
}

func (dc *connectionEmulator) StartChildDc(ctx context.Context, childConfig config.DeviceConfiguration) {
	dc.device.StartChildDc(ctx, childConfig)
}

// Setup plugin manager of emulated connection. This function must be called prior to
// calling ce.run()
func (ce *connectionEmulator) setupPluginManager(inctx context.Context) {
	trace := &utils.TraceType{
		Platform:   ce.device.config.PlatformType,
		Node:       ce.nodeId,
		Uuid:       ce.device.accessKey,
		Serial:     ce.device.identifier,
		Ip:         ce.device.ip,
		DeviceMoid: ce.device.id,
		Config:     &ce.device.config,
	}
	if ce.device.parentDevice != nil {
		trace.ParentNode = ce.device.parentDevice.connections[0].nodeId
	}
	topctx := plugins.CreateDispatcherContext(inctx, ce)
	topctx = utils.CreateTraceContext(topctx, trace)
	ce.ctx = topctx
	ce.platform = plugins.GetPluginManager(topctx, ce.device.config.PlatformType,
		ce.device.config.InventoryDatabase, ce.nodeId)
}

// The main connection loop routine. Responsible for establishing the connection to Intersight
// and retrying in case of disconnects.
func (ce *connectionEmulator) run(inctx context.Context) {
	topctx := ce.ctx
	logger := adlog.MustFromContext(inctx)

	ce.connectionId = adutil.NewRandStr(8)
	ce.claim = make(chan struct{})
	patched := false
	first := true
	for {
		ce.serverBackoffDur = 0
		ce.setConnectionStatus("NotConnected")
		ce.ws = nil
		ce.rspMap = make(map[string]chan adconnector.Body)
		ce.writeCh = make(chan *writeMesage)
		ce.flowChannel = make(chan *flowMessage)

		var ctx context.Context
		ctx, ce.cancel = context.WithCancel(topctx)

		if !first {
			var backoff time.Duration
			if ce.serverBackoffDur != 0 && honorServerRequests {
				backoff = ce.serverBackoffDur
			} else {
				backoff = reconnectRate
			}

			logger.Sugar().Infof("sleeping: %s", backoff.String())
			time.Sleep(backoff)
		}
		first = false

		// Create the websocket
		if !ce.dialWebsocket() {
			ce.cancel()
			continue
		}

		ce.ws.SetPingHandler(func(pingmsg string) error {
			//logger.Sugar().Info("got ping")
			return nil
		})
		ce.ws.SetPongHandler(func(pingmsg string) error {
			//logger.Sugar().Info("got pong")
			return nil
		})

		ce.setConnectionStatus(connectedStatus)

		wg := new(sync.WaitGroup)
		wg.Add(1)
		go ce.writeLoop(ctx, wg)

		ce.disconnectLoop(ctx, wg)

		if !patched {
			go ce.patchDr(ctx)
			patched = true
		}

		go ce.runOperations(ctx)

		ce.readLoop(ctx)
		logger.Info("read done")
		ce.cancel()

		wg.Wait()
		logger.Info("wait done")
	}
}

func (dc *deviceConnector) getConfigForChild() (*utils.ParentSignature, error) {

	curTime := time.Now().UTC()

	sig := &utils.ParentSignature{}
	sig.TimeStamp = curTime

	sig.DeviceId = dc.id

	// Hash and sign the nonce
	h := crypto.SHA256.New()

	_, err := h.Write([]byte(sig.DeviceId))
	if err != nil {
		return nil, err
	}

	_, err = h.Write([]byte(curTime.Format(time.RFC3339)))
	if err != nil {
		return nil, err
	}
	signed, err := rsa.SignPKCS1v15(rand.Reader, dc.privateKey, crypto.SHA256, h.Sum(nil))
	if err != nil {
		return nil, err
	}

	sig.Signature = signed
	sig.NodeId = dc.connections[0].nodeId
	return sig, nil
}

func (ce *deviceConnector) setupIp(numNodes int) {
	ce.ip = []string{}
	for k := 0; k < numNodes; k++ {
		ip := ""
		for i := 0; i < 4; i++ {
			ip += strconv.Itoa(mathrand.Intn(254) + 1)
			if i < 3 {
				ip += "."
			}
		}
		ce.ip = append(ce.ip, ip)
	}
}

func randomHex(n int) string {
	bytes := make([]byte, n)
	if _, err := rand.Read(bytes); err != nil {
		return "a148"
	}
	return hex.EncodeToString(bytes)
}

func (ce *deviceConnector) setupIpV6(numNodes int) {
	ce.ipv6 = []string{}
	for k := 0; k < numNodes; k++ {
		ipv6 := fmt.Sprintf("%s::%s:%s:%s:%s", randomHex(2), randomHex(2),
			randomHex(2), randomHex(2), randomHex(2))
		ce.ipv6 = append(ce.ipv6, ipv6)
	}
}

func (ce *deviceConnector) setupIpSerial() {
	numNodes := len(ce.config.Nodes)
	if numNodes == 0 {
		numNodes = 1
	}

	if len(ce.identifier) == 0 {
		if len(ce.config.Serial) != 0 {
			ce.identifier = ce.config.Serial
		} else {
			for i := 0; i < numNodes; i++ {
				ce.identifier = append(ce.identifier, "EM"+strings.ToUpper(adutil.NewRandStr(4)))
			}
		}
	}

	ce.setupIp(numNodes)
	ce.setupIpV6(numNodes)
}

// Registers a new connection to Intersight
func (ce *deviceConnector) register(ctx context.Context) bool {

	logger := adlog.MustFromContext(ctx)

	var hostname []string
	if len(ce.config.HostnameAbs) != 0 {
		hostname = ce.config.HostnameAbs
	} else {
		hostname = []string{ce.config.Hostname + "-" + adutil.NewRandStr(8)}
	}

	ce.setupIpSerial()
	if ce.vendor == "" {
		ce.vendor = "FakeVendor"
	}

	if len(ce.pid) == 0 {
		ce.pid = []string{"ConnectionEmulator"}
	}

	r := utils.Registration{
		ApiVersion:       apiVersion,
		DeviceHostname:   hostname,
		DeviceIpAddress:  ce.ip,
		ExecutionMode:    "ContainerEmulator",
		Pid:              ce.pid,
		PlatformType:     ce.config.PlatformType,
		PublicAccessKey:  ce.publicKey,
		Serial:           ce.identifier,
		Vendor:           ce.vendor,
		ConnectorVersion: ce.connectorVersion,
		ParentSignature:  nil,
		Tags:             []map[string]interface{}{{"Key": "cisco.meta.emulatorPod", "Value": os.Getenv("KUBE_POD_NAME")}},
	}

	if ce.parentDevice != nil {
		var perr error
		r.ParentSignature, perr = ce.parentDevice.getConfigForChild()
		if perr != nil {
			logger.Error("failed to get parent", zap.Error(perr))
			return false
		}
	}

	b, err := json.Marshal(&r)
	if err != nil {
		logger.Error("failed to marshal registration", zap.Error(err))
		return false
	}

	req, err := http.NewRequest(http.MethodPost,
		"https://"+cloud+"/api/v1/asset/DeviceRegistrations", bytes.NewReader(b))
	if err != nil {
		logger.Error("failed to create registration request", zap.Error(err))
		return false
	}

	req.Header.Add("x-starship-jsonfmt", "moref")
	logger.Info("registering")

	resp, err := c.Do(req)
	if err != nil {
		logger.Error("failed to execute registration request", zap.Error(err))
		ce.connections[0].setConnectionStatus(fmt.Sprintf("register failed. Err: %s", err.Error()))
		return false
	}
	defer func() {
		cerr := resp.Body.Close()
		if cerr != nil {
			logger.Error("failed to close body", zap.Error(err))
		}
	}()

	out, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		logger.Error("failed to read response body", zap.Error(err))
		ce.connections[0].setConnectionStatus(fmt.Sprintf("register failed. Err: %s", err.Error()))
		return false
	}
	logger.Sugar().Infof("got back: %s", string(out))

	if resp.StatusCode != http.StatusOK {
		ce.connections[0].setConnectionStatus(fmt.Sprintf("register failed. Status: %d. Body : %s", resp.StatusCode, string(out)))
		logger.Sugar().Infof("non-zero status code: %d : %s", resp.Status, string(out))

		if resp.StatusCode == http.StatusServiceUnavailable {
			backoff := resp.Header.Get("X-ConnectorConnectionBackoff")
			if backoff != "" {
				backoffDuration, berr := strconv.Atoi(backoff)
				if berr != nil {
					logger.Error("Unable to parse backoff interval", zap.Error(berr))
				} else {
					// Reset the sleep index
					fmt.Println("got backoff of:", time.Duration(backoffDuration).String())
					ce.registerRetry = time.Duration(backoffDuration)
				}
			}
		}

		return false
	}

	if string(out) == "" {
		logger.Info("empty response")
		return false
	}

	rout := &utils.RegistrationOut{}

	err = json.Unmarshal(out, rout)
	if err != nil {
		logger.Error("failed to parse registration response", zap.Error(err))
		return false
	}

	logger.Sugar().Infof("registered with cloud as id: %s", rout.Moid)
	ce.id = rout.Moid
	ce.accessKey = rout.AccessKeyId

	privkey_bytes := x509.MarshalPKCS1PrivateKey(ce.privateKey)
	privateKeyPem := string(pem.EncodeToMemory(
		&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: privkey_bytes,
		},
	))

	sdevice := &storage.EmulatorDevice{
		Moid:          ce.id,
		AccessKeyId:   rout.AccessKeyId,
		PrivateKeyPem: privateKeyPem,
		Identifier:    ce.identifier,
		Config:        ce.config,
	}
	if ce.parentDevice != nil {
		sdevice.ParentId = ce.parentDevice.id
	}
	err = storage.GetEmulatorStorage(ctx).WriteDevice(ctx, sdevice)
	if err != nil {
		logger.Error("failed to add device to storage", zap.Error(err))
	}

	return true
}

func (dc *deviceConnector) UpdateVersion(version string) {
	go func() {
		time.Sleep(5 * time.Second)
		dc.connectorVersion = version
		for _, conn := range dc.connections {
			conn.cancel()
		}
	}()
}

func (ce *connectionEmulator) ToggleConnection() {
	for _, conn := range ce.device.connections {
		conn.cancel()
	}
}

func (ce *connectionEmulator) UpdateVersion(version string) {
	ce.device.UpdateVersion(version)
}

func (ce *connectionEmulator) GetVersion() string {
	return ce.device.connectorVersion
}

// Creates the websocket connection to Intersight
func (ce *connectionEmulator) dialWebsocket() bool {
	logger := adlog.MustFromContext(ce.ctx)
	lUrl := url.URL{Scheme: "wss", Host: cloud, Path: "/ws/v1/mgmt"}

	dateString := time.Now().Format(time.RFC1123)

	authCredentials := []byte(ce.device.accessKey + ":")

	nonce := utils.NewRandStr(16)
	h := crypto.SHA256.New()
	_, err := h.Write([]byte(nonce))
	if err != nil {
		logger.Error("failed to write signature sha", zap.Error(err))
		return false
	}
	_, err = h.Write([]byte(dateString))
	if err != nil {
		logger.Error("failed to write signature sha", zap.Error(err))
		return false
	}
	signed, err := rsa.SignPKCS1v15(rand.Reader, ce.device.privateKey, crypto.SHA256, h.Sum(nil))
	if err != nil {
		logger.Error("failed to sign signature", zap.Error(err))
		return false
	}

	authCredentials = append(authCredentials, []byte(nonce+":")...)
	authCredentials = append(authCredentials, signed...)

	lead, err := json.Marshal(ce.leadership)
	if err != nil {
		logger.Error("failed to marshal leadership", zap.Error(err))
		return false
	}

	wsHeader := http.Header{
		"Authorization":           []string{"Basic " + base64.StdEncoding.EncodeToString(authCredentials)},
		"Date":                    []string{dateString},
		"Origin":                  []string{"http://localhost/"},
		"ConnectorApiVersion":     []string{strconv.Itoa(apiVersion)},
		"X-ConnectorVersion":      []string{ce.device.connectorVersion},
		"X-ConnectorConnectionId": []string{ce.connectionId},
		"X-ConnectorLeadership":   []string{string(lead)},
		"X-ConnectorNodeId":       []string{ce.nodeId},
		"X-Forwarded-For":         []string{"10.8.9.39"},
	}

	if ce.device.GetAccount() != "" {
		wsHeader.Set("Device-Claim-State", "Claimed")
	} else {
		wsHeader.Set("Device-Claim-State", "Unclaimed")
	}

	if ce.device.parentDevice != nil {
		parentSig, serr := ce.device.parentDevice.getConfigForChild()
		if serr != nil {
			logger.Error("failed to get parent config", zap.Error(serr))
			return false
		}
		wsHeader.Set("X-Parent-Signature", string(adio.ObjectToJson(ce.ctx, parentSig, adio.JsonOptSerializeAllNoIndent)))
	}

	ws, resp, err := dialer.Dial(lUrl.String(), wsHeader)
	if err != nil {
		fmt.Printf("URL: %v", lUrl.String())
		logger.Error("dial error", zap.Error(err))

		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			logger.Error("bad resp from cloud", zap.String(adlog.LogTraceId, resp.Header.Get(adsec.TraceIdHeader)))
			if resp.StatusCode == http.StatusServiceUnavailable {
				backoff := resp.Header.Get("X-ConnectorConnectionBackoff")
				if backoff != "" {
					backoffDuration, berr := strconv.Atoi(backoff)
					if berr != nil {
						logger.Error("Unable to parse backoff interval", zap.Error(berr))
					} else {
						// Reset the sleep index
						fmt.Println("got backoff of:", time.Duration(backoffDuration).String())
						ce.serverBackoffDur = time.Duration(backoffDuration)
					}
				}
			}

			respb, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				fmt.Println("failed read response body: ", err)
			}
			logger.Sugar().Infof("ws non-zero status code: " + resp.Status)
			ce.setConnectionStatus(fmt.Sprintf("Intersight rejectected with : %d : %s", resp.StatusCode, string(respb)))
		} else {
			ce.setConnectionStatus(fmt.Sprintf("websocket dial failed: %s", err.Error()))
		}
		return false
	}

	serverCap := resp.Header.Get("X-Intersight-Server-Cap")
	caps := strings.Split(serverCap, ",")
	for _, cap := range caps {
		switch cap {
		case "write-compression":
			ce.readCompressionEnabled = true
		case "flow-control":
			ce.flowControlEnabled = true
		}
	}

	err = utils.ClientAuth(ws, ce.device.privateKey)
	if err != nil {
		logger.Error("client auth fail", zap.Error(err))
		return false
	}

	ce.ws = ws
	return true
}

// Routine periodically breaks the connection to Intersight if disconnect rate is defined in environment
func (ce *connectionEmulator) disconnectLoop(ctx context.Context, wg *sync.WaitGroup) {
	logger := adlog.MustFromContext(ce.ctx)
	if disconnectRate != 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-time.After(disconnectRate):
					if ce.ws != nil {
						logger.Info("closing ws due to disconnect rate")
						cerr := ce.ws.Close()
						if cerr != nil {
							logger.Error("failed to close websocket", zap.Error(cerr))
						}

						return
					}
				case <-ctx.Done():
					return
				}
			}
		}()
	}
}

// Send a rest request
// TODO: return response body
func (ce *connectionEmulator) SendRestStim(ctx context.Context, body map[string]interface{}, method, url string) error {
	logger := adlog.MustFromContext(ctx)
	stim := adapp_stim_base.RestStim{
		RestUrl:     url,
		Verb:        method,
		RestHeaders: http.Header{"x-starship-jsonfmt": []string{"moref"}},
		RestBody:    string(adio.ObjectToJson(ctx, body, nil)),
	}
	stim.GenerateRequestId()

	rb := adio.ObjectToJson(ctx, &stim, adio.JsonOptSerializeAllNoIndent)
	logger.Sugar().Infof("sending rest stim: %s", rb)

	msg, err := adconnector.NewMessage(ctx,
		adconnector.Header{Type: "TypeRestStim", RequestId: stim.RequestId},
		rb)
	if err != nil {
		return err
	}

	rspChan := make(chan adconnector.Body, 1)

	ce.mu.Lock()
	ce.rspMap[stim.RequestId] = rspChan
	ce.mu.Unlock()

	ce.Dispatch(ctx, bytes.NewReader(msg))

	var rsp adconnector.Body
	select {
	case <-ctx.Done():
		return ctx.Err()
	case rsp = <-rspChan:
	}

	var respStim adapp_stim_base.RestStim

	err = adio.JsonToObjectCtx(ctx, bytes.NewReader(rsp), &respStim)
	if err != nil {
		// This could happen in normal case when the websocket connection is closed.
		logger.Error("failed to read response rest stim", zap.Error(err))
		return err
	}

	if respStim.RespStatus != http.StatusOK {
		return fmt.Errorf("non-zero response code: %d\nbody:%s\n", respStim.RespStatus, respStim.RestBody)
	}

	return nil
}

type controlMessage struct {
	ClaimTime time.Time
	Account   string
	DeviceId  string
	User      string
}

func (ec *connectionEmulator) handleControlMessage(in adconnector.Body) {
	logger := adlog.MustFromContext(ec.ctx)

	if ec.leadership != leaderPrimary {
		logger.Info("non-primary ignoring control message")
		return
	}
	cm := &controlMessage{}
	err := json.Unmarshal(in, cm)
	if err != nil {
		logger.Error("failed unmarshal of control message", zap.Error(err))
		return
	}
	logger.Sugar().Infof("got control message: %+v\n", *cm)
	previousAccountId := ec.device.GetAccount()
	if cm.Account != previousAccountId {
		close(ec.claim)
		ec.claim = make(chan struct{})
	}
	ec.device.setClaimedToAccount(cm.Account)
	ec.device.setClaimedByUser(cm.User)
	if cm.DeviceId != "" && cm.DeviceId != ec.device.id {
		oldid := ec.device.id
		emulatorsMu.Lock()
		delete(emulators, oldid)
		ec.device.id = cm.DeviceId
		emulators[cm.DeviceId] = ec.device
		emulatorsMu.Unlock()
	}

	if cm.Account != "" && previousAccountId != cm.Account && ec.device.config.PlatformType == plugins.UcsfiismPlatformType {
		for _, con := range ec.device.connections {
			networkAgent := con.platform.Plugins["TypeNetworkAgent"].(*plugins.NetworkAgent)
			networkAgent.ResetInventoryStatusMaps(ec.ctx)
		}
	}
}

func (ec *connectionEmulator) GetIdentity() string {
	return ec.device.id
}

func handleOsSignals(ctx context.Context) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGUSR1)
	go func() {
		// Wait for signal
		for sig := range sigs {
			if sig == syscall.SIGUSR1 {
				bt := make([]byte, 5<<10<<10)
				size := runtime.Stack(bt, true)
				adlog.MustFromContext(ctx).Sugar().Infof("Stack dump requested\n%s", bt[:size])
			}
		}
	}()
}

func generateRedfishEndpoints(ctx context.Context, httpPlugin plugins.HttpPlugin, config *config.DeviceConfiguration) {
	logger := adlog.MustFromContext(ctx)
	logger.Info("Generating redfish mock dirs for children")

	for i := range config.Children {
		httpPlugin.SetAdditionalHeaders(nil)
		if len(config.Children[i].Serial) == 0 {
			config.Children[i].Serial = append(config.Children[i].Serial,
				"EM"+strings.ToUpper(adutil.NewRandStr(4)))
		}
		serial := config.Children[i].Serial[0]

		httpPlugin.InventoryDb = config.Children[i].InventoryDatabase
		_, err := httpPlugin.ResolvePath(ctx, fmt.Sprintf("/%s/redfish/v1/", serial))
		if err != nil {
			logger.Error("failed to init redfish mock dir", zap.Error(err))
		}
		if config.Children[i].PlatformType == plugins.ChassisType && config.Children[i].Children != nil {
			for j := range config.Children[i].Children {
				bladeId := j + 1

				if config.Children[i].Children[j].Hostname == "" {
					config.Children[i].Children[j].Hostname = "dcemulator"
				}

				bladeDetails, err := httpPlugin.ResolvePath(ctx,
					fmt.Sprintf("/%s/redfish/v1/Chassis/Blade%d", serial, bladeId))
				if err != nil {
					logger.Error("failed to get blade details from chassis redfish endpoint", zap.Error(err))
				}

				config.Children[i].Children[j].Serial = []string{bladeDetails["SerialNumber"].(string)}

				httpPlugin.InventoryDb = config.Children[i].Children[j].InventoryDatabase
				httpPlugin.SetAdditionalHeaders(map[string]string{
					"CHASSIS_ID": fmt.Sprintf("%d", 1),
					"BLADE_ID":   fmt.Sprintf("%d", bladeId),
				})

				_, err = httpPlugin.ResolvePath(ctx, fmt.Sprintf("/%s/redfish/v1/", config.Children[i].Children[j].Serial[0]))

				if err != nil {
					logger.Error("blade redfish response error", zap.Error(err))
				}
			}
		}
	}
}

func StartEmulators() {
	if utils.GetErrorState() != "" {
		fmt.Println("error state set: " + utils.GetErrorState())
		return // Don't do anything if config is wrong
	}

	ctx := context.Background()
	ctx = adlog.ContextWithValue(ctx, glogger)

	handleOsSignals(ctx)

	spawned := 0
	devices := storage.GetEmulatorStorage(ctx).GetAllDevices(ctx)
	for _, device := range devices {
		ce := &deviceConnector{
			id:         device.Moid,
			accessKey:  device.AccessKeyId,
			privateKey: device.PrivateKey,
			identifier: device.Identifier,
			config:     device.Config,
			status:     emulatorStatusCreated,
		}

		if device.ParentId != "" {
			ce.parentDevice = &deviceConnector{id: device.ParentId}
		}

		time.Sleep(2 * time.Second)
		go ce.start(ctx)
		spawned++

		time.Sleep(spawnRate)

		if spawned > connections && spawned == len(devices) {
			break
		}
	}

	httpPlugin := plugins.HttpPlugin{}
	httpPlugin.Init(ctx)

	for ; spawned < connections; spawned++ {
		ce := &deviceConnector{}
		err := ce.parseDeviceConfiguration()

		if err != nil {
			utils.SetErrorState(fmt.Sprintf("%s", err.Error()))
			continue
		}

		if ce.config.PlatformType == plugins.UcsfiismPlatformType {
			generateRedfishEndpoints(ctx, httpPlugin, &ce.config)
		}

		go ce.start(ctx)
		time.Sleep(spawnRate)
	}
}

func (ec *deviceConnector) GetSecurityToken() (string, string, error) {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	for _, conn := range ec.connections {
		if conn.leadership == leaderPrimary {
			return conn.getSecurityToken()
		}
	}
	return "", "", fmt.Errorf("no leader to fetch security token")
}

func (ec *connectionEmulator) GetRaw(url string, header http.Header) ([]byte, int, http.Header, error) {
	if ec.getConnectionStatus() != connectedStatus {
		return nil, 0, nil, fmt.Errorf("device not connected")
	}

	ctx, cancel := context.WithDeadline(ec.ctx, time.Now().Add(30*time.Second))
	defer cancel()

	stim := adapp_stim_base.RestStim{
		RestUrl:     url,
		Verb:        http.MethodGet,
		RestHeaders: header,
	}
	stim.GenerateRequestId()

	msg, err := adconnector.NewMessage(ec.ctx,
		adconnector.Header{Type: "TypeRestStim", RequestId: stim.RequestId},
		adio.ObjectToJson(ec.ctx, &stim, adio.JsonOptSerializeAllNoIndent))
	if err != nil {
		return nil, 0, nil, err
	}

	rspChan := make(chan adconnector.Body, 1)

	ec.mu.Lock()
	ec.rspMap[stim.RequestId] = rspChan
	ec.mu.Unlock()

	ec.Dispatch(ctx, bytes.NewReader(msg))

	var rsp adconnector.Body
	select {
	case <-ctx.Done():
		return nil, 0, nil, ctx.Err()
	case rsp = <-rspChan:
	}

	var respStim adapp_stim_base.RestStim

	err = adio.JsonToObjectCtx(ctx, bytes.NewReader(rsp), &respStim)
	if err != nil {
		// This could happen in normal case when the websocket connection is closed.
		glogger.Error("failed to read response rest stim", zap.Error(err))
		return nil, 0, nil, err
	}

	if respStim.RespStatus != http.StatusOK {
		return nil, respStim.RespStatus, respStim.RespHeaders, nil
	}

	return []byte(respStim.RespBody), respStim.RespStatus, respStim.RespHeaders, nil
}

func (ec *connectionEmulator) PostRaw(ctx context.Context, url string, header http.Header, body map[string]interface{}) ([]byte, int, http.Header, error) {
	if ec.getConnectionStatus() != connectedStatus {
		return nil, 0, nil, fmt.Errorf("device not connected")
	}

	ctx, cancel := context.WithDeadline(ctx, time.Now().Add(30*time.Second))
	defer cancel()

	stim := adapp_stim_base.RestStim{
		RestUrl:     url,
		Verb:        http.MethodPost,
		RestHeaders: header,
		RestBody:    string(adio.ObjectToJson(ctx, body, nil)),
	}
	stim.GenerateRequestId()

	msg, err := adconnector.NewMessage(ec.ctx,
		adconnector.Header{Type: "TypeRestStim", RequestId: stim.RequestId},
		adio.ObjectToJson(ec.ctx, &stim, adio.JsonOptSerializeAllNoIndent))
	if err != nil {
		return nil, 0, nil, err
	}

	rspChan := make(chan adconnector.Body, 1)

	ec.mu.Lock()
	ec.rspMap[stim.RequestId] = rspChan
	ec.mu.Unlock()

	ec.Dispatch(ctx, bytes.NewReader(msg))

	var rsp adconnector.Body
	select {
	case <-ctx.Done():
		return nil, 0, nil, ctx.Err()
	case rsp = <-rspChan:
	}

	var respStim adapp_stim_base.RestStim

	err = adio.JsonToObjectCtx(ctx, bytes.NewReader(rsp), &respStim)
	if err != nil {
		// This could happen in normal case when the websocket connection is closed.
		glogger.Error("failed to read response rest stim", zap.Error(err))
		return nil, 0, nil, err
	}

	if respStim.RespStatus != http.StatusOK {
		return nil, respStim.RespStatus, respStim.RespHeaders, nil
	}

	return []byte(respStim.RespBody), respStim.RespStatus, respStim.RespHeaders, nil
}

func (ec *connectionEmulator) getSecurityToken() (string, string, error) {
	if ec.getConnectionStatus() != connectedStatus {
		return "", "", fmt.Errorf("device not connected")
	}

	ctx, cancel := context.WithDeadline(ec.ctx, time.Now().Add(30*time.Second))
	defer cancel()

	stim := adapp_stim_base.RestStim{
		RestUrl:     "/v1/asset/DeviceRegistrations/" + ec.device.id,
		Verb:        http.MethodGet,
		RestHeaders: http.Header{"x-starship-jsonfmt": []string{"moref"}},
	}
	stim.GenerateRequestId()

	msg, err := adconnector.NewMessage(ec.ctx,
		adconnector.Header{Type: "TypeRestStim", RequestId: stim.RequestId},
		adio.ObjectToJson(ec.ctx, &stim, adio.JsonOptSerializeAllNoIndent))
	if err != nil {
		return "", "", err
	}

	rspChan := make(chan adconnector.Body, 1)

	ec.mu.Lock()
	ec.rspMap[stim.RequestId] = rspChan
	ec.mu.Unlock()

	ec.Dispatch(ctx, bytes.NewReader(msg))

	var rsp adconnector.Body
	select {
	case <-ctx.Done():
		return "", "", ctx.Err()
	case rsp = <-rspChan:
	}

	var respStim adapp_stim_base.RestStim

	err = adio.JsonToObjectCtx(ctx, bytes.NewReader(rsp), &respStim)
	if err != nil {
		// This could happen in normal case when the websocket connection is closed.
		glogger.Error("failed to read response rest stim", zap.Error(err))
		return "", "", err
	}

	if respStim.RespStatus != http.StatusOK {
		glogger.Sugar().Errorf("non-zero response code: %d\n", respStim.RespStatus)
		return "", "", fmt.Errorf("non-zero response code: %d", respStim.RespStatus)
	}

	rout := &utils.RegistrationOutSecToken{}

	err = json.Unmarshal([]byte(respStim.RespBody), rout)
	if err != nil {
		glogger.Error("failed to parse registration response", zap.Error(err))
		return "", "", err
	}

	stim = adapp_stim_base.RestStim{
		RestUrl:     "/v1/asset/SecurityTokens/" + rout.SecurityToken,
		Verb:        http.MethodGet,
		RestHeaders: http.Header{"x-starship-jsonfmt": []string{"moref"}},
	}
	stim.GenerateRequestId()

	msg, err = adconnector.NewMessage(ec.ctx,
		adconnector.Header{Type: "TypeRestStim", RequestId: stim.RequestId},
		adio.ObjectToJson(ec.ctx, &stim, adio.JsonOptSerializeAllNoIndent))
	if err != nil {
		return "", "", err
	}

	ec.mu.Lock()
	ec.rspMap[stim.RequestId] = rspChan
	ec.mu.Unlock()

	ec.Dispatch(ctx, bytes.NewReader(msg))

	select {
	case <-ctx.Done():
		return "", "", ctx.Err()
	case rsp = <-rspChan:
	}

	err = adio.JsonToObjectCtx(ctx, bytes.NewReader(rsp), &respStim)
	if err != nil {
		// This could happen in normal case when the websocket connection is closed.
		glogger.Error("failed to read response rest stim", zap.Error(err))
		return "", "", err
	}

	if respStim.RespStatus != http.StatusOK {
		glogger.Sugar().Errorf("non-zero response code: %d\n", respStim.RespStatus)
		return "", "", fmt.Errorf("non-zero response code: %d", respStim.RespStatus)
	}

	secToken := &utils.SecurityToken{}

	err = json.Unmarshal([]byte(respStim.RespBody), secToken)
	if err != nil {
		glogger.Error("failed to parse registration response", zap.Error(err))
		return "", "", err
	}

	deviceId := ""
	for _, i := range ec.device.identifier {
		if deviceId != "" {
			deviceId += "&"
		}
		deviceId += i
	}

	return secToken.Token, deviceId, nil
}

// Patch the dr on connect with latest emulator info.
func (ec *connectionEmulator) patchDr(ctx context.Context) {
	logger := adlog.MustFromContext(ctx)

	// Add tags
	body := string(adio.ObjectToJson(ctx,
		map[string]interface{}{
			"Tags": []map[string]interface{}{{"Key": "cisco.meta.emulatorPod", "Value": os.Getenv("KUBE_POD_NAME")}},
		}, nil))
	stim := adapp_stim_base.RestStim{
		RestUrl:     "/v1/asset/DeviceRegistrations/" + ec.device.id,
		Verb:        http.MethodPatch,
		RestHeaders: http.Header{"x-starship-jsonfmt": []string{"moref"}},
		RestBody:    body,
	}
	stim.GenerateRequestId()

	msg, err := adconnector.NewMessage(ec.ctx,
		adconnector.Header{Type: "TypeRestStim", RequestId: stim.RequestId},
		adio.ObjectToJson(ec.ctx, &stim, adio.JsonOptSerializeAllNoIndent))
	if err != nil {
		logger.Error("failed to create patch message", zap.Error(err))
		return
	}

	rspChan := make(chan adconnector.Body, 1)

	ec.mu.Lock()
	ec.rspMap[stim.RequestId] = rspChan
	ec.mu.Unlock()

	ec.Dispatch(ctx, bytes.NewReader(msg))

	var rsp adconnector.Body
	select {
	case <-ctx.Done():
		logger.Error("timeout on patch")
		return
	case rsp = <-rspChan:
	}

	var respStim adapp_stim_base.RestStim

	err = adio.JsonToObjectCtx(ctx, bytes.NewReader(rsp), &respStim)
	if err != nil {
		// This could happen in normal case when the websocket connection is closed.
		logger.Error("failed to create read patch response", zap.Error(err))
		return
	}

	if respStim.RespStatus != http.StatusOK {
		logger.Sugar().Errorf("non-zero response code to dr patch: %d\n", respStim.RespStatus)
		return
	}
	logger.Info("dr patch success")
}

func (ce *connectionEmulator) GetChildConfig(ctx context.Context) map[string]config.DeviceConfiguration {
	configs := make(map[string]config.DeviceConfiguration)
	for _, child := range ce.device.children {
		configs[child.identifier[0]] = child.config
	}
	return configs
}

type EmulatorState struct {
	Id                 string
	Account            string
	ConnectionStatuses []connectionState

	ErrorState string `json:",omitempty"`
}

type connectionState struct {
	NodeId           string
	ConnectionStatus string
}

type EmulatorToken struct {
	EmulatorState
	DeviceIdentifier string
	Token            string
}

func GetEmulatorIds() []string {
	emulatorsMu.RLock()
	defer emulatorsMu.RUnlock()
	var out []string

	for _, v := range emulators {
		id := v.id
		out = append(out, id)
	}

	return out
}

func GetEmulatorConnectionStatus() []EmulatorState {
	emulatorsMu.RLock()
	defer emulatorsMu.RUnlock()
	var out []EmulatorState

	for k, v := range emulators {
		state := v.GetConnectionStatuses()

		out = append(out, EmulatorState{Id: k,
			Account:            v.GetAccount(),
			ConnectionStatuses: state})
	}

	return out
}

func GetEmulatorTokens() []EmulatorToken {
	emulatorsMu.RLock()
	defer emulatorsMu.RUnlock()
	var out []EmulatorToken

	var wg sync.WaitGroup
	outch := make(chan EmulatorToken)
	returned := 0
	// Only return claimed devices if no unclaimed exist?
	var claimedDevices []EmulatorToken
outer:
	for k, v := range emulators {
		state := v.GetConnectionStatuses()
		if v.GetAccount() == "" {
			returned++
			wg.Add(1)
			go func(deviceid string, device *deviceConnector) {
				defer wg.Done()
				token, id, err := device.GetSecurityToken()
				if err == nil {
					outch <- EmulatorToken{EmulatorState{Id: deviceid,
						Account: device.GetAccount(), ConnectionStatuses: state}, id, token}
				}
			}(k, v)
			if returned > 20 {
				break outer // Limit the number of security token fetches
			}
		} else {
			claimedDevices = append(claimedDevices, EmulatorToken{EmulatorState{Id: k,
				Account: v.GetAccount(), ConnectionStatuses: state}, "", ""})
		}
	}

	var readWg sync.WaitGroup
	readWg.Add(1)
	go func() {
		defer readWg.Done()
		for o := range outch {
			out = append(out, o)
		}
	}()
	wg.Wait()
	close(outch)
	readWg.Wait()
	if len(out) == 0 {
		return claimedDevices
	}

	return out
}

func GetEmulator(id string) *deviceConnector {
	emulatorsMu.RLock()
	defer emulatorsMu.RUnlock()
	return emulators[id]
}

func ImportInventoryConfig(id string, config io.Reader) error {
	emulatorsMu.RLock()
	emu := emulators[id]
	emulatorsMu.RUnlock()
	if emu == nil {
		return fmt.Errorf("%s does not exist", id)
	}

	invConf := &utils.InventoryConfiguration{}
	err := adio.JsonToObjectCtx(context.TODO(), config, invConf)
	if err != nil {
		return err
	}

	var conn *connectionEmulator
	for _, ec := range emu.connections {
		if ec.nodeId == invConf.NodeId || invConf.NodeId == "" && ec.leadership == leaderPrimary {
			conn = ec
			break
		}
	}

	if conn == nil {
		return fmt.Errorf("%s node does not exist", invConf.NodeId)
	}

	switch emu.config.PlatformType {
	case plugins.UcsfiPlatformType, plugins.Imcm5PlatformType, plugins.Imcm4PlatformType:
		// Get the xml plugin
		xplug := conn.platform.Plugins["TypeXmlApiNoAuth"].(*plugins.XmlPlugin)
		err = xplug.ImportConfiguration(conn.ctx, invConf)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("inventory config not supported on %s", emu.config.PlatformType)
	}

	return nil
}

func ToggleConnections(idchange bool) error {
	emulatorsMu.RLock()
	defer emulatorsMu.RUnlock()
	for _, emu := range emulators {
		for _, conn := range emu.connections {
			if idchange {
				conn.connectionId = adutil.NewRandStr(8)
			}
			if conn.cancel != nil {
				conn.cancel()
			}
		}
	}
	return nil
}
