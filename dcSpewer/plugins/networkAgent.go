package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/config"
	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/storage"
	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/utils"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adconnector"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adio"
	"bitbucket-eng-sjc1.cisco.com/an/barcelona/adlog"
	"go.uber.org/zap"
)

// Complete inventory sync period defined in hours
const completeInventorySyncPeriod = 24

type NetworkAgent struct {
	// Inventory that was built based on responses from device connector
	Inventory          map[string]interface{}
	httpPlugin         *HttpPlugin
	onlineIomSerials   map[string]struct{}
	inventorySyncState map[string]bool
	inventoryJobOrder  map[int]string
	mu                 sync.Mutex
}

// Describes the data that is returned from Device Connector to Intersight
type DCResponseTrace struct {
	ObjectType string
	ActionType string
	ErrorText  string
	MessageId  string
	MoList     []interface{}
	ResultCode string
}

type DCRequestTrace struct {
	ObjectType string
	ActionType string
	MessageId  string
	MoList     []interface{}
}

func (p *NetworkAgent) Init(ctx context.Context) {
	if p.httpPlugin != nil {
		p.httpPlugin.Init(ctx)
	}
	p.inventoryJobOrder = map[int]string{}
	p.ResetInventoryStatusMaps(ctx)
	go func() {
		for {
			p.runInventorySyncResetTimer()
		}
	}()
}

type agentMessage struct {
	ActionType string
	ObjectType string
	MessageId  string
	MoList     []map[string]interface{}
}

type agentResponse struct {
	ActionType string
	ErrorText  string
	MessageId  string
	MoList     json.RawMessage
	ObjectType string
	ResultCode string
}

type macBinding struct {
	ObjectType      string
	PortMac         string
	DeviceMac       string
	SlotId          int64
	PortId          int64
	AggregatePortId int64
	SwitchId        int
	ChassisId       int64
}

func lowerCaseFirstChar(s string) string {
	a := []rune(s)
	a[0] = unicode.ToLower(a[0])
	return string(a)
}

func (p *NetworkAgent) ResetInventoryStatusMaps(ctx context.Context) {
	logger := adlog.MustFromContext(ctx)
	logger.Info("resetting inventory sync state and job order maps")
	p.inventorySyncState = map[string]bool{}
}

func (p *NetworkAgent) runInventorySyncResetTimer() {
	timer := time.NewTimer(completeInventorySyncPeriod * time.Hour)
	defer timer.Stop()
	select {
	case <-timer.C:
		for k := range p.inventorySyncState {
			p.inventorySyncState[k] = false
		}
	}
}

func (p *NetworkAgent) addJobToJobOrder(jobName string) int {
	for i, v := range p.inventoryJobOrder {
		if v == jobName {
			return i
		}
	}

	index := len(p.inventoryJobOrder)
	p.inventoryJobOrder[index] = jobName
	return index
}

// Retrieve inventory items from storage and json parse into []map[string]interface{}
func getInventoryItems(ctx context.Context, key string) ([]map[string]interface{}, error) {
	var items []map[string]interface{}
	rawItems, err := storage.GetEmulatorStorage(ctx).GetInventory(ctx, key)
	if err == nil {
		err = adio.JsonToObjectCtx(ctx, rawItems, &items)
	}
	return items, err
}

func (p *NetworkAgent) getValueFromInventory(ctx context.Context, actionType string) (string, json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	logger := adlog.MustFromContext(ctx)
	trace := utils.GetTraceFromContext(ctx)
	actionType = lowerCaseFirstChar(actionType)
	inventoryKey := actionType

	logger.Sugar().Infof("Attempting to get inventory %s", actionType)

	jobOrderIndex := p.addJobToJobOrder(actionType)
	if jobOrderIndex > 0 && !p.inventorySyncState[p.inventoryJobOrder[jobOrderIndex-1]] {
		logger.Sugar().Infof("Waiting for previous job before executing %s", actionType)
		return "", nil, nil
	}
	// Check if inventory for requested actionType was synced already
	if synched, ok := p.inventorySyncState[actionType]; ok && synched {
		logger.Sugar().Infof("inventory for action type %s for node %s has already been sent", actionType, trace.Node)
		return "", nil, nil
	}

	inv, err := storage.GetEmulatorStorage(ctx).GetInventory(ctx, inventoryKey)
	if err != nil {
		return "", nil, err
	}

	messageId := utils.GetTraceFromContext(ctx).Node
	switch actionType {
	case "portInventory", "hifPortInventory", "switchInventory", "inventoryFcPorts":
		var molist interface{}

		if actionType != "switchInventory" {
			switchInv, err := storage.GetEmulatorStorage(ctx).GetInventory(ctx, "switchInventory")
			if err != nil {
				return "", nil, err
			}
			err = adio.JsonToObjectCtx(ctx, bytes.NewReader(switchInv.Bytes()), &molist)
			if err != nil {
				return "", nil, err
			}
		} else {
			err = adio.JsonToObjectCtx(ctx, bytes.NewReader(inv.Bytes()), &molist)
			if err != nil {
				return "", nil, err
			}
		}

		switch molist := molist.(type) {
		case []interface{}:
			if len(molist) >= 1 {
				mo, ok := molist[0].(map[string]interface{})
				if !ok {
					logger.Info("mo not a map")
				} else {
					modata, ok := mo["MoData"].(map[string]interface{})
					if !ok {
						logger.Info("can't find modata")
					} else {
						serial, ok := modata["Serial"].(string)
						if !ok {
							logger.Info("can't find serial")
						} else {
							messageId = serial
						}
					}
				}
			}
		case map[string]interface{}:
			messageId = molist["Serial"].(string)
		}
	}

	p.inventorySyncState[actionType] = true
	return messageId, inv.Bytes(), nil
}

