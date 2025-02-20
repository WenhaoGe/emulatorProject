package inventory

import (
	"strconv"
	"strings"

	"bitbucket-eng-sjc1.cisco.com/an/apollo-test/dcSpewer/config"
)

type InventoryItem interface {
	Init(config config.DeviceConfiguration)
	GetSerial() string
	GetConfig() config.DeviceConfiguration
}

/*
func GetInvItemFromConfig(inv []*InventoryItem, config config.DeviceConfiguration) *InventoryItem {
	for _, item := range inv {
		exists := reflect.DeepEqual(item.Config(), config)
		if exists {
			return item
		}
	}
	return nil
}
*/

// EventPayload - payload for inventory discovery events
type EventPayload struct {
	Type string      `json:"type"`
	API  string      `json:"api"`
	Args interface{} `json:"args"`
}

// FaPresentPayloadArgs - args sent in rack inventory dicsovery event payload
type FaPresentPayloadArgs struct {
	AInChVendor       ArgsBufProperty          `json:"aInChVendor"`
	AInFaModel        ArgsBufProperty          `json:"aInFaModel"`
	AInFaSlot         int                      `json:"aInFaSlot"`
	AdapterMode       int                      `json:"adapterMode"`
	AInSwitchIntf     []*ArgsIntf              `json:"aInSwitchIntf"`
	AInFaSerial       ArgsBufProperty          `json:"aInFaSerial"`
	AInChSerial       ArgsBufProperty          `json:"aInChSerial"`
	AInFaSide         int                      `json:"aInFaSide"`
	AInChModel        ArgsBufProperty          `json:"aInChModel"`
	AInFaUplinkMac    map[string][]interface{} `json:"aInFaUplinkMac"`
	AInFaVendor       ArgsBufProperty          `json:"aInFaVendor"`
	AInFaUplinkPortId int                      `json:"aInFaUplinkPortId"`
}

// NeighborPresentPayloadArgs - args sent by neighborPresent event
type NeighborPresentPayloadArgs struct {
	AInIntf []*ArgsIntf `json:"aInIntf"`
}

// MuxPresentPayloadArgs - args sent by muxPresent event
type MuxPresentPayloadArgs struct {
	AInChVendor     ArgsBufProperty `json:"aInChVendor"`
	AInChSerial     ArgsBufProperty `json:"aInChSerial"`
	AInMuxModel     ArgsBufProperty `json:"aInMuxModel"`
	AInMuxVendor    ArgsBufProperty `json:"aInMuxVendor"`
	AInMgmtInstance ArgsBufProperty `json:"aInMgmtInstance"`
	AInSwitchIntf   []*ArgsIntf     `json:"aInSwitchIntf"`
	AInModuleNum    int             `json:"aInModuleNum"`
	AInMuxSerial    ArgsBufProperty `json:"aInMuxSerial"`
	AInChModel      ArgsBufProperty `json:"aInChModel"`
	AInMuxIntf      []*ArgsIntf     `json:"aInMuxIntf"`
}

type ArgsBufProperty struct {
	Buf interface{} `json:"buf"`
}

type ArgsIntf struct {
	Slot         int                    `json:"slot"`
	PortType     int                    `json:"portType"`
	BreakoutPort map[string]interface{} `json:"breakoutPort"`
	Chassis      int                    `json:"chassis"`
}

func GeneratePortDetails(fiPort string, fiSide int, iomModel string) (*ArgsIntf, error) {
	confPort := strings.Split(fiPort, "/")

	intf := &ArgsIntf{
		PortType: 1,
		Chassis:  0,
	}

	slot, err := strconv.Atoi(confPort[0])
	if err != nil {
		return nil, err
	}

	port, err := strconv.Atoi(confPort[1])
	if err != nil {
		return nil, err
	}

	if iomModel != "" {
		iomPortCount := 4
		if strings.Contains(iomModel, "2208") {
			iomPortCount = 8
		}
		slot = fiSide
		port = GetIOMportNumber(port, iomPortCount)
		intf.PortType = 5
	}
	intf.BreakoutPort = make(map[string]interface{})
	intf.BreakoutPort["aggrPort"] = 0
	intf.BreakoutPort["port"] = port
	intf.Slot = slot

	return intf, nil
}
