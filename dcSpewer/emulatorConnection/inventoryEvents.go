package emulatorConnection

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/config"
	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/inventory"
	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/plugins"
	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/utils"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adlog"
	"go.uber.org/zap"
)

func (dc *deviceConnector) prepareRackEventsPayload(ctx context.Context, plugin *plugins.HttpPlugin, portStr string) ([][]byte, error) {
	var err error
	logger := adlog.MustFromContext(ctx)
	faPresentArgs := &inventory.FaPresentPayloadArgs{}
	neighborPresentArgs := &inventory.NeighborPresentPayloadArgs{}

	faPresentPayload := inventory.EventPayload{
		Type: "REQUEST",
		API:  "SCI_faPresentUpdatedCbV2",
		Args: faPresentArgs,
	}

	neighborPresentPayload := inventory.EventPayload{
		Type: "REQUEST",
		API:  "SCI_vimNeighborPresentCb",
		Args: neighborPresentArgs,
	}

	rack, err := plugin.ResolvePath(ctx, fmt.Sprintf("/%s/redfish/v1/Systems/%s/", dc.identifier[0], dc.identifier[0]))
	if err != nil {
		logger.Error("redfish response error", zap.Error(err))
		return nil, err
	}

	portId := 0
	dc.vendor = rack["Manufacturer"].(string)
	dc.pid = []string{rack["Model"].(string)}
	faPresentArgs.AInChSerial = inventory.ArgsBufProperty{Buf: dc.identifier[0]}
	faPresentArgs.AInChModel = inventory.ArgsBufProperty{Buf: rack["Model"]}
	faPresentArgs.AInChVendor = inventory.ArgsBufProperty{Buf: dc.vendor}
	faPresentArgs.AInFaVendor = inventory.ArgsBufProperty{Buf: dc.vendor}
	faPresentArgs.AInFaSerial = inventory.ArgsBufProperty{Buf: "FCH11111ZZZ"}

	trace := utils.GetTraceFromContext(ctx)
	if strings.ToLower(trace.Node) == "a" {
		faPresentArgs.AInFaSide = 1
	} else {
		faPresentArgs.AInFaSide = 2
		portId = 1
	}

	networkAdapters, err := plugin.ResolvePath(ctx,
		fmt.Sprintf("/%s/redfish/v1/Chassis/1/NetworkAdapters/", dc.identifier[0]))
	networkMembers := networkAdapters["Members"].([]interface{})
	if err != nil {
		logger.Error("redfish response error", zap.Error(err))
		return nil, err
	}
	adapterPath := ""
	// Get path to MLOM adapter
	for _, m := range networkMembers {
		member := m.(map[string]interface{})
		rfishPath := member["@odata.id"].(string)
		if strings.Contains(rfishPath, "MLOM") {
			adapterPath = fmt.Sprintf("/%s%s", dc.identifier[0], rfishPath)
		}
	}
	// If no MLOM adapter get path to the first adapter in the list
	if adapterPath == "" {
		adapterPath = fmt.Sprintf("/%s%s", dc.identifier[0],
			networkMembers[0].(map[string]interface{})["@odata.id"].(string))
	}
	adaptorDetails, err := plugin.ResolvePath(ctx, adapterPath)
	if err != nil {
		logger.Error("redfish response error", zap.Error(err))
		return nil, err
	}

	portDetails, err := plugin.ResolvePath(ctx,
		fmt.Sprintf("%s/NetworkPorts/Port-%d", adapterPath, portId+1))
	if err != nil {
		logger.Error("redfish response error", zap.Error(err))
		return nil, err
	}
	portNetAddr := portDetails["AssociatedNetworkAddresses"].([]interface{})

	faPresentArgs.AInFaModel = inventory.ArgsBufProperty{Buf: adaptorDetails["Model"]}
	faPresentArgs.AInFaVendor = inventory.ArgsBufProperty{Buf: adaptorDetails["Manufacturer"]}
	faPresentArgs.AInFaSerial = inventory.ArgsBufProperty{Buf: adaptorDetails["SerialNumber"]}

	port, err := inventory.GeneratePortDetails(portStr, faPresentArgs.AInFaSide, "")
	if err != nil {
		logger.Error("failed generate Intf", zap.Error(err))
		return nil, err
	}

	faPresentArgs.AInSwitchIntf = []*inventory.ArgsIntf{port}
	neighborPresentArgs.AInIntf = []*inventory.ArgsIntf{port}
	faPresentArgs.AInFaUplinkMac = make(map[string][]interface{})
	for _, octet := range strings.Split(portNetAddr[0].(string), ":") {
		octetInt, err := strconv.ParseInt(octet, 16, 0)
		if err != nil {
			logger.Error("failed mac octet into hex", zap.Error(err))
			return nil, err
		}
		faPresentArgs.AInFaUplinkMac["mac_addr_octet"] = append(faPresentArgs.AInFaUplinkMac["mac_addr_octet"], octetInt)
	}
	faPresentArgs.AInFaUplinkPortId = portId

	// Not sure what slot refers to
	faPresentArgs.AInFaSlot = 3
	faPresentArgs.AdapterMode = 0

	faPresentBytes, err := json.Marshal(faPresentPayload)
	if err != nil {
		return nil, err
	}

	neighborPresentBytes, err := json.Marshal(neighborPresentPayload)
	if err != nil {
		return nil, err
	}
	return [][]byte{faPresentBytes, neighborPresentBytes}, nil
}