func macInList(mac string, moList []map[string]interface{}) bool {
	for _, v := range moList {

		if m := v["MoData"].(string); mac == m {
			return true
		}
	}
	return false
}

func (p *NetworkAgent) getConnectedPorts(ctx context.Context, msg *agentMessage) (string, json.RawMessage, error) {

	logger := adlog.MustFromContext(ctx)
	logger.Info("Handling GetConnectedPorts action")
	trace := utils.GetTraceFromContext(ctx)
	messageId := trace.Node
	macBindings := []map[string]interface{}{}

	portId := 1
	if strings.ToLower(trace.Node) == "b" {
		portId = 2
	}

	reqUrl := url.URL{
		Host:   p.httpPlugin.RedfishEndpoint,
		Scheme: "http",
	}

	configs := getDispatcher(ctx).GetChildConfig(ctx)
	for serial, devConfig := range configs {
		switch devConfig.PlatformType {
		case Imcm5PlatformType, Imcm4PlatformType:
			reqUrl.Path = fmt.Sprintf("/%s/redfish/v1/Chassis/1/NetworkAdapters/", serial)
			resp, err := p.httpPlugin.MakeRedfishRestAPIcall(ctx, "GET", reqUrl.String(), nil)
			if err != nil {
				logger.Error("redfish response error", zap.Error(err))
				continue
			}
			networkAdapters, err := decodeRedfishResponse(resp)
			networkMembers := networkAdapters["Members"].([]interface{})

			// Get path to MLOM adapter
			adapterPath := ""
			for _, m := range networkMembers {
				member := m.(map[string]interface{})
				rfishPath := member["@odata.id"].(string)
				if strings.Contains(rfishPath, "MLOM") {
					adapterPath = fmt.Sprintf("/%s%s", serial, rfishPath)
				}
			}
			// If no MLOM adapter get path to the first adapter in the list
			if adapterPath == "" {
				adapterPath = fmt.Sprintf("/%s%s", serial,
					networkMembers[0].(map[string]interface{})["@odata.id"].(string))
			}

			reqUrl.Path = fmt.Sprintf("%s/NetworkPorts/Port-%d", adapterPath, portId)
			resp, err = p.httpPlugin.MakeRedfishRestAPIcall(ctx, "GET", reqUrl.String(), nil)
			if err != nil {
				logger.Error("redfish response error", zap.Error(err))
				continue
			}

			port, err := decodeRedfishResponse(resp)
			if err != nil {
				logger.Error("decode port json error", zap.Error(err))
				continue
			}
			netAddresses := port["AssociatedNetworkAddresses"].([]interface{})

			reqUrl.Path = adapterPath
			resp, err = p.httpPlugin.MakeRedfishRestAPIcall(ctx, "GET", reqUrl.String(), nil)
			if err != nil {
				logger.Error("redfish response error", zap.Error(err))
				continue
			}
			adapter, err := decodeRedfishResponse(resp)
			if err != nil {
				logger.Error("decode adapter json error", zap.Error(err))
				continue
			}

			portMac := netAddresses[0].(string)
			if ok := macInList(portMac, msg.MoList); !ok {
				continue
			}

			srvrPort := strings.Split(devConfig.Ports[0], "/")
			slot, err := strconv.ParseInt(srvrPort[0], 10, 32)
			id, err := strconv.ParseInt(srvrPort[1], 10, 32)

			if err != nil {
				logger.Error("failed to convert port string to int", zap.Error(err))
				continue
			}

			adapterOem := adapter["Oem"].(map[string]interface{})["Cisco"].(map[string]interface{})
			binding := macBinding{
				SlotId:     slot,
				PortId:     id,
				SwitchId:   portId,
				DeviceMac:  strings.ToLower(adapterOem["BaseMac"].(string)),
				PortMac:    portMac,
				ObjectType: "port.MacBinding",
			}
			mo := map[string]interface{}{
				"ActionType": "",
				"MoData":     binding,
			}
			macBindings = append(macBindings, mo)
		}
	}

	if len(macBindings) > 0 {
		portsRes := adio.ObjectToJson(ctx, macBindings, adio.JsonOptSerializeAllNoIndent)
		return messageId, portsRes, nil
	}

	return messageId, nil, nil
}

func (p *NetworkAgent) getPort(portId int, slotId int, ports []map[string]interface{}) (int, map[string]interface{}) {
	for i, port := range ports {
		modata := port["MoData"].(map[string]interface{})
		var id int
		var slot int

		switch modata["PortId"].(type) {

		case int:
			id = modata["PortId"].(int)
		case float64:
			id = int(modata["PortId"].(float64))
		case int64:
			id = int(modata["PortId"].(int64))
		}

		switch modata["SlotId"].(type) {
		case int:
			slot = modata["SlotId"].(int)
		case float64:
			slot = int(modata["SlotId"].(float64))
		case int64:
			slot = int(modata["SlotId"].(int64))
		}

		if id == portId && slot == slotId {
			return i, port
		}
	}
	return 0, nil
}

