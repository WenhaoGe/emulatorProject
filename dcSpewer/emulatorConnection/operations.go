package emulatorConnection

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"sort"
	"strconv"
	"time"

	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/plugins"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adcore"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adio"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adlog"
	"go.uber.org/zap"
)

func (ce *connectionEmulator) runOperations(ctx context.Context) {
	//logger := adlog.MustFromContext(ctx)
	operationsRunner := &operations{
		started:          make(chan struct{}, 1),
		ctx:              ctx,
		running:          make(map[string]plugins.Operation),
		operationConfigs: make(map[string][]byte),
		ce:               ce,
	}
	operationsRunner.run(ctx)
}

func (o *operations) getOperationsDevice(ctx context.Context) {
	logger := adlog.MustFromContext(ctx)

	pollTimer := time.NewTimer(10 * time.Second)
	defer pollTimer.Stop()
	first := true

	for {
		if !first {
			select {
			case <-ctx.Done():
				return
			case <-pollTimer.C:
			}
		}
		first = false
		resp, status, _, err := o.ce.GetRaw(fmt.Sprintf("/v1/asset/DeviceRegistrations/%s", o.ce.device.id), nil)
		if err != nil {
			logger.Error("failed to get device", zap.Error(err))
			continue
		}

		if status != http.StatusOK {
			logger.Sugar().Errorf("bad status returned %d", status)
			continue
		}

		var dev map[string]interface{}
		err = adio.JsonToObjectCtx(ctx, bytes.NewReader(resp), &dev)
		if err != nil {
			logger.Error("failed to deserialize device", zap.Error(err))
			continue
		}
		dev["MemberIdentity"] = o.ce.nodeId
		dev["Leadership"] = o.ce.leadership
		o.deviceHeader = string(adio.ObjectToJson(ctx, dev, nil))
		logger.Sugar().Infof("got device : %+v", o.deviceHeader)
		return
	}
}

const (
	// Header containing a json map with the device details.
	deviceHeader = "x-device"

	// Header containing the hash of the current configuration running on the device.
	configHashHeader = "x-config-hash"

	// Response header with the polling interval the device should use
	pollingIntervalHeader = "x-polling-interval"
)

// The polling intervals to retrieve operations from the cloud.
//
// The first fetch has not completed yet.
const firstPollInterval = 5 * time.Minute

// Device is claimed.
const pollingInterval = 10 * time.Minute

// Device is unclaimed.
const pollingIntervalUnClaimed = time.Hour

type operations struct {
	ce *connectionEmulator

	// Channel written to if operations is currently running.
	started chan struct{}

	// Cached context from init.
	ctx context.Context

	// Set of running operations.
	running map[string]plugins.Operation

	// Set of running operations configuration hashes.
	operationConfigs map[string][]byte

	// The cached device header to send on poll
	deviceHeader string

	// The current hash of the operations received from cloud
	opHashHeader string

	// The device has received a successful response from the operations poller
	fetchedFirst bool
}

// Main run routine, manages the running operations.
// Polls the cloud service periodically and starts/stops operations accordingly.
func (o *operations) run(ctx context.Context) {
	logger := adlog.MustFromContext(ctx)
	//defer o.runEnd(ctx)

	pollTimer := time.NewTimer(time.Minute)
	defer pollTimer.Stop()

	// Delay initial run slightly.
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Second):
	}
	logger.Info("starting operations runner")

	o.getOperationsDevice(ctx)

	// At start run the poll immediately
	nextPoll := o.poll(ctx)
	var pollInt time.Duration
	for {
		// Possible polling intervals:
		// 1) Cloud has requested an interval.
		// 2) If we have not gotten the first response from the cloud, shortest interval.
		// 3) If device is claimed.
		// 4) If device is unclaimed set the longest interval.
		if nextPoll != 0 {
			pollInt = nextPoll
			// Do sanity check on the cloud request to protect ourselves from bugs.
			if nextPoll < pollingInterval {
				nextPoll = pollingInterval
			}
		} else if !o.fetchedFirst {
			pollInt = firstPollInterval + time.Duration(rand.Int63())%firstPollInterval
		} else if o.ce.device.claimuser != "" {
			pollInt = pollingInterval + time.Duration(rand.Int63())%pollingInterval
		} else {
			pollInt = pollingIntervalUnClaimed + time.Duration(rand.Int63())%pollingIntervalUnClaimed
		}
		logger.Sugar().Debugf("polling operations after %s", pollInt.String())

		pollTimer.Reset(pollInt)
		select {
		case <-ctx.Done():
			logger.Info("operations run cancelled")
			return
		case <-o.ce.claim:
			go o.run(ctx)
			return
		case <-pollTimer.C:
			nextPoll = o.poll(ctx)
		}

	}
}

type deviceOperation struct {
	Message    map[string]interface{}
	Name       string
	PluginName string
}