func (dc *deviceConnector) getChassis(config config.DeviceConfiguration) *inventory.Chassis {
	for _, i := range dc.inventory {
		item := i.(*inventory.Chassis)
		exists := reflect.DeepEqual(item.Config, config)
		if exists {
			return item
		}
	}
	chassis := inventory.GetNewChassis(config)
	dc.inventory = append(dc.inventory, chassis)
	return chassis
}

func (ce *connectionEmulator) getInventoryItemFromSerial(ctx context.Context, serial string) inventory.InventoryItem {
	for _, item := range ce.device.inventory {
		if item.GetSerial() == serial {
			return item
		}
	}
	return nil
}

func (ce *connectionEmulator) prepMuxPresencePayload(ctx context.Context, childConfig config.DeviceConfiguration, portStr string) ([][]byte, error) {
	chassis := ce.device.getChassis(childConfig)
	httpPlugin := ce.platform.Plugins["TypeHttp"].(*plugins.HttpPlugin)
	payload, err := chassis.GetMuxPresence(ctx, httpPlugin, portStr)

	if err != nil {
		return nil, err
	}
	return [][]byte{payload}, nil
}

func (ce *connectionEmulator) PrepInvEventsPayload(ctx context.Context, childConfig config.DeviceConfiguration, portStr string) ([][]byte, error) {
	ce.device.mu.Lock()
	defer ce.device.mu.Unlock()
	if childConfig.PlatformType == plugins.ChassisType {
		return ce.prepMuxPresencePayload(ctx, childConfig, portStr)
	}

	if childConfig.Hostname == "" {
		childConfig.Hostname = "dcemulator"
	}

	child := &deviceConnector{
		parentDevice: ce.device,
		config:       childConfig,
	}

	existingChild := ce.device.getChild(childConfig)

	if existingChild == nil {
		child.setupIpSerial()
		child.setupConnections(ctx)
		ce.device.children = append(ce.device.children, child)
	} else {
		child = existingChild
	}

	switch childConfig.PlatformType {
	case plugins.Imcm5PlatformType, plugins.Imcm4PlatformType:
		httpPlugin := child.connections[0].platform.Plugins["TypeHttp"].(*plugins.HttpPlugin)
		return child.connections[0].device.prepareRackEventsPayload(ctx, httpPlugin, portStr)
	}
	return [][]byte{}, nil
}

// EnableBladeDc starts all blade server device connector emulators for the given chassisSerial
func (ce *connectionEmulator) EnableBladeDc(ctx context.Context, chassisSerial string, chassisId int) error {
	logger := adlog.MustFromContext(ctx)
	invItem := ce.getInventoryItemFromSerial(ctx, chassisSerial)
	chassis := invItem.(*inventory.Chassis)
	if chassis == nil {
		return fmt.Errorf("failed to find Chassis object with serial %s", chassisSerial)
	}

	chassisConfig := chassis.GetConfig()

	for i, bladeConfig := range chassisConfig.Children {
		bladeId := i + 1

		// Create DeviceConnector object for the blade and attach it to the UCSI dc
		// No event is perpared for Blade server but dc must be created before we start it
		if _, err := ce.PrepInvEventsPayload(ctx, bladeConfig, ""); err != nil {
			return err
		}

		// Set headers to properly assign blade slot
		bladeDc := ce.device.getChild(bladeConfig)
		if bladeDc.status >= emulatorStatusStarting {
			continue
		}

		bladeHttpPlugin := bladeDc.connections[0].platform.Plugins["TypeHttp"].(*plugins.HttpPlugin)
		bladeHttpPlugin.SetAdditionalHeaders(map[string]string{
			"CHASSIS_ID": fmt.Sprintf("%d", chassisId),
			"BLADE_ID":   fmt.Sprintf("%d", bladeId),
		})
		chassIdPayload, err := json.Marshal(map[string]string{"Id": strconv.Itoa(chassisId)})

		reqUrl := url.URL{
			Host:   bladeHttpPlugin.RedfishEndpoint,
			Scheme: "http",
		}
		reqUrl.Path = fmt.Sprintf("/%s/redfish/v1/Chassis/1/", bladeDc.identifier[0])

		// Change chassis id at /redfish/v1/Chassis/1 path of the Redfish mock dir
		resp, err := bladeHttpPlugin.MakeRedfishRestAPIcall(ctx, "PATCH", reqUrl.String(), bytes.NewBuffer(chassIdPayload))
		if err != nil {
			return err
		}
		resp.Body.Close()

		blade, err := bladeHttpPlugin.ResolvePath(ctx,
			fmt.Sprintf("/%s/redfish/v1/Systems/%s/", bladeDc.identifier[0], bladeDc.identifier[0]))

		if err != nil {
			logger.Error("blade redfish response error", zap.Error(err))
			return err
		}

		bladeDc.vendor = blade["Manufacturer"].(string)

		if len(bladeDc.pid) == 0 {
			bladeDc.pid = append(bladeDc.pid, blade["Model"].(string))
		}

		go ce.StartChildDc(ctx, bladeConfig)
	}

	return nil
}