func (p *NetworkAgent) getChannel(portchannelId int, channels []map[string]interface{}) (int, map[string]interface{}) {

	for i, channel := range channels {
		modata := channel["MoData"].(map[string]interface{})

		stringid := strconv.Itoa(portchannelId)
		if modata["PortChannelId"] == stringid {
			return i, channel
		}
	}
	return 0, nil

}

func (p *NetworkAgent) createFcPortChannel(ctx context.Context, modata map[string]interface{}) map[string]interface{} {

	/*
		create port channel for FC PC and FCoE pC
	*/
	portchannel := make(map[string]interface{}, 0)
	res := strings.Split(modata["ObjectType"].(string), ".")
	if res[1] == "FcPortChannel" {
		portchannel["ActionType"] = "ConfigureFcUplinks"
	} else {
		return nil
	}

	portchannel["MoData"] = make(map[string]interface{}, 0)
	trace := utils.GetTraceFromContext(ctx)
	content := make(map[string]interface{}, 0)
	content["Moid"] = modata["Moid"].(string)
	portchannel["ObjectType"] = "fc.PortChannel"
	content["ClassId"] = modata["ClassId"].(string)
	content["CreateTime"] = modata["CreateTime"].(string)
	content["ModTime"] = modata["ModTime"].(string)
	content["SharedScope"] = modata["SharedScope"].(string)
	content["AccountMoid"] = modata["AccountMoid"].(string)
	content["DomainGroupMoid"] = modata["DomainGroupMoid"].(string)
	content["DeviceMoId"] = trace.DeviceMoid
	if val, ok := modata["Dn"]; ok {
		content["Dn"] = val.(string)
	} else {
		content["Dn"] = ""
	}
	content["Rn"] = ""

	if val, ok := modata["AdminState"]; ok {
		content["AdminState"] = val.(string)
	} else {
		content["AdminState"] = "Enabled"
	}

	if val, ok := modata["OperSpeed"]; ok {
		content["OperSpeed"] = val.(string)
	} else {
		content["OperSpeed"] = "8Gbps"
	}
	content["OperState"] = "up"
	if val, ok := modata["OperStateQual"]; ok {
		content["OperStateQual"] = val.(string)
	} else {
		content["OperStateQual"] = "port-channel-members-down"
	}
	content["PortChannelId"] = fmt.Sprint(modata["PcId"])

	if val, ok := modata["AdminSpeed"]; ok {
		content["AdminSpeed"] = val.(string)
	} else {
		content["AdminSpeed"] = "8Gps"
	}

	if val, ok := modata["VsanId"]; ok {
		content["Vsan"] = val
	} else {
		content["Vsan"] = 0
	}

	if val, ok := modata["Mode"]; ok {
		content["Mode"] = val.(string)
	} else {
		content["Mode"] = "N-proxy"
	}

	content["Role"] = "FC Uplink Port Channel Member"
	content["SwitchId"] = fmt.Sprint(modata["SwitchId"])
	portchannel["MoData"] = content
	return portchannel

}

func sendInventoryEventPerChild(ctx context.Context,
	dcConfig *config.DeviceConfiguration, enabledPorts map[string]bool) {
	logger := adlog.MustFromContext(ctx)

	if dcConfig.Children != nil {
		for _, child := range dcConfig.Children {
			for _, portStr := range child.Ports {
				if enabled, ok := enabledPorts[portStr]; ok && enabled {
					events, err := getDispatcher(ctx).PrepInvEventsPayload(ctx, child, portStr)
					if err != nil {
						logger.Error("Failed to get events payload", zap.Error(err))
						continue
					}
					for _, event := range events {
						sendEvent(ctx, event)
					}
				}
			}
		}
	}
}

func (p *NetworkAgent) createPortChannel(ctx context.Context, modata map[string]interface{}, pctype string) map[string]interface{} {
	// only create ethernet port channels
	portchannel := make(map[string]interface{}, 0)
	if pctype == "fcoe" {
		portchannel["ActionType"] = "ConfigureFcoeUplinks"
	} else {
		portchannel["ActionType"] = "ConfigureEtherUplinks"
	}
	trace := utils.GetTraceFromContext(ctx)
	content := make(map[string]interface{}, 0)
	portchannel["ObjectType"] = "ether.PortChannel"
	content["ClassId"] = modata["ClassId"].(string)
	content["CreateTime"] = modata["CreateTime"].(string)
	content["ModTime"] = modata["ModTime"].(string)
	content["SharedScope"] = modata["SharedScope"].(string)
	content["AccountMoid"] = modata["AccountMoid"].(string)
	content["DomainGroupMoid"] = modata["DomainGroupMoid"].(string)
	content["DeviceMoId"] = trace.DeviceMoid
	if val, ok := modata["Dn"]; ok {
		content["Dn"] = val.(string)
	} else {
		content["Dn"] = ""
	}
	content["Rn"] = ""
	if val, ok := modata["AdminState"]; ok {
		content["AdminState"] = val.(string)
	} else {
		content["AdminState"] = "Enabled"
	}

	if val, ok := modata["OperSpeed"]; ok {
		content["OperSpeed"] = val.(string)
	} else {
		content["OperSpeed"] = "8Gbps"
	}
	content["OperState"] = "up"
	if val, ok := modata["OperStateQual"]; ok {
		content["OperStateQual"] = val.(string)
	} else {
		content["OperStateQual"] = "port-channel-members-down"
	}
	content["PortChannelId"] = fmt.Sprint(modata["PcId"])

	if val, ok := modata["NativeVlan"]; ok {
		content["NativeVlan"] = fmt.Sprint(val)
	} else {
		content["NativeVlan"] = ""
	}
	if val, ok := modata["AllowedVlans"]; ok {
		content["AllowedVlans"] = val.(string)
	} else {
		content["AllowedVlans"] = ""
	}
	if val, ok := modata["AccessVlan"]; ok {
		content["AccessVlan"] = fmt.Sprint(val)
	} else {
		content["AccessVlan"] = ""
	}
	if val, ok := modata["Mode"]; ok {
		content["Mode"] = val.(string)
	} else {
		content["Mode"] = "trunk"
	}
	if modata["Description"].(string) == "" {
		if pctype == "fcoe" {
			content["Role"] = "FcoeUplink"
		} else {
			content["Role"] = "Uplink"
		}
	} else {
		content["Role"] = modata["Description"].(string)
	}
	content["SwitchId"] = fmt.Sprint(modata["SwitchId"])
	portchannel["MoData"] = content
	return portchannel

}