// Poll Intersight for any new/changed operations to start.
func (o *operations) poll(ctx context.Context) (cloudPollReq time.Duration) {
	logger := adlog.MustFromContext(ctx)
	defer func() {
		if r := recover(); r != nil {
			logger.Sugar().Infof("poll panic : %+v", r)
			cloudPollReq = pollingIntervalUnClaimed
		}
	}()
	// Check if all operations are active, remove any inactive.
	for name, op := range o.running {
		if !op.Active(ctx) {
			logger.Sugar().Infof("operation %s no longer active", name)
			delete(o.running, name)
			delete(o.operationConfigs, name)
			o.opHashHeader = ""
		}
	}

	// If we're not connected don't try poll.
	if o.ce.getConnectionStatus() != "Connected" {
		logger.Info("device not connected skipping poll")
		return 0
	}
	header := http.Header{}
	header.Add(deviceHeader, o.deviceHeader)
	header.Add(configHashHeader, o.opHashHeader)
	resp, status, rspheader, err := o.ce.GetRaw("/v1/deviceconnector/DeviceOperations", header)
	if err != nil {
		logger.Error("dispatch error", zap.Error(err))
		return 0
	}

	if rspheader.Get(pollingIntervalHeader) != "" {
		dur, err := strconv.Atoi(rspheader.Get(pollingIntervalHeader))
		if err == nil {
			cloudPollReq = time.Duration(dur)
			logger.Sugar().Infof("cloud requested poll interval of %s", cloudPollReq.String())
		}
	}

	switch status {
	case http.StatusAlreadyReported:
		logger.Info("no change in operations")
		o.fetchedFirst = true
		return
	case http.StatusOK:
		logger.Info("received change in operations from cloud")
		o.fetchedFirst = true
	case http.StatusInternalServerError:
		// If cloud responds with 500, assume feature has not been deployed and
		// set a long interval for next poll
		logger.Info("received internal error for poll, retrying later")
		cloudPollReq = time.Hour
		return
	default:
		logger.Sugar().Infof("unexpected status in response : %d %s", status, resp)
		return
	}

	var configsFromCloud map[string]json.RawMessage
	err = adio.JsonToObjectCtx(ctx, bytes.NewReader(resp), &configsFromCloud)
	if err != nil {
		logger.Error("failed to unmarshal configs", zap.Error(err))
		return
	}

	// Calculate the hashes of the incoming.
	incomingHashes := make(map[string][]byte)
	for name, conf := range configsFromCloud {
		sum := sha256.New()
		_, err = sum.Write(conf)
		if err != nil {
			logger.Error("failed to write to sha", zap.Error(err))
			return
		}
		incomingHashes[name] = sum.Sum(nil)
	}

	// Compare with the current hashes.
	changedConfigs := make(map[string]*deviceOperation)
	for name, hash := range incomingHashes {
		ehash := o.operationConfigs[name]
		if !bytes.Equal(ehash, hash) {
			logger.Sugar().Infof("received new config for %s", name)

			op := &deviceOperation{}
			err = adio.JsonToObjectCtx(ctx, bytes.NewReader(configsFromCloud[name]), op)
			if err != nil {
				logger.Error("failed to deserialize operation", zap.Error(err))
				continue
			}
			changedConfigs[name] = op
		}
	}

	// Delete non-existent operations
	for name := range o.operationConfigs {
		if _, ok := incomingHashes[name]; !ok {
			logger.Sugar().Infof("stopping removed operation %s", name)
			op := o.running[name]
			if op != nil {
				err = op.Stop(ctx)
				if err != nil {
					logger.Error("failed to stop operation", zap.Error(err))
				}
				delete(o.running, name)
				delete(o.operationConfigs, name)
			}
		}
	}

	// Start or restart operations.
	for name, conf := range changedConfigs {
		if running, ok := o.running[name]; ok {
			err = running.Stop(ctx)
			if err != nil {
				logger.Error("failed to stop running operation", zap.Error(err))
			}
			delete(o.running, name)
		}

		// Start the operation.
		plug, ok := o.ce.platform.Plugins[conf.PluginName]
		if !ok {
			logger.Error("plugin does not exist")
			continue
		}

		opPlug, ok := plug.(plugins.OperationsPlugin)
		if !ok {
			logger.Warn("plugin does not implement operation plugin")
			continue
		}

		op, err := opPlug.StartOperation(ctx, conf.Message)
		if err != nil {
			logger.Error("failed to start operation", zap.Error(err))
			o.reportOpStatus(ctx, name, "Errored", err)
			continue
		}

		o.reportOpStatus(ctx, name, "Running", err)
		o.running[name] = op
		o.operationConfigs[name] = incomingHashes[name]
	}

	// Sort the configurations lexicographically by name, needed to maintain a consistent
	// hash calculation between cloud and dc as map iteration is not deterministic.
	type confs struct {
		name string
		hash []byte
	}
	configs := make([]confs, len(o.operationConfigs))
	i := 0
	for op, hash := range o.operationConfigs {
		configs[i] = confs{op, hash}
		i++
	}

	sort.Slice(configs, func(i, j int) bool { return configs[i].name < configs[j].name })
	// Re-calculate overall hash.
	ohash := sha256.New()
	for _, config := range configs {
		_, err = ohash.Write(config.hash)
		if err != nil {
			logger.Error("failed to write hash", zap.Error(err))
			return
		}
	}

	overallB := ohash.Sum(nil)
	o.opHashHeader = hex.EncodeToString(overallB)
	return
}

// Report status of an operation to Intersight.
func (o *operations) reportOpStatus(ctx context.Context, name string, state string, statuserr error) {
	logger := adlog.MustFromContext(ctx)
	logger.Sugar().Infof("reporting status of %s", name)

	opstate := make(map[string]interface{})
	opstate["Configuration"] = adcore.MoRef{
		ObjectType: "deviceconnector.OperationConfiguration",
		Selector:   fmt.Sprintf("Name eq '%s'", name),
	}
	opstate["State"] = state
	if statuserr != nil {
		opstate["Error"] = statuserr.Error()
	} else {
		opstate["Error"] = nil
	}
	opstate["RegisteredDevice"] = o.ce.device.id
	logger.Sugar().Infof("sending body : %+v", opstate)

	_, status, _, err := o.ce.PostRaw(ctx, "/v1/deviceconnector/DeviceOperationStates", nil, opstate)
	if err != nil {
		logger.Error("failed to post operation state", zap.Error(err))
	} else if status != http.StatusOK {
		logger.Sugar().Errorf("bad status returned from update : %d", status)
	}
}