func (p *NetworkAgent) configurePortChannel(ctx context.Context, node string,
	dcConfig *config.DeviceConfiguration, msg *agentMessage) error {

	p.mu.Lock()
	defer p.mu.Unlock()

	channels, err := getInventoryItems(ctx, "portChannelInventory")
	if err != nil {
		return nil
	}
	pctype := ""
	if msg.ActionType == "ConfigureFcoeUplinks" {
		pctype = "fcoe"
	} else {
		pctype = "ether"
	}

	for _, mo := range msg.MoList {
		modata := mo["MoData"].(map[string]interface{})
		if strings.Contains(modata["ObjectType"].(string), "PortChannel") {
			id := int((modata["PcId"]).(float64))
			_, channel := p.getChannel(id, channels)
			if channel == nil {
				var portchannel map[string]interface{}

				portchannel = p.createPortChannel(ctx, modata, pctype)
				channels = append(channels, portchannel)
			} else {
				pcdata := channel["MoData"].(map[string]interface{})
				if pctype == "ether" {
					pcdata["Role"] = "Ethernet Uplink Port Channel Member"
				} else if pctype == "fcoe" {
					pcdata["Role"] = "FCoE Uplink Port Channel Member"
				}
			}
		}
	}

	// Write the port channels back to the inventory
	channelsRes := adio.ObjectToJson(ctx, channels, adio.JsonOptSerializeAllNoIndent)
	adlog.MustFromContext(ctx).Sugar().Infof("writing back portChannelInventory : %s", string(channelsRes))
	err = storage.GetEmulatorStorage(ctx).WriteInventory(ctx, "portChannelInventory", bytes.NewBuffer(channelsRes))
	p.inventorySyncState["portChannelInventory"] = false
	return err
}

func (p *NetworkAgent) configureFcoeUplinks(ctx context.Context, node string,
	dcConfig *config.DeviceConfiguration, msg *agentMessage) error {

	// this action type has both portchannel objects and EtherPorts
	trace := utils.GetTraceFromContext(ctx)

	if err := p.configurePorts(ctx, trace.Node, trace.Config, msg); err != nil {
		return err
	}
	if err := p.configurePortChannel(ctx, trace.Node, trace.Config, msg); err != nil {
		return err
	}

	return nil
}

func (p *NetworkAgent) configureFcPorts(ctx context.Context, node string,
	dcConfig *config.DeviceConfiguration, msg *agentMessage) error {
	/*
		Configure FC ports only
	*/

	fcports, err := getInventoryItems(ctx, "inventoryFcPorts")
	if err != nil {
		return err
	}

	if fcports == nil {
		return fmt.Errorf("inventoryFcPorts not found")
	}

	for _, mo := range msg.MoList {
		modata := mo["MoData"].(map[string]interface{})
		if modata["ConfigState"] == "unconfigured" {
			continue
		}
		if strings.Contains(modata["ObjectType"].(string), "PortChannel") {
			continue
		}
		id := int(modata["PortId"].(float64))
		slot := int(modata["SlotId"].(float64))
		_, port := p.getPort(id, slot, fcports)
		if port == nil {
			continue
		}

		portMoData := port["MoData"].(map[string]interface{})

		if portMoData["ObjectType"] == "ether.PhysicalPort" {
			return fmt.Errorf("%d/%d port object type is ether.PhysicalPort. It should be fc.PhysicalPort", slot, id)
		}

		portMoData["PortChannelId"] = modata["PcId"]
		portMoData["AdminState"] = "Enabled"
		portMoData["OperStateQual"] = "none"
		portMoData["Role"] = "FC Uplink"
		portMoData["OperState"] = "up"
		portMoData["Mode"] = "N-proxy"

	}

	// Write the ports back to the inventory
	portsRes := adio.ObjectToJson(ctx, fcports, adio.JsonOptSerializeAllNoIndent)

	adlog.MustFromContext(ctx).Sugar().Infof("writing back portInventory : %s", string(portsRes))
	err = storage.GetEmulatorStorage(ctx).WriteInventory(ctx, "inventoryFcPorts", bytes.NewBuffer(portsRes))
	p.inventorySyncState["inventoryFcPorts"] = false
	p.inventorySyncState["hifPortInventory"] = false

	return err

}

func (p *NetworkAgent) configureFcUplinks(ctx context.Context, node string,
	dcConfig *config.DeviceConfiguration, msg *agentMessage) error {

	for _, mo := range msg.MoList {
		modata := mo["MoData"].(map[string]interface{})
		if strings.Contains(modata["ObjectType"].(string), "FcPort") &&
			!strings.Contains(modata["ObjectType"].(string), "PortChannel") {
			if err := p.configureFcPorts(ctx, node, dcConfig, msg); err != nil {
				return err
			}
			break
		}
	}
	for _, mo := range msg.MoList {
		modata := mo["MoData"].(map[string]interface{})
		if strings.Contains(modata["ObjectType"].(string), "PortChannel") {
			if err := p.configureFcPc(ctx, node, dcConfig, msg); err != nil {
				return err
			}
			break
		}
	}
	return nil
}

func (p *NetworkAgent) getfcPortChannelInventory(ctx context.Context) ([]map[string]interface{}, error) {
	var channels []map[string]interface{}
	rawchannels, err := storage.GetEmulatorStorage(ctx).GetInventory(ctx, "inventoryFcPortChannels")
	if err == nil {
		err = adio.JsonToObjectCtx(ctx, rawchannels, &channels)
	}
	return channels, err
}

func (p *NetworkAgent) configureFcPc(ctx context.Context, node string,
	dcConfig *config.DeviceConfiguration, msg *agentMessage) error {

	channels, err := p.getfcPortChannelInventory(ctx)
	if err != nil {
		return nil
	}

	for _, mo := range msg.MoList {
		modata := mo["Modata"].(map[string]interface{})

		if strings.Contains(modata["ObjectType"].(string), "PortChannel") {

			id := int((modata["PcId"]).(float64))
			index, channel := p.getChannel(id, channels)

			if modata["ObjectType"].(string) == "unconfigured" {

				removeInventoryItem(channels, index)
				continue
			}
			if channel == nil {
				fcchannel := make(map[string]interface{}, 0)
				fcchannel = p.createFcPortChannel(ctx, modata)
				channels = append(channels, fcchannel)
			}
		}
	}
	channelsRes := adio.ObjectToJson(ctx, channels, adio.JsonOptSerializeAllNoIndent)
	adlog.MustFromContext(ctx).Sugar().Infof("writing back FC portChannelInventory : %s", string(channelsRes))
	err = storage.GetEmulatorStorage(ctx).WriteInventory(ctx, "inventoryFcPortChannels", bytes.NewBuffer(channelsRes))
	p.inventorySyncState["inventoryFcPortChannels"] = false
	return nil
}

func removeInventoryItem(items []map[string]interface{}, index int) []map[string]interface{} {

	items[index] = items[len(items)-1]
	items[len(items)-1] = nil
	items = items[:len(items)-1]
	return items
}

func (p *NetworkAgent) configureUnifiedports(ctx context.Context, node string,
	dcConfig *config.DeviceConfiguration, msg *agentMessage) error {

	trace := utils.GetTraceFromContext(ctx)
	fcportsfromstorage, _ := getInventoryItems(ctx, "inventoryFcPorts")

	ports, err := getInventoryItems(ctx, "portInventory")
	if err != nil {
		return err
	}

	for _, mo := range msg.MoList {
		modata := mo["MoData"].(map[string]interface{})
		id := int(modata["PortId"].(float64))
		slot := int(modata["SlotId"].(float64))
		portIndex, port := p.getPort(id, slot, ports)
		fcPortIndex, fcport := p.getPort(id, slot, fcportsfromstorage)

		finalport := make(map[string]interface{}, 0)
		porttype := "fc"
		if port == nil && fcport == nil {
			return fmt.Errorf("%d/%d port not found", slot, id)
		}
		if port == nil {
			finalport = fcport
			portIndex = fcPortIndex
		} else {
			finalport = port
			porttype = "ether"
		}
		portMoData := finalport["MoData"].(map[string]interface{})
		portState := modata["ConfigState"].(string)
		switch portState {
		case "configured", "unconfigured":
			createFcPort(portMoData, modata, trace.DeviceMoid, portState)
			finalport["MoData"] = portMoData
			if porttype == "ether" {
				// delete this port from fc port storage
				// add this port to port inventory
				ports = removeInventoryItem(ports, portIndex)
				fcportsfromstorage = append(fcportsfromstorage, finalport)
			}
		case "deleted":
			createetherPort(portMoData, modata, trace.DeviceMoid)
			finalport["MoData"] = portMoData
			if porttype == "fc" {
				fcportsfromstorage = removeInventoryItem(fcportsfromstorage, portIndex)
				ports = append(ports, finalport)
			}
		}
	}

	// Write the ports back to the inventory
	fcPortsRes := adio.ObjectToJson(ctx, fcportsfromstorage, adio.JsonOptSerializeAllNoIndent)
	ethPortsRes := adio.ObjectToJson(ctx, ports, adio.JsonOptSerializeAllNoIndent)
	adlog.MustFromContext(ctx).Sugar().Infof("writing back inventoryFcPorts : %s", string(fcPortsRes))
	err = storage.GetEmulatorStorage(ctx).WriteInventory(ctx, "inventoryFcPorts", bytes.NewBuffer(fcPortsRes))
	adlog.MustFromContext(ctx).Sugar().Infof("writing back portInventory : %s", string(ethPortsRes))
	err = storage.GetEmulatorStorage(ctx).WriteInventory(ctx, "portInventory", bytes.NewBuffer(ethPortsRes))
	// InventoryFcPorts
	p.inventorySyncState["portInventory"] = false
	p.inventorySyncState["inventoryFcPorts"] = false

	return err
}

func createetherPort(portmodata map[string]interface{}, modata map[string]interface{}, devicemoid string) {

	// generate an unconfigured ether port because config state is deleted
	// action type is configure unified port request
	portmodata["AdminSpeed"] = ""
	portmodata["AdminState"] = "Disabled"
	portmodata["ClassId"] = "ether.PhysicalPort"
	portmodata["Dn"] = ""
	portmodata["Mode"] = "access"
	portmodata["ObjectType"] = "ether.PhysicalPort"
	portmodata["OperSpeed"] = "auto"
	portmodata["OperState"] = "down"
	portmodata["OperStateQual"] = "admin-down"
	portmodata["PeerDn"] = ""
	portmodata["PortChannelId"] = 0
	portmodata["PortId"] = modata["PortId"]
	portmodata["Rn"] = ""
	portmodata["Role"] = "unknown"
	portmodata["SlotId"] = modata["SlotId"]
	portmodata["SwitchId"] = modata["SwitchId"]
	portmodata["TransceiverType"] = "absent"

}

func createFcPort(portmodata map[string]interface{}, modata map[string]interface{},
	devicemoid string, state string) {
	// config state of this modata is configured,
	// make that port to be unconfigured ether port

	portmodata["ObjectType"] = "fc.PhysicalPort"
	portmodata["ClassId"] = "fc.PhysicalPort"
	intslot := int64(modata["SlotId"].(float64))
	intport := int64(modata["PortId"].(float64))
	slotid := strconv.FormatInt(intslot, 10)
	portid := strconv.FormatInt(intport, 10)
	portmodata["Dn"] = "sys/switch-B/slot-" + slotid +
		"/switch-fc/port-" + portid

	portmodata["Rn"] = ""
	if state == "configured" {
		portmodata["OperState"] = "up"
		portmodata["OperStateQual"] = ""
		portmodata["Role"] = "FcUplink PC Member"
		portmodata["AdminState"] = "Enabled"
		portmodata["MaxSpeed"] = modata["AdminSpeed"].(string)
		portmodata["OperSpeed"] = "8Gbps"
		portmodata["PortChannelId"] = int(modata["PcId"].(float64))
	} else {
		portmodata["OperState"] = "down"
		portmodata["OperStateQual"] = "SFP not present"
		portmodata["Role"] = "Unconfigured"
		portmodata["AdminState"] = "Disabled"
		portmodata["MaxSpeed"] = "unknown"
		portmodata["OperSpeed"] = "auto"
		portmodata["PortChannelId"] = 0
	}
	portmodata["PortId"] = modata["PortId"]
	portmodata["SlotId"] = modata["SlotId"]
	if int(modata["SwitchId"].(float64)) == 1 {
		portmodata["SwitchId"] = "A"
	} else {
		portmodata["SwitchId"] = "B"
	}

	portmodata["AdminSpeed"] = modata["AdminSpeed"]
	portmodata["B2bCredit"] = 0
	portmodata["Mode"] = "N-proxy"
	portmodata["PeerDn"] = ""
	portmodata["TransceiverType"] = ""
	portmodata["Vsan"] = modata["VsanId"]
	portmodata["Wwn"] = ""

}

func (p *NetworkAgent) configurePorts(ctx context.Context, node string,
	dcConfig *config.DeviceConfiguration, msg *agentMessage) error {

	enabledPorts := map[string]bool{}

	ports, err := getInventoryItems(ctx, "portInventory")
	if err != nil {
		return err
	}

	for _, mo := range msg.MoList {
		modata := mo["MoData"].(map[string]interface{})
		if modata["ConfigState"] == "unconfigured" {
			continue
		}
		if strings.Contains(modata["ObjectType"].(string), "PortChannel") {
			continue
		}

		id := int(modata["PortId"].(float64))
		slot := int(modata["SlotId"].(float64))
		descr := modata["Description"]
		_, port := p.getPort(id, slot, ports)
		if port == nil {
			return fmt.Errorf("%d/%d port not found", slot, id)
		}

		portMoData := port["MoData"].(map[string]interface{})

		enabledKey := fmt.Sprintf("%d/%d", slot, id)

		switch descr {
		case "FcoeUplink PC Member":
			portMoData["PortChannelId"] = modata["PcId"]
			portMoData["AdminState"] = "Enabled"
			portMoData["OperStateQual"] = "none"
			portMoData["Role"] = "FCoE Uplink"
			portMoData["OperState"] = "up"
			portMoData["Mode"] = "trunk"
		case "FcoeUplink":
			portMoData["AdminState"] = "Enabled"
			portMoData["OperStateQual"] = "none"
			portMoData["Role"] = "FCoE Uplink"
			portMoData["OperState"] = "up"
			portMoData["Mode"] = "trunk"
		case "Uplink PC Member":
			// this is ethernet port
			portMoData["PortChannelId"] = modata["PcId"]
			portMoData["AdminState"] = "Enabled"
			portMoData["OperStateQual"] = "none"
			portMoData["Role"] = "Uplink"
			portMoData["OperState"] = "up"
			portMoData["Mode"] = "trunk"
		case "Server":
			enabledPorts[enabledKey] = true
			portMoData["AdminState"] = "Enabled"
			portMoData["OperStateQual"] = "none"
			portMoData["Role"] = "Server"
			portMoData["OperState"] = "up"
			portMoData["Mode"] = "fex-fabric"
		default:
			enabledPorts[enabledKey] = false
		}
	}

	go sendInventoryEventPerChild(ctx, dcConfig, enabledPorts)

	// Write the ports back to the inventory
	portsRes := adio.ObjectToJson(ctx, ports, adio.JsonOptSerializeAllNoIndent)

	adlog.MustFromContext(ctx).Sugar().Infof("writing back portInventory : %s", string(portsRes))
	err = storage.GetEmulatorStorage(ctx).WriteInventory(ctx, "portInventory", bytes.NewBuffer(portsRes))
	p.inventorySyncState["portInventory"] = false

	return err
}

func (p *NetworkAgent) configRackPort(ctx context.Context, node string, dcConfig *config.DeviceConfiguration, msg *agentMessage) error {
	logger := adlog.MustFromContext(ctx)
	logger.Info("Handling ConfigRackPort action")

	var portId, slotId int = 0, 0

	for _, mo := range msg.MoList {
		if mo["ObjectType"].(string) == "sw.EtherPort" {
			moData := mo["MoData"].(map[string]interface{})
			portId = int(moData["PortId"].(float64))
			slotId = int(moData["SlotId"].(float64))
		}
	}
	if portId == 0 || slotId == 0 {
		return fmt.Errorf("Failed to identify port during configRackPort action")
	}

	configuredPort := fmt.Sprintf("%d/%d", slotId, portId)
	if dcConfig.Children != nil {
		for _, child := range dcConfig.Children {
			for _, portStr := range child.Ports {
				if portStr == configuredPort {
					logger.Info("Starting child rack server")
					getDispatcher(ctx).StartChildDc(ctx, child)
				}
			}
		}
	}

	time.Sleep(10 * time.Second)
	return nil
}

func (p *NetworkAgent) sendMuxOnline(ctx context.Context, node string, msg *agentMessage) error {
	var iomSerial string
	var chassisId, moduleId int
	logger := adlog.MustFromContext(ctx)
	logger.Info("preparing MuxOnline event")
	moData := msg.MoList[0]["MoData"].(map[string]interface{})

	if iomSerialInterface, ok := moData["ModuleSerial"]; ok {
		iomSerial = iomSerialInterface.(string)
		chassisId = int(moData["ChassisId"].(float64))
		moduleId = int(moData["ModuleId"].(float64))

	} else {
		iomSerial = moData["PeerModuleSerial"].(string)
		chassisId = int(moData["PeerEquipmentId"].(float64))
		moduleId = int(moData["PeerModuleId"].(float64))
	}

	if _, ok := p.onlineIomSerials[iomSerial]; ok {
		logger.Sugar().Infof("iom %s is already online")
		return nil
	}

	muxOnlinePayload := map[string]interface{}{
		"api":  "SCI_muxOnlineCb",
		"type": "REQUEST",
	}
	args := map[string]interface{}{
		"aInChId":     chassisId,
		"aInModuleId": moduleId,
	}

	muxOnlinePayload["args"] = args

	payload, err := json.Marshal(muxOnlinePayload)
	if err != nil {
		return fmt.Errorf("failed to marshal muxOnline payload")
	}

	if chassisSerial, ok := p.httpPlugin.IomToChassisMap[iomSerial]; ok {
		p.httpPlugin.AddItemToChassisSerial(chassisId, chassisSerial)
	} else {
		p.httpPlugin.AddItemToChassisSerial(chassisId, moData["PeerEquipmentSerial"].(string))
	}
	// 10 second delay before sending muxOnline
	time.Sleep(10 * time.Second)
	logger.Info("Sending MuxOnline")
	sendEvent(ctx, payload)
	// Setting value to emtry struct as it is ignored.
	// the map is only used to quickly lookup if key exists
	if p.onlineIomSerials == nil {
		p.onlineIomSerials = make(map[string]struct{})
	}
	p.onlineIomSerials[iomSerial] = struct{}{}
	return nil
}

func (p *NetworkAgent) enableBladeServerDCs(ctx context.Context, node string, msg *agentMessage) error {
	var chassisId int
	moData := msg.MoList[0]["MoData"].(map[string]interface{})
	if fexId, ok := moData["FexId"]; ok {
		chassisId = int(fexId.(float64))
	} else {
		chassisId = int(moData["ChassisId"].(float64))
	}

	if chassisSerial, ok := p.httpPlugin.ChassisSerials[chassisId]; ok {
		err := getDispatcher(ctx).EnableBladeDc(ctx, chassisSerial, chassisId)
		if err != nil {
			return err
		}
	} else {
		return fmt.Errorf("failed to get chassis serial from HttpPlugin")
	}
	return nil
}

func (p *NetworkAgent) Delegate(ctx context.Context, in adconnector.Body) (io.Reader, error) {
	logger := adlog.MustFromContext(ctx)
	logger.Sugar().Infof("network agent got: %s", string(in))

	msg := &agentMessage{}
	err := adio.JsonToObjectCtx(ctx, bytes.NewReader(in), msg)
	trace := utils.GetTraceFromContext(ctx)

	if err != nil {
		return nil, err
	}

	var messageID string = trace.Node
	var moList json.RawMessage = nil
	actionType := ""

	switch msg.ActionType {
	case "ConfigurePortChannel":
		if err := p.configurePortChannel(ctx, trace.Node, trace.Config, msg); err != nil {
			logger.Error("Failed to configure ports", zap.Error(err))
			return nil, err
		}
		logger.Sugar().Infof("Changed port channel configuration")
	case "ConfigureFcUplinks":
		if err := p.configureFcUplinks(ctx, trace.Node, trace.Config, msg); err != nil {
			logger.Error("Failed to configure ports", zap.Error(err))
			return nil, err
		}
		logger.Sugar().Infof("Changed FC Uplinks configuration")
	case "PortConfigs":
		if err := p.configurePorts(ctx, trace.Node, trace.Config, msg); err != nil {
			logger.Error("Failed to configure ports", zap.Error(err))
			return nil, err
		}
		logger.Sugar().Infof("Changed port configuration")
	case "ConfigureUnifiedPorts":
		if err := p.configureUnifiedports(ctx, trace.Node, trace.Config, msg); err != nil {
			logger.Error("Failed to configure unified ports", zap.Error(err))
			return nil, err
		}
		logger.Sugar().Infof("Changed unified port configuration")
	case "ConfigureFcoeUplinks":
		if err := p.configureFcoeUplinks(ctx, trace.Node, trace.Config, msg); err != nil {
			logger.Error("Failed to configure ", zap.Error(err))
			return nil, err
		}
		logger.Sugar().Infof("Changed FCoe uplink port channel configuration")
	case "ConfigRackPort":
		if err := p.configRackPort(ctx, trace.Node, trace.Config, msg); err != nil {
			logger.Error("Failed to process ConfigRackPort action", zap.Error(err))
			return nil, err
		}
	case "CONFIGURE_FABRIC_PC", "ConfigureFabricPc":
		if err := p.sendMuxOnline(ctx, trace.Node, msg); err != nil {
			return nil, err
		}
		return nil, nil
	case "CONFIGURE_CMS_PORT", "ConfigureCmsPort":
		if err := p.enableBladeServerDCs(ctx, trace.Node, msg); err != nil {
			return nil, err
		}
		return nil, nil
	case "GetConnectedPorts":
		messageID, moList, err = p.getConnectedPorts(ctx, msg)
		logger.Sugar().Infof("connected ports: %s", moList)
	default:
		if strings.Contains(strings.ToLower(msg.ActionType), "inventory") || msg.ActionType == "SwSystemStats" {
			messageID, moList, err = p.getValueFromInventory(ctx, msg.ActionType)
			actionType = "inventory"
			if moList == nil {
				logger.Info("no inventory to send")
				return nil, err
			}
			logger.Sugar().Infof("Returning Inventory from NetworkAgent")
		}
	}

	if err == nil {
		resp := &agentResponse{
			ActionType: actionType,
			MessageId:  messageID,
			MoList:     moList,
			ObjectType: "connector.NetworkAgentResponse",
			ResultCode: "Success"}
		respBytes, err := json.Marshal(resp)

		logger.Sugar().Infof("ina respoonse : %s", respBytes)
		return bytes.NewBuffer(respBytes), err
	}

	logger.Sugar().Infof("Failed to process action %s", msg.ActionType)
	return nil, err
}

type networkAgentEvent struct {
	SwitchId int
	Type     string
	Payload  []byte
}

func sendEvent(ctx context.Context, payload []byte) {
	logger := adlog.MustFromContext(ctx)
	trace := utils.GetTraceFromContext(ctx)

	switchId := 1
	if strings.ToLower(trace.Node) == "b" {
		switchId = 2
	}

	if payload == nil {
		logger.Debug("Empty payload. Skipping sendEvent")
		return
	}
	eventType := "SAM_PROXY_EVENT"

	scievent := &eventOut{
		Payload: adio.ObjectToJson(ctx, &networkAgentEvent{
			SwitchId: switchId,
			Payload:  payload,
			Type:     eventType,
		}, nil),
	}

	outbuf := new(bytes.Buffer)
	enc := json.NewEncoder(outbuf)
	err := enc.Encode(scievent)
	if err != nil {
		logger.Error("failed to encode network agent event message", zap.Error(err))
		return
	}

	eventMsg, err := utils.GetMessageReader(ctx, adconnector.Header{
		Type:      "TypeEvtChannelMsg",
		SessionId: "AD.streamingevent.gobi",
	}, outbuf)
	if err != nil {
		logger.Error("failed to create network agent event message", zap.Error(err))
		return
	}
	getDispatcher(ctx).Dispatch(ctx, eventMsg)
}
